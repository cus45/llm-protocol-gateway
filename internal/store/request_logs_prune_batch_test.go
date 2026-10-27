package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// 剪枝必须分批改写。一条 DELETE 清掉整表过期行会长时间独占唯一的写连接，
// 关停时的 flush 排在它后面就拿不到超时预算之外的任何东西：systemd 到
// TimeoutStopSec 直接 SIGKILL，事务整段回滚，下次启动再做一遍同样的活——
// 表现为每次发布都卡在重启上。分批之后每条语句都很短，批次之间连接是空的。
func TestPruneRequestLogsContextDeletesExpiredRowsInBatches(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	expired := 3*pruneBatchRows + 7 // more than one pass' worth of batches
	base := time.Now().UTC().Add(-30 * 24 * time.Hour)
	for start := 0; start < expired; start += 500 {
		batch := make([]monitor.RequestLog, 0, 500)
		for i := start; i < min(start+500, expired); i++ {
			batch = append(batch, monitor.RequestLog{Time: base.Add(time.Duration(i) * time.Second), Status: 200})
		}
		if err := s.AppendRequestLogs(batch); err != nil {
			t.Fatalf("append batch %d: %v", start, err)
		}
	}
	fresh := []monitor.RequestLog{{Time: time.Now().UTC(), Status: 200}, {Time: time.Now().UTC(), Status: 500}}
	if err := s.AppendRequestLogs(fresh); err != nil {
		t.Fatal(err)
	}

	if err := s.PruneRequestLogsContext(context.Background(), 7); err != nil {
		t.Fatalf("prune: %v", err)
	}
	rows, err := s.ListRequestLogs(1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(fresh) {
		t.Fatalf("remaining rows = %d, want %d: the batched prune left expired rows behind", len(rows), len(fresh))
	}
}

// 取消关停时，剪枝要停在批次之间，把剩下的交给下一次维护，而不是当作失败。
func TestPruneRequestLogsContextStopsBetweenBatchesWhenCancelled(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	base := time.Now().UTC().Add(-30 * 24 * time.Hour)
	batch := make([]monitor.RequestLog, 0, 2*pruneBatchRows)
	for i := 0; i < 2*pruneBatchRows; i++ {
		batch = append(batch, monitor.RequestLog{Time: base.Add(time.Duration(i) * time.Second), Status: 200})
	}
	if err := s.AppendRequestLogs(batch); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.PruneRequestLogsContext(ctx, 7); err != nil {
		t.Fatalf("a cancelled pass is a deliberate stop, not a failure: %v", err)
	}
	// count(*) rather than ListRequestLogs: the log page is capped at 500 rows.
	var remaining int
	if err := s.db.QueryRow(`SELECT count(*) FROM request_logs`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != len(batch) {
		t.Fatalf("rows after a cancelled pass = %d, want %d: it deleted past the stop signal", remaining, len(batch))
	}
}

// 安全上限那条 SQL 的形状（LIMIT -1 OFFSET 再套一层 LIMIT）是这次改动里最容易
// 写错的地方：偏移量之外的行才删，且每批只取 pruneBatchRows 条。真实的 5 万行
// 上限在生产库里验证，这里在小表上验证同一条语句的语义。
func TestPruneSafetyCapStatementKeepsNewestRows(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const total, keep = 50, 20
	if _, err := s.db.Exec(`WITH RECURSIVE n(i) AS (
			SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < ?)
		INSERT INTO request_logs (time, status)
		SELECT printf('2026-01-01T00:00:%02dZ', i), 200 FROM n`, total); err != nil {
		t.Fatalf("seed rows: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM request_logs WHERE id IN (
			SELECT id FROM (
				SELECT id FROM request_logs ORDER BY time DESC, id DESC LIMIT -1 OFFSET ?
			) LIMIT ?)`, keep, pruneBatchRows); err != nil {
		t.Fatalf("safety-cap statement: %v", err)
	}

	var remaining int
	if err := s.db.QueryRow(`SELECT count(*) FROM request_logs`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != keep {
		t.Fatalf("rows after cap statement = %d, want %d", remaining, keep)
	}
	var newest string
	if err := s.db.QueryRow(`SELECT max(time) FROM request_logs`).Scan(&newest); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(newest, "50Z") {
		t.Fatalf("the newest row was dropped instead of kept: max(time) = %q", newest)
	}
}

// 大库的 NONE/FULL → INCREMENTAL 转换需要一次重写整文件的 VACUUM，不能让启动维护
// 悄悄发起；只有显式放行才做。转换阈值是包级变量，这里压到 0 一次验证两侧行为。
func TestMaintainStorageDefersFullVacuumUnlessAllowed(t *testing.T) {
	original := fullVacuumAutoConvertMaxPages
	fullVacuumAutoConvertMaxPages = 0
	t.Cleanup(func() { fullVacuumAutoConvertMaxPages = original })

	legacy, err := openWithAutoVacuum(filepath.Join(t.TempDir(), "legacy.db"), "NONE")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })

	// 撑出一个空闲页足够多的文件：反复写大请求体再删掉，剩下的空闲页要高于
	// 增量回收阈值，否则维护本来就该直接跳过，测不到这道闸门。
	for i := 0; i < 40; i++ {
		if _, err := legacy.db.Exec(`INSERT INTO request_logs (time, status, request_body)
			VALUES ('2020-01-01T00:00:00Z', 200, hex(zeroblob(200000)))`); err != nil {
			t.Fatalf("seed large row %d: %v", i, err)
		}
	}
	if _, err := legacy.db.Exec(`DELETE FROM request_logs`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}

	size := func(query string) int {
		var n int
		if err := legacy.db.QueryRow(query).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}
	freeBefore := size(`PRAGMA freelist_count`)
	if freeBefore < incrementalVacuumThresholdPages {
		t.Skipf("seed left only %d free pages, below the reclaim threshold; nothing for the guard to defer", freeBefore)
	}

	t.Setenv("GATEWAY_ALLOW_VACUUM", "")
	if err := legacy.MaintainStorageContext(context.Background()); err != nil {
		t.Fatalf("deferring a conversion is not a failure: %v", err)
	}
	if free := size(`PRAGMA freelist_count`); free != freeBefore {
		t.Fatalf("free pages went from %d to %d while the conversion was supposed to be deferred", freeBefore, free)
	}

	t.Setenv("GATEWAY_ALLOW_VACUUM", "1")
	if err := legacy.MaintainStorageContext(context.Background()); err != nil {
		t.Fatalf("opted-in conversion: %v", err)
	}
	// 放行之后要真的把空闲页还给操作系统 —— 操作者要的是这个，不是模式标签。
	if free := size(`PRAGMA freelist_count`); free >= freeBefore {
		t.Fatalf("opted-in VACUUM left %d free pages, want fewer than %d", free, freeBefore)
	}
}
