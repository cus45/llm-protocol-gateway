package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/monitor"
	"github.com/luca/llm-protocol-gateway/internal/monitoring"
)

// monitorStreamServer starts the real router behind httptest so the handler
// gets a flushing, cancellable connection like it does in production, and
// returns a client that stamps the admin identity the console's session
// middleware would inject.
func monitorStreamServer(t *testing.T) (*Server, *httptest.Server, *http.Client) {
	t.Helper()
	server, _ := monitorTestServer(t, monitorClaudeProvider(), monitorAPIKeyProvider())
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	client := &http.Client{Transport: monitorStreamTransport{identity: monitorAdminID()}}
	return server, ts, client
}

type monitorStreamTransport struct {
	identity sessionIdentity
}

func (tr monitorStreamTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return http.DefaultTransport.RoundTrip(
		request.WithContext(context.WithValue(request.Context(), authContextKey{}, tr.identity)))
}

type monitorStreamFrame struct {
	Event string
	ID    string
	Data  string
}

// readMonitorStreamFrame scans one frame, skipping heartbeat comments
// (": ping") and blank separators. ok=false means the body ended. A bare
// comment frame terminates a frame just like a blank line does.
func readMonitorStreamFrame(t *testing.T, scanner *bufio.Scanner) (monitorStreamFrame, bool) {
	t.Helper()
	frame := monitorStreamFrame{}
	for scanner.Scan() {
		line := scanner.Text()

		if line == "" || strings.HasPrefix(line, ":") {
			if frame.Event != "" {
				return frame, true
			}
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		// SSE allows "field:value"; the writer always emits one space.
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			frame.ID = value
		case "event":
			frame.Event = value
		case "data":
			if strings.Contains(value, "\n") {
				t.Fatalf("data must be single-line JSON, got %q", value)
			}
			frame.Data = value
		}
	}
	return frame, false
}

// readMonitorStreamFrameWithin is readMonitorStreamFrame with a deadline: a
// long-lived SSE connection stays open and silent after its burst, so a plain
// scan would block forever waiting for the next heartbeat.
func readMonitorStreamFrameWithin(t *testing.T, scanner *bufio.Scanner, wait time.Duration) (monitorStreamFrame, bool) {
	t.Helper()
	type result struct {
		frame monitorStreamFrame
		ok    bool
	}
	done := make(chan result, 1)
	go func() {
		frame, ok := readMonitorStreamFrame(t, scanner)
		done <- result{frame, ok}
	}()
	select {
	case got := <-done:
		return got.frame, got.ok
	case <-time.After(wait):
		t.Fatalf("no SSE frame within %s", wait)
		return monitorStreamFrame{}, false
	}
}

// openMonitorStream dials the endpoint and returns the frame stream. The
// connection is torn down at test cleanup.
func openMonitorStream(t *testing.T, ts *httptest.Server, client *http.Client, lastEventID string) *bufio.Scanner {
	t.Helper()
	scanner, _ := openMonitorStreamWithContext(t, ts, client, lastEventID)
	return scanner
}

// openMonitorStreamWithContext is openMonitorStream plus the cancel that hangs
// up the connection: a handler holds the connection until the client or the
// request is gone, so the tests that count goroutines and subscribers must be
// able to close it mid-test.
func openMonitorStreamWithContext(t *testing.T, ts *httptest.Server, client *http.Client, lastEventID string) (*bufio.Scanner, context.CancelFunc) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/__monitor/stream", nil)
	if err != nil {
		stop()
		t.Fatal(err)
	}
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	response, err := client.Do(request)
	if err != nil {
		stop()
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		stop()
		t.Fatalf("stream status = %d, want 200", response.StatusCode)
	}
	if ct := response.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
	t.Cleanup(func() {
		stop()
		_ = response.Body.Close()
	})
	return bufio.NewScanner(response.Body), stop
}

func waitForSubscribers(t *testing.T, server *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if server.monitorStreamHub().subscribers() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("subscribers = %d, want %d", server.monitorStreamHub().subscribers(), want)
}

// Live request frames are ephemeral, throttled, and carry no request bodies.
// A console that is not connected must not consume the throttle budget either:
// the next subscriber should see traffic immediately.
func TestMonitorStreamTrafficFrames(t *testing.T) {
	server, _ := monitorTestServer(t, monitorClaudeProvider(), monitorAPIKeyProvider())
	hub := server.monitorStreamHub()
	entry := monitor.RequestLog{
		Time: time.Now().UTC(), ProviderID: "p-claude", Model: "claude-sonnet",
		Status: 200, LatencyMillis: 1200, TTFTMillis: 340,
		InputTokens: 42, OutputTokens: 7, CostMicros: 1234, Priced: true,
		RequestBody: `{"prompt":"sk-ant-SECRET"}`,
	}

	// No subscriber: publishing is a no-op and must not start the throttle window.
	server.publishMonitorTraffic(entry)
	sub, ok := hub.subscribe()
	if !ok {
		t.Fatal("subscribe failed")
	}
	t.Cleanup(func() { hub.unsubscribe(sub) })

	server.publishMonitorTraffic(entry)
	first := nextTrafficFrame(t, sub)
	var payload map[string]any
	if err := json.Unmarshal(first.Data, &payload); err != nil {
		t.Fatalf("traffic payload: %v", err)
	}
	if payload["providerId"] != "p-claude" || payload["model"] != "claude-sonnet" {
		t.Fatalf("traffic payload lost routing fields: %v", payload)
	}
	if strings.Contains(string(first.Data), "SECRET") {
		t.Fatalf("traffic frame leaked the request body: %s", first.Data)
	}

	// Second publish inside the interval is dropped.
	server.publishMonitorTraffic(entry)
	select {
	case frame := <-sub.ch:
		t.Fatalf("throttled publish still emitted %q", frame.Event)
	default:
	}

	// After the interval a frame goes out again.
	time.Sleep(monitorTrafficInterval + 20*time.Millisecond)
	server.publishMonitorTraffic(entry)
	nextTrafficFrame(t, sub)
}

func nextTrafficFrame(t *testing.T, sub *monitorStreamSubscriber) monitorStreamEnvelope {
	t.Helper()
	select {
	case frame := <-sub.ch:
		if frame.Event != monitorStreamTraffic {
			t.Fatalf("frame event = %q, want %q", frame.Event, monitorStreamTraffic)
		}
		return frame
	case <-time.After(time.Second):
		t.Fatal("no traffic frame")
		return monitorStreamEnvelope{}
	}
}

// ① Connecting yields a snapshot carrying every console section, then no-ops.
func TestMonitorStreamSendsSnapshotOnConnect(t *testing.T) {
	server, ts, client := monitorStreamServer(t)
	scanner := openMonitorStream(t, ts, client, "")
	waitForSubscribers(t, server, 1)

	frame, ok := readMonitorStreamFrameWithin(t, scanner, 3*time.Second)
	if !ok {
		t.Fatal("stream closed before the snapshot")
	}
	if frame.Event != "snapshot" {
		t.Fatalf("first frame = %q, want snapshot", frame.Event)
	}
	var snapshot struct {
		Overview struct {
			GeneratedAt string `json:"generatedAt"`
			Accounts    struct {
				Total int `json:"total"`
			} `json:"accounts"`
		} `json:"overview"`
		Accounts    []monitorAccountView       `json:"accounts"`
		Alerts      []json.RawMessage          `json:"alerts"`
		Probes      monitorStreamProbeSnapshot `json:"probes"`
		EventCursor int64                      `json:"eventCursor"`
	}
	if err := json.Unmarshal([]byte(frame.Data), &snapshot); err != nil {
		t.Fatalf("snapshot JSON: %v (%s)", err, frame.Data)
	}
	if snapshot.Overview.GeneratedAt == "" || snapshot.Overview.Accounts.Total != 2 {
		t.Fatalf("overview = %+v", snapshot.Overview)
	}
	if len(snapshot.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(snapshot.Accounts))
	}
	if snapshot.Alerts == nil || snapshot.Probes.Accounts == nil || snapshot.Probes.Runs == nil {
		t.Fatalf("snapshot sections must be present, not null: %s", frame.Data)
	}
	if len(snapshot.Probes.Accounts) != 2 || snapshot.Probes.Config.EventDriven.MinIntervalMinutes < 0 {
		t.Fatalf("probes = %+v", snapshot.Probes)
	}
	// The credential never rides the stream.
	assertNoSecrets(t, frame.Data)
	if strings.Contains(frame.Data, "rt-SECRET-REFRESH") || strings.Contains(frame.Data, "sk-ant-oat01-ACCESS") {
		t.Fatalf("snapshot leaked a credential")
	}
}

// ②b Alert state changes (firing, ack, resolve) reach the console as `alert`
// frames carrying the alert row, with the id as the resume cursor.
func TestMonitorStreamPublishesAlertChanges(t *testing.T) {
	server, ts, client := monitorStreamServer(t)
	scanner := openMonitorStream(t, ts, client, "")
	waitForSubscribers(t, server, 1)
	if frame, ok := readMonitorStreamFrameWithin(t, scanner, 3*time.Second); !ok || frame.Event != "snapshot" {
		t.Fatalf("first frame = %+v ok=%v", frame, ok)
	}

	state, err := server.monitoringStore.UpsertMonitorState(monitoring.AlertState{
		Fingerprint: "fp-stream", RuleKey: "quota_low", Severity: monitoring.SeverityP1,
		TargetType: monitoring.TargetProvider, TargetID: "p-claude", TargetName: "Claude 主号",
		Status: monitoring.StatusFiring, Detail: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	frame, ok := readMonitorStreamFrameWithin(t, scanner, 3*time.Second)
	if !ok || frame.Event != "alert" {
		t.Fatalf("after upsert = %+v ok=%v", frame, ok)
	}
	if frame.ID != itoa(state.ID) {
		t.Fatalf("alert frame id = %q, want alert id %d", frame.ID, state.ID)
	}
	var payload struct {
		ID     int64                  `json:"id"`
		Reason string                 `json:"reason"`
		Alert  *monitoring.AlertState `json:"alert"`
	}
	if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Reason != "firing" || payload.Alert == nil || payload.Alert.Fingerprint != "fp-stream" {
		t.Fatalf("alert payload = %s", frame.Data)
	}

	if err := server.monitoringStore.SetMonitorDisposition(state.ID, monitoring.DispositionAcknowledged, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	acked, ok := readMonitorStreamFrameWithin(t, scanner, 3*time.Second)
	if !ok || acked.Event != "alert" || !strings.Contains(acked.Data, `"reason":"acknowledged"`) {
		t.Fatalf("after ack = %+v ok=%v", acked, ok)
	}
	assertNoSecrets(t, frame.Data)
}

// ② A committed event reaches a connected console as an `events` frame and the
// follow-up probe round as a `probe` frame.
func TestMonitorStreamPublishesEventsAfterInsert(t *testing.T) {
	server, ts, client := monitorStreamServer(t)
	scanner := openMonitorStream(t, ts, client, "")
	waitForSubscribers(t, server, 1)
	if frame, ok := readMonitorStreamFrameWithin(t, scanner, 3*time.Second); !ok || frame.Event != "snapshot" {
		t.Fatalf("first frame = %+v ok=%v", frame, ok)
	}

	// The prober path writes probe rows and an event row through the same
	// monitorStreamSink the engine uses.
	server.recordProbeRun(probeRun{
		ProviderID: "p-claude", ProviderName: "Claude 主号", Kind: probeKindLight, At: time.Now().UTC(),
		Cases: []probeCaseResult{{Case: probeCaseChat, OK: true, Status: 200, Duration: 12}},
	})
	// The engine and the prober both write through s.monitoringStore; taking
	// its monitorStreamSink view proves the wrapper is the one in place and
	// that a write through it reaches the connected console.
	sink, wrapped := server.monitoringStore.(monitorStreamSink)
	if !wrapped {
		t.Fatal("monitoringStore is not wrapped by monitorStreamSink; increments would never publish")
	}
	engineStore := monitoring.StateStore(sink)
	if _, err := engineStore.InsertMonitorEvent(monitoring.Event{
		Kind: monitoring.EventKindSystem, Severity: monitoring.SeverityInfo, Status: monitoring.StatusInfo,
		TargetType: monitoring.TargetGateway, Title: "测试事件", Detail: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}

	seenProbe, seenEvents := false, false
	deadline := time.Now().Add(2 * time.Second)
	for !(seenProbe && seenEvents) {
		if time.Now().After(deadline) {
			t.Fatalf("probe=%v events=%v", seenProbe, seenEvents)
		}
		frame, ok := readMonitorStreamFrameWithin(t, scanner, 3*time.Second)
		if !ok {
			t.Fatal("stream closed early")
		}
		switch frame.Event {
		case "probe":
			var payload struct {
				ProviderID string                      `json:"providerId"`
				Runs       []monitoring.ProbeRunRecord `json:"runs"`
			}
			if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.ProviderID != "p-claude" || len(payload.Runs) == 0 || payload.Runs[0].Case != probeCaseChat {
				t.Fatalf("probe payload = %s", frame.Data)
			}
			seenProbe = true
		case "events":
			var events []monitoring.Event
			if err := json.Unmarshal([]byte(frame.Data), &events); err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || frame.ID == "" {
				t.Fatalf("events frame id=%q data=%s", frame.ID, frame.Data)
			}
			if got := frame.ID; got != itoa(events[0].ID) {
				t.Fatalf("frame id %s != event id %d", got, events[0].ID)
			}
			if events[0].Title == "测试事件" {
				seenEvents = true
			}
		}
	}
}

// ③ Reconnecting with Last-Event-ID replays only the rows written after it.
func TestMonitorStreamResumesFromLastEventID(t *testing.T) {
	server, ts, client := monitorStreamServer(t)

	first, err := server.monitoringStore.InsertMonitorEvent(monitoring.Event{
		Kind: monitoring.EventKindSystem, Severity: monitoring.SeverityInfo, Status: monitoring.StatusInfo,
		TargetType: monitoring.TargetGateway, Title: "旧事件", Detail: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.monitoringStore.InsertMonitorEvent(monitoring.Event{
		Kind: monitoring.EventKindSystem, Severity: monitoring.SeverityInfo, Status: monitoring.StatusInfo,
		TargetType: monitoring.TargetGateway, Title: "新事件", Detail: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}

	// Resume reads through the same store object the console uses, so the
	// decorator must pass the by-id read through to *store.Store.
	if _, ok := server.monitoringStore.(monitorStoreHooks); !ok {
		t.Fatal("monitoringStore does not expose MonitorEventRowsAfter; resume cannot work")
	}

	scanner := openMonitorStream(t, ts, client, itoa(first))
	// The resumed stream is idle once it has flushed: read what it already
	// sent, bounded by a deadline on every scan.
	var replayed []monitoring.Event
	var snapshotEventCursor int64
	var snapshotFrameID string
	var replayedBeforeSnapshot bool
	seenSnapshot := false
	for !seenSnapshot {
		frame, ok := readMonitorStreamFrameWithin(t, scanner, 2*time.Second)
		if !ok {
			t.Fatal("stream closed before the replay")
		}
		switch frame.Event {
		case "events":
			var events []monitoring.Event
			if err := json.Unmarshal([]byte(frame.Data), &events); err != nil {
				t.Fatal(err)
			}
			replayedBeforeSnapshot = !seenSnapshot
			replayed = append(replayed, events...)
		case "snapshot":
			seenSnapshot = true
			snapshotFrameID = frame.ID
			var snapshot struct {
				EventCursor int64 `json:"eventCursor"`
				Overview    struct {
					RecentEvents []monitoring.Event `json:"recentEvents"`
				} `json:"overview"`
			}
			if err := json.Unmarshal([]byte(frame.Data), &snapshot); err != nil {
				t.Fatal(err)
			}
			snapshotEventCursor = snapshot.EventCursor
			if len(snapshot.Overview.RecentEvents) < 2 {
				t.Fatalf("snapshot recent events = %d, want the full history", len(snapshot.Overview.RecentEvents))
			}
		}
	}
	if len(replayed) != 1 || replayed[0].Title != "新事件" || replayed[0].ID <= first {
		t.Fatalf("resume replayed %+v, want only the row after %d", replayed, first)
	}
	if !replayedBeforeSnapshot {
		t.Fatal("the gap should be replayed before the snapshot")
	}
	if snapshotEventCursor != replayed[0].ID {
		t.Fatalf("snapshot eventCursor = %d, want %d; frames snapshotID=%q", snapshotEventCursor, replayed[0].ID, snapshotFrameID)
	}

	// A fresh connection (no Last-Event-ID) never replays history: its first
	// frame is the snapshot, carrying the newest event id as the cursor.
	fresh := openMonitorStream(t, ts, client, "")
	firstFresh, ok := readMonitorStreamFrameWithin(t, fresh, 2*time.Second)
	if !ok || firstFresh.Event != "snapshot" {
		t.Fatalf("fresh first frame = %+v ok=%v", firstFresh, ok)
	}
	var freshSnapshot struct {
		EventCursor int64 `json:"eventCursor"`
	}
	if err := json.Unmarshal([]byte(firstFresh.Data), &freshSnapshot); err != nil {
		t.Fatal(err)
	}
	if freshSnapshot.EventCursor != replayed[0].ID || firstFresh.ID != itoa(replayed[0].ID) {
		t.Fatalf("snapshot cursor id=%q eventCursor=%d, want %d", firstFresh.ID, freshSnapshot.EventCursor, replayed[0].ID)
	}
}

// ④ A disconnected client leaves no subscriber and no goroutine behind.
func TestMonitorStreamUnsubscribesOnDisconnect(t *testing.T) {
	server, ts, client := monitorStreamServer(t)

	// One connection stays up: the goroutine count is compared before and
	// after a SECOND connection hangs up, so the live one cancels out.
	keepScanner, keepAlive := openMonitorStreamWithContext(t, ts, client, "")
	waitForSubscribers(t, server, 1)
	if frame, ok := readMonitorStreamFrameWithin(t, keepScanner, 3*time.Second); !ok || frame.Event != "snapshot" {
		t.Fatalf("first connection first frame = %+v ok=%v", frame, ok)
	}
	scanner, cancel := openMonitorStreamWithContext(t, ts, client, "")
	waitForSubscribers(t, server, 2)
	if frame, ok := readMonitorStreamFrameWithin(t, scanner, 3*time.Second); !ok || frame.Event != "snapshot" {
		t.Fatalf("second connection first frame = %+v ok=%v", frame, ok)
	}
	// Steady state: handler, heartbeat, and the transport's reader.
	settle := time.Now().Add(2 * time.Second)
	for time.Now().Before(settle) {
		time.Sleep(20 * time.Millisecond)
	}
	withBoth := runtime.NumGoroutine()

	cancel()
	waitForSubscribers(t, server, 1)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && runtime.NumGoroutine() > withBoth-1 {
		time.Sleep(10 * time.Millisecond)
	}
	after := runtime.NumGoroutine()
	if after >= withBoth {
		t.Fatalf("hanging up one stream left goroutines at %d (was %d with both)", after, withBoth)
	}
	// The remaining connection is still served: the hub is live, not drained.
	if _, err := server.monitoringStore.InsertMonitorEvent(monitoring.Event{
		Kind: monitoring.EventKindSystem, Severity: monitoring.SeverityInfo, Status: monitoring.StatusInfo,
		TargetType: monitoring.TargetGateway, Title: "仍在推送", Detail: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	if frame, ok := readMonitorStreamFrameWithin(t, keepScanner, 3*time.Second); !ok || frame.Event != "events" {
		t.Fatalf("surviving connection got %+v ok=%v, want events", frame, ok)
	}
	keepAlive()
	waitForSubscribers(t, server, 0)
}

// ⑤ A subscriber that never reads cannot stall publishing; frames are evicted
// oldest-first and the loss is counted.
func TestMonitorStreamSlowSubscriberDoesNotBlockPublish(t *testing.T) {
	hub := newMonitorStreamHub()
	sub, ok := hub.subscribe()
	if !ok {
		t.Fatal("subscribe refused")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			hub.publish(monitorStreamEnvelope{Event: monitorStreamEvents, ID: int64(i + 1), Data: json.RawMessage(`[]`)})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a slow subscriber")
	}
	if queued := len(sub.ch); queued != monitorStreamQueue {
		t.Fatalf("queued = %d, want the buffer cap %d", queued, monitorStreamQueue)
	}
	hub.mu.Lock()
	dropped, total := sub.dropped, hub.droppedTotal
	hub.mu.Unlock()
	if dropped != 100-monitorStreamQueue || total != uint64(dropped) {
		t.Fatalf("dropped = %d total = %d, want %d", dropped, total, 100-monitorStreamQueue)
	}
	// The newest frame survives: a reader draining now catches up to 100.
	var newest int64
	for len(sub.ch) > 0 {
		newest = (<-sub.ch).ID
	}
	if newest != 100 {
		t.Fatalf("newest queued = %d, want 100", newest)
	}
	hub.unsubscribe(sub)
	if hub.subscribers() != 0 {
		t.Fatalf("subscribers = %d after unsubscribe", hub.subscribers())
	}
}

// ⑥ Only admins may subscribe. The full chain (withAdminAuth) is exercised by
// TestMonitorEndpointsRequireAdmin; here the handler is its own gate.
func TestMonitorStreamRequiresAdmin(t *testing.T) {
	server, _ := monitorTestServer(t, monitorClaudeProvider())

	user := monitorCall(t, server.handleMonitorStream, http.MethodGet, "/__monitor/stream", "", nil, monitorUserID())
	if user.Code != http.StatusForbidden {
		t.Fatalf("user = %d body=%s, want 403", user.Code, user.Body.String())
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18093/__monitor/stream", nil)
	naked := httptest.NewRecorder()
	server.handleMonitorStream(naked, request)
	if naked.Code != http.StatusUnauthorized {
		t.Fatalf("no identity = %d, want 401", naked.Code)
	}
	if server.monitorStreamHub().subscribers() != 0 {
		t.Fatalf("a rejected request registered a subscriber")
	}
}
