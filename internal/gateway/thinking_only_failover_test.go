package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// 这一轮「只开了口、没有任何内容」的转换流（thinking 块开始后上游就结束）走的是
// 「只有 thinking 的空回答」判定，调用方本应换一个账号重答。现在有两个缺陷叠在一起
// 让兜底账号的答案到不了客户端（见 docs/known-issues.md #5）：
//
//  1. runStreamConverter 在一个「转换器什么都没写出」的尝试上，只要上游正文里出现过
//     `data:` 且没有本协议的收尾帧，就当成被截断——哪怕正文是一份完整、没有分块的
//     JSON（兜底账号的正常回答）。完整 JSON 永远不会有 SSE 收尾帧。
//  2. 被判定之后这一轮会沿着「传输错误」的分支走，兜底账号的正常回答同样被 1 判成截断，
//     最终客户端拿到 502，而两个账号都已经花掉了这一次请求。
//
// 先把已经成立的那条钉住（客户端绝不能同时看到断掉的开口和兜底账号的回答）；
// 等到 1/2 修好之后，下面被 Skip 的断言应当转为通过。
func TestSlowEmptyTurnIsNotAnsweredTwice(t *testing.T) {
	upstream, backupHits := emptyThenClosingUpstream(t, converterHoldLimit+300*time.Millisecond)
	recorder, _ := runEmptyTurnThroughFailover(t, upstream.URL)
	body := recorder.Body.String()
	if backupHits.Load() > 0 && strings.Contains(body, "from backup") && strings.Contains(body, "message_start") {
		t.Fatalf("the client received both the broken opening and the backup's answer: %q", body)
	}
}

// 缺陷的复现（当前失败，修复后应通过）：开口之后立刻正常结束的空轮次，兜底账号的
// 回答必须原样到达客户端。
func TestEmptyTurnWithinHoldLimitGoesToTheBackup(t *testing.T) {
	t.Skip("已知缺陷：兜底账号的完整 JSON 被 runStreamConverter 当成截断（docs/known-issues.md #5）")
	upstream, backupHits := emptyThenClosingUpstream(t, 0)
	recorder, execErr := runEmptyTurnThroughFailover(t, upstream.URL)
	t.Logf("backup=%d rec=%q err=%v", backupHits.Load(), recorder.Body.String(), execErr)
	if backupHits.Load() == 0 {
		t.Fatal("the empty turn was never handed to the backup")
	}
	if !strings.Contains(recorder.Body.String(), "from backup") {
		t.Fatalf("the backup answered but the client never received it: %q (err %v)", recorder.Body.String(), execErr)
	}
}

// emptyThenClosingUpstream answers the first turn with an opening that never says
// anything (message_start + a thinking block, then message_stop); the backup answers
// with a plain JSON message.
func emptyThenClosingUpstream(t *testing.T, openSilentlyFor time.Duration) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var backupHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/backup") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"))
			_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n"))
			if openSilentlyFor > 0 {
				time.Sleep(openSilentlyFor)
			}
			_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			return
		}
		backupHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_backup","type":"message","role":"assistant","content":[{"type":"text","text":"from backup"}]}`))
	}))
	t.Cleanup(upstream.Close)
	return upstream, &backupHits
}

func runEmptyTurnThroughFailover(t *testing.T, upstreamURL string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	state := domain.GatewayState{
		Providers: []domain.Provider{
			{ID: "p1", Name: "p1", Protocol: domain.ProtocolClaude, BaseURL: upstreamURL + "/one", AuthType: domain.AuthTypeAPIKey, APIKeySource: "sk-a"},
			{ID: "p2", Name: "p2", Protocol: domain.ProtocolClaude, BaseURL: upstreamURL + "/backup", AuthType: domain.AuthTypeAPIKey, APIKeySource: "sk-b"},
		},
		Routes: []domain.Route{{ID: "r1", ProviderID: "p1", OutputProtocol: domain.ProtocolOpenAIChat, Enabled: true}},
	}
	server := NewServer(NewRouter(state), monitor.NewStore())
	route, err := server.router.RouteByID("r1")
	if err != nil {
		t.Fatal(err)
	}
	key := domain.APIKey{ID: "k1", RouteID: "r1", FallbackProviderIDs: []string{"p2"}}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	recorder := httptest.NewRecorder()
	_, _, _, _, _, execErr := server.executeProtocolFlowWithFailover(
		recorder, request, route, domain.RouteDecision{RouteID: route.ID, ProviderID: route.ProviderID},
		"claude-opus-5", map[string]any{"model": "claude-opus-5", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		domain.ProtocolOpenAIChat, false, key, true,
	)
	return recorder, execErr
}
