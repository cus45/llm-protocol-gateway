package gateway

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// timedRecorder records when each write reached the client, so a test can tell
// a stream from a reply that only arrived at the end.
// 底层用 syncRecorder：心跳 goroutine 与流拷贝会并发写，httptest 的 buffer 没锁。
type timedRecorder struct {
	*syncRecorder
	mu     sync.Mutex
	first  time.Time
	writes int
	bytes  int
}

func newTimedRecorder() *timedRecorder {
	return &timedRecorder{syncRecorder: newSyncRecorder()}
}

func (r *timedRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	if r.first.IsZero() && !isSSECommentPayload(p) {
		r.first = time.Now()
	}
	r.writes++
	r.bytes += len(p)
	r.mu.Unlock()
	return r.syncRecorder.Write(p)
}

func (r *timedRecorder) Flush() { r.syncRecorder.Flush() }

// pacedBody sends each chunk after a pause, the way an upstream model does:
// finish is called when the last chunk has been handed out.
type pacedBody struct {
	mu     sync.Mutex
	chunks []string
	delay  time.Duration
	index  int
	done   chan struct{}
}

func newPacedBody(delay time.Duration, chunks ...string) *pacedBody {
	return &pacedBody{chunks: chunks, delay: delay, done: make(chan struct{})}
}

func (b *pacedBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.index >= len(b.chunks) {
		close(b.done)
		return 0, io.EOF
	}
	if b.index > 0 {
		time.Sleep(b.delay)
	}
	n := copy(p, b.chunks[b.index])
	b.index++
	return n, nil
}

func (b *pacedBody) Close() error { return nil }

// The report's probe: a pass-through stream through the failover writer must
// reach the client as it comes — first byte long before the last, one write per
// chunk — whether or not the attempt could still be deferred. Buffering the
// whole reply made time-to-first-token equal the whole request's duration.
func TestPassThroughStreamsThroughFailoverAsItComes(t *testing.T) {
	const chunks = 3
	for _, canDefer := range []bool{true, false} {
		name := "lastAttempt"
		if canDefer {
			name = "backupBehind"
		}
		t.Run(name, func(t *testing.T) {
			rec := newTimedRecorder()
			fw := newFailoverResponseWriter(rec, canDefer)
			body := newPacedBody(400*time.Millisecond,
				"data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n",
				"data: {\"choices\":[{\"delta\":{\"content\":\"two\"}}]}\n\n",
				"data: {\"choices\":[{\"delta\":{\"content\":\"three\"}}]}\n\n",
				"data: [DONE]\n\n",
			)
			response := &http.Response{
				StatusCode:    http.StatusOK,
				ContentLength: -1,
				Header:        http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:          body,
			}
			start := time.Now()
			_, _, _, err := writePassThroughResponse(fw, response, true, usageStreamOpenAI, ParseOpenAIUsage, openAIChatStreamInterrupter{model: "m"})
			total := time.Since(start)
			if err != nil {
				t.Fatalf("complete stream reported %v", err)
			}
			fw.FlushError() // what the loop does once it settles on this attempt
			rec.mu.Lock()
			first, writes := rec.first, rec.writes
			rec.mu.Unlock()
			t.Logf("canDefer=%v firstWrite=%v total=%v writes=%d", canDefer, first.Sub(start), total, writes)
			if first.IsZero() {
				t.Fatal("nothing reached the client before the stream ended")
			}
			if firstWrite := first.Sub(start); firstWrite >= total/2 {
				t.Fatalf("first byte after %v of a %v stream: the reply was buffered, not streamed", firstWrite, total)
			}
			if writes < chunks {
				t.Fatalf("client saw %d writes for %d chunks apart: the chunks were coalesced", writes, chunks+1)
			}
			if !strings.Contains(rec.String(), "data: [DONE]") {
				t.Fatalf("the stream was cut short: %q", rec.String())
			}
		})
	}
}

// The report's probe without the failover writer: the stream through a bare
// writer is the baseline streaming is measured against, and it must stay as
// fast as the failover path — one path, not two behaviours.
func TestPassThroughBareWriterStreamsAsItComes(t *testing.T) {
	rec := newTimedRecorder()
	chunk := func(text string) string {
		return "data: {\"choices\":[{\"delta\":{\"content\":\"" + text + "\"}}]}\n\n"
	}
	body := newPacedBody(400*time.Millisecond, chunk("one"), chunk("two"), chunk("three"), "data: [DONE]\n\n")
	response := &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: -1,
		Header:        http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:          body,
	}
	start := time.Now()
	if _, _, _, err := writePassThroughResponse(rec, response, true, usageStreamOpenAI, ParseOpenAIUsage, openAIChatStreamInterrupter{model: "m"}); err != nil {
		t.Fatalf("complete stream reported %v", err)
	}
	total := time.Since(start)
	rec.mu.Lock()
	first, writes := rec.first, rec.writes
	rec.mu.Unlock()
	t.Logf("bare firstWrite=%v total=%v writes=%d", first.Sub(start), total, writes)
	if first.Sub(start) >= total/2 || writes < 3 {
		t.Fatalf("bare writer buffered the reply: firstWrite=%v total=%v writes=%d", first.Sub(start), total, writes)
	}
}

// A keep-alive written while a pass-through stream is flowing is the whole
// point of it: an idle thinking period must reach the client, and through the
// failover writer Flush must actually flush.
func TestPassThroughHeartbeatReachesClient(t *testing.T) {
	old := sseHeartbeatInterval
	sseHeartbeatInterval = 20 * time.Millisecond
	defer func() { sseHeartbeatInterval = old }()

	rec := newTimedRecorder()
	fw := newFailoverResponseWriter(rec, true)
	// The upstream goes quiet after its opening frame, the way a model does
	// while it thinks.
	body := newPacedBody(200*time.Millisecond,
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\n",
		"data: [DONE]\n\n",
	)
	response := &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: -1,
		Header:        http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:          body,
	}
	if _, _, _, err := writePassThroughResponse(fw, response, true, usageStreamOpenAI, ParseOpenAIUsage, openAIChatStreamInterrupter{model: "m"}); err != nil {
		t.Fatalf("complete stream reported %v", err)
	}
	fw.FlushError()
	if got := len(rec.String()); !strings.Contains(rec.String(), ": ping") {
		t.Fatalf("the keep-alive never reached the client (%d bytes: %q)", got, rec.String())
	}
}

// A stream of thinking: the opening frames arrive, then a long silence, then
// the answer. The opening reaches the client at once (with the heartbeats that
// fill the silence), and the content follows as it comes.
func TestPassThroughHeartbeatBeforeFirstDeltaReachesClient(t *testing.T) {
	old := sseHeartbeatInterval
	sseHeartbeatInterval = 20 * time.Millisecond
	defer func() { sseHeartbeatInterval = old }()

	const silence = 300 * time.Millisecond
	for _, canDefer := range []bool{true, false} {
		rec := newTimedRecorder()
		fw := newFailoverResponseWriter(rec, canDefer)
		body := newPacedBody(silence,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-x\",\"usage\":{\"input_tokens\":3}}}\n\n"+
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		)
		response := &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: -1,
			Header:        http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:          body,
		}
		start := time.Now()
		if _, _, _, err := writePassThroughResponse(fw, response, true, usageStreamClaude, ParseClaudeUsage, claudeStreamInterrupter{}); err != nil {
			t.Fatalf("complete stream reported %v", err)
		}
		fw.FlushError()
		total := time.Since(start)
		if first := rec.first.Sub(start); first > silence/2 {
			t.Fatalf("canDefer=%v: the opening only reached the client after %v of a %v stream", canDefer, first, total)
		}
		if got := strings.Count(rec.String(), ": ping"); got < 3 {
			t.Fatalf("canDefer=%v: the silence was not filled with keep-alives (%d): %q", canDefer, got, rec.String())
		}
		if !strings.Contains(rec.String(), `"text":"hi"`) {
			t.Fatalf("canDefer=%v: the answer never reached the client: %q", canDefer, rec.String())
		}
	}
}
