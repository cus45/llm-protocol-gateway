package store

import (
	"net/http"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/monitoring"
)

// 一轮轻探测落两行（chat / tools，时间戳相同）。连续失败与当日配额都按轮算：
// 按行算会让默认 4 次/天变成 2 轮，也会让一轮两个 case 都失败就凑满
// probe_failed 的 consecutiveFailures=2。
func TestMonitorProbeOutcomeCountsRoundsNotRows(t *testing.T) {
	db := openTestProbeStore(t)
	round := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	insert := func(at time.Time, caseKey string, ok bool, status int) {
		t.Helper()
		if err := db.InsertMonitorProbeRun(monitoring.ProbeRunRecord{
			ProviderID: "p1", Kind: "light", Case: caseKey, OK: ok, Status: status,
			Day: at.Format("2006-01-02"), Time: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 同一轮的两行共用一个时间戳，与 persistProbeRun 的写法一致。
	insertRound := func(at time.Time, chatStatus int, toolsOk bool, toolsStatus int) {
		insert(at, "chat", chatStatus == http.StatusOK, chatStatus)
		insert(at, "tools", toolsOk, toolsStatus)
	}

	insertRound(round, http.StatusOK, true, http.StatusOK)
	outcome, used, err := db.MonitorProbeOutcome("p1", round.Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	if used != 1 || outcome.Consecutive != 0 {
		t.Fatalf("one passing round: used=%d outcome=%+v", used, outcome)
	}

	insertRound(round.Add(time.Hour), http.StatusOK, false, http.StatusInternalServerError)
	outcome, used, err = db.MonitorProbeOutcome("p1", round.Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	if used != 2 || outcome.Consecutive != 1 {
		t.Fatalf("one failed round must count once: used=%d outcome=%+v", used, outcome)
	}
	if outcome.LastStatus != 500 {
		t.Fatalf("last status = %d, want the failed round's 500", outcome.LastStatus)
	}

	// 同一轮里只有 tools 失败：最近一轮的状态要跟失败 case 走（401），chat 的
	// 200 不能把这一轮盖成健康。
	insertRound(round.Add(2*time.Hour), http.StatusOK, false, http.StatusUnauthorized)
	outcome, _, err = db.MonitorProbeOutcome("p1", round.Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.AuthFailure || outcome.LastStatus != http.StatusUnauthorized {
		t.Fatalf("a tools-only 401 must surface as the round's status: %+v", outcome)
	}
	if outcome.Consecutive != 2 {
		t.Fatalf("consecutive failed rounds = %d, want 2", outcome.Consecutive)
	}

	// 更早的轮次不覆写最近结果：再插一轮 500，最近一轮仍是上面的 401 轮。
	insertRound(round.Add(3*time.Hour), http.StatusOK, false, http.StatusInternalServerError)
	older, _, err := db.MonitorProbeOutcome("p1", round.Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	if older.LastStatus != http.StatusInternalServerError || older.Consecutive != 3 {
		t.Fatalf("latest round must be the 500 round: %+v", older)
	}

	// 成功一轮归零。
	insertRound(round.Add(4*time.Hour), http.StatusOK, true, http.StatusOK)
	outcome, used, err = db.MonitorProbeOutcome("p1", round.Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Consecutive != 0 || used != 5 || outcome.LastStatus != http.StatusOK || outcome.AuthFailure {
		t.Fatalf("a passing round must reset the streak: used=%d outcome=%+v", used, outcome)
	}
}

func openTestProbeStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
