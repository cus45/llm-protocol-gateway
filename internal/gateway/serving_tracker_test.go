package gateway

import (
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// 会话粘性命中的备选在服务时，粘滞位（ActiveProviderID）不会被改写；控制台的
// 「在服务」必须跟着 tracker 走，否则日志显示备选、界面却一直标着首选。
func TestServingProviderBeatsStaleActiveProvider(t *testing.T) {
	t.Parallel()
	tracker := newServingProviderTracker()
	key := domain.APIKey{ID: "k1", ActiveProviderID: ""}

	if got := (&Server{servingProviders: tracker}).servingProviderFor(key); got != "" {
		t.Fatalf("no traffic yet: serving = %q, want empty", got)
	}
	// 第一次响应立即生效：刚发完请求就去控制台看，应该已经是备选。
	tracker.note("k1", "backup-7", 0)
	if got := tracker.serving("k1"); got != "backup-7" {
		t.Fatalf("first answer: serving = %q, want backup-7", got)
	}
	stale := domain.APIKey{ID: "k1", ActiveProviderID: ""}
	if got := (&Server{servingProviders: tracker}).servingProviderFor(stale); got != "backup-7" {
		t.Fatalf("serving should win over sticky slot: %q", got)
	}
}

func TestServingProviderDebouncesFlapping(t *testing.T) {
	t.Parallel()
	tracker := newServingProviderTracker()
	tracker.note("k1", "p1", 0)
	// 两个会话交替服务：短时间内的切换不应让绿点来回跳。
	tracker.note("k1", "p2", 200*time.Millisecond)
	if got := tracker.serving("k1"); got != "p1" {
		t.Fatalf("pending move leaked: %q", got)
	}
	tracker.note("k1", "p1", 200*time.Millisecond)
	time.Sleep(250 * time.Millisecond)
	if got := tracker.serving("k1"); got != "p1" {
		t.Fatalf("settled back on p1, got %q", got)
	}
	// 稳定切到 p3 后才落定。
	tracker.note("k1", "p3", 50*time.Millisecond)
	time.Sleep(120 * time.Millisecond)
	if got := tracker.serving("k1"); got != "p3" {
		t.Fatalf("settled move: %q, want p3", got)
	}
}

func TestServingProviderFallsBackToActiveProvider(t *testing.T) {
	t.Parallel()
	server := &Server{servingProviders: newServingProviderTracker()}
	key := domain.APIKey{ID: "k2", ActiveProviderID: "backup-2"}
	if got := server.servingProviderFor(key); got != "backup-2" {
		t.Fatalf("restart fallback: %q, want backup-2", got)
	}
}
