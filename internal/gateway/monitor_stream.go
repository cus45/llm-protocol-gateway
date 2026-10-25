package gateway

// GET /__monitor/stream — the live feed behind the monitoring console.
//
// The console used to poll /__monitor/events, /__monitor/alerts and
// /__monitor/probes every few seconds. This endpoint replaces that: one
// long-lived text/event-stream connection gets a snapshot on connect and then
// only the deltas the engine, the prober and the disposition endpoints
// publish through monitorStreamHub.
//
// Nothing here invents storage: the snapshot is the same reads the REST
// handlers do, and every incremental event carries the store's own row id, so
// Last-Event-ID resumes from the database and the in-process hub only has to
// wake clients up.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
	"github.com/luca/llm-protocol-gateway/internal/monitoring"
)

const (
	// monitorStreamMaxSubscribers is the connection cap. Each subscriber costs
	// a goroutine and a buffered channel; 32 console tabs is already far past
	// anything a single operator opens.
	monitorStreamMaxSubscribers = 32
	// monitorStreamQueue is the per-subscriber backlog. A console that falls
	// this far behind is better served by one catch-up read from the store
	// than by replaying the queue.
	monitorStreamQueue = 64
	// monitorStreamSnapshotLimit bounds each section of the connect snapshot
	// and one catch-up page.
	monitorStreamSnapshotLimit = 50
	// monitorStreamCatchUpPages bounds how many event pages one catch-up read
	// pulls; 50 × 5 = 250 rows in one frame set.
	monitorStreamCatchUpPages = 5
	// monitorTrafficInterval throttles live request frames. A busy gateway would
	// otherwise flush the queue with them and push out the probe/alert frames
	// that actually carry state. 5 frames/s is plenty for a tail view.
	monitorTrafficInterval = 200 * time.Millisecond
)

// monitorStreamEvent names mirror the SSE `event:` lines.
const (
	monitorStreamSnapshotEvent = "snapshot"
	monitorStreamEvents        = "events"
	monitorStreamAlert         = "alert"
	monitorStreamProbe         = "probe"
	monitorStreamTraffic       = "traffic"
	monitorStreamResync        = "resync"
)

// monitorStreamEnvelope is one ready-to-write frame. ID is the store row id
// the client resumes from; snapshot frames carry the cursor they were built
// against but are not replayed from it (a reconnect re-snapshots anyway).
type monitorStreamEnvelope struct {
	Event string
	Data  json.RawMessage
	ID    int64
}

// ── hub ─────────────────────────────────────────────────────────────────

type monitorStreamSubscriber struct {
	ch chan monitorStreamEnvelope
	// dropped counts frames evicted because the console fell behind; the
	// reader turns it into a catch-up read.
	dropped int
}

// monitorStreamHub is the in-process fan-out. Publishing never blocks on a slow
// client: the oldest queued frame is evicted instead, and the reader notices
// the loss and reads the gap back from the database (ids are monotonic).
type monitorStreamHub struct {
	mu sync.Mutex
	// subs is nil-safe: a zero hub still accepts and drops nothing because
	// Server.monitorStreamHub always returns a built one.
	subs         map[*monitorStreamSubscriber]struct{}
	droppedTotal uint64
}

func newMonitorStreamHub() *monitorStreamHub {
	return &monitorStreamHub{subs: map[*monitorStreamSubscriber]struct{}{}}
}

func (h *monitorStreamHub) subscribe() (*monitorStreamSubscriber, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= monitorStreamMaxSubscribers {
		return nil, false
	}
	sub := &monitorStreamSubscriber{ch: make(chan monitorStreamEnvelope, monitorStreamQueue)}
	h.subs[sub] = struct{}{}
	return sub, true
}

func (h *monitorStreamHub) unsubscribe(sub *monitorStreamSubscriber) {
	if sub == nil {
		return
	}
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
}

func (h *monitorStreamHub) subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// publish hands one frame to every subscriber and returns immediately.
func (h *monitorStreamHub) publish(envelope monitorStreamEnvelope) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if len(sub.ch) >= monitorStreamQueue {
			// Full: evict the oldest frame so the newest still arrives. The
			// eviction happens under the same lock as the send, so only one
			// publisher can create a free slot and nothing is lost outside a
			// counted drop.
			<-sub.ch
			sub.dropped++
			h.droppedTotal++
		}
		sub.ch <- envelope
	}
}

// ── publisher ───────────────────────────────────────────────────────────

// monitorStoreHooks are the stores the publisher and the catch-up reads need
// beyond monitoringStore: the rows just written. *store.Store implements it;
// a Server wired without it still serves snapshots, just without increments.
type monitorStoreHooks interface {
	monitoringStore
	MonitorEventRowsAfter(afterID int64, limit int) ([]monitoring.Event, error)
}

// monitorStreamSink decorates monitoringStore so every committed write is
// announced. The engine, the prober and the disposition handlers all write
// through s.monitoringStore, so this one wrapper covers every producer without
// touching the rule logic in internal/monitoring.
type monitorStreamSink struct {
	monitoringStore
	server *Server
}

func (w monitorStreamSink) InsertMonitorEvent(event monitoring.Event) (int64, error) {
	id, err := w.monitoringStore.InsertMonitorEvent(event)
	if err == nil && id > 0 {
		w.server.publishMonitorEvents(id)
	}
	return id, err
}

func (w monitorStreamSink) InsertMonitorProbeRuns(records []monitoring.ProbeRunRecord) error {
	err := w.monitoringStore.InsertMonitorProbeRuns(records)
	if err == nil && len(records) > 0 {
		w.server.publishMonitorProbes(records)
	}
	return err
}

func (w monitorStreamSink) UpsertMonitorState(state monitoring.AlertState) (monitoring.AlertState, error) {
	// The engine calls upsert once per evaluation, firing or not; only the
	// transitions the console renders are worth a frame.
	previous, existed := w.server.monitorStateForFingerprint(state.Fingerprint)
	updated, err := w.monitoringStore.UpsertMonitorState(state)
	if err != nil || updated.ID <= 0 {
		return updated, err
	}
	if reason := streamAlertReason(previous, existed, updated); reason != "" {
		w.server.publishMonitorAlert(reason, updated.ID)
	}
	return updated, nil
}

func (w monitorStreamSink) SetMonitorDisposition(id int64, disposition string, at time.Time) error {
	err := w.monitoringStore.SetMonitorDisposition(id, disposition, at)
	if err == nil {
		w.server.publishMonitorAlert(disposition, id)
	}
	return err
}

// MonitorEventRowsAfter rides on the wrapped store when it has the read; the
// sink itself is the object stored in Server.monitoringStore, so the resume
// path and the insert payload must be able to reach it through the decorator.
func (w monitorStreamSink) MonitorEventRowsAfter(afterID int64, limit int) ([]monitoring.Event, error) {
	hooks, ok := w.monitoringStore.(interface {
		MonitorEventRowsAfter(int64, int) ([]monitoring.Event, error)
	})
	if !ok {
		return nil, errMonitorStreamNoRangeRead
	}
	return hooks.MonitorEventRowsAfter(afterID, limit)
}

var errMonitorStreamNoRangeRead = errors.New("monitoring store cannot read event rows by id")

// monitorEventRows is the by-id read shared by the resume path and the insert
// payload; a store without it falls back to MonitorEventByID for single rows.
func monitorEventRows(store monitoringStore, afterID int64, limit int) ([]monitoring.Event, error) {
	if hooks, ok := store.(interface {
		MonitorEventRowsAfter(int64, int) ([]monitoring.Event, error)
	}); ok {
		return hooks.MonitorEventRowsAfter(afterID, limit)
	}
	return nil, errMonitorStreamNoRangeRead
}

// streamAlertReason names what happened to the alert row: firing, escalating
// to a higher severity, resolved, acknowledged, ignored.
func streamAlertReason(before monitoring.AlertState, existed bool, after monitoring.AlertState) string {
	switch {
	case !existed || before.Status != after.Status:
		// New row, or the incident started / resolved.
		if after.Status == monitoring.StatusResolved {
			return monitoring.StatusResolved
		}
		return "firing"
	case after.Status == monitoring.StatusFiring && severityRank(after.Severity) > severityRank(before.Severity):
		return "escalated"
	case after.Status == monitoring.StatusFiring && before.Disposition != after.Disposition:
		return after.Disposition
	default:
		// Repeat evaluations of an open incident would otherwise wake every
		// console once a minute for no visible change.
		return ""
	}
}

func severityRank(severity monitoring.Severity) int {
	switch severity {
	case monitoring.SeverityP0:
		return 3
	case monitoring.SeverityP1:
		return 2
	case monitoring.SeverityP2:
		return 1
	default:
		return 0
	}
}

// EnableMonitorStream installs the publisher on the current monitoring store.
// StartMonitoring calls it; idempotent, so a test that drives the publisher by
// hand can call it too.
// EnableMonitorStream 装上广播装饰器 + 建 hub。两条初始化路径（这里和 monitorStreamHub）
// 必须用同一个 hub 实例：早先这里直接写 s.monitorHub、那边走 sync.Once，
// 并发调用时会各建一个——发布方推给 A，订阅方挂在 B，探测帧永远到不了前端。
func (s *Server) EnableMonitorStream() {
	if s == nil || s.monitoringStore == nil {
		return
	}
	_ = s.monitorStreamHub()
	if _, wrapped := s.monitoringStore.(monitorStreamSink); !wrapped {
		s.monitoringStore = monitorStreamSink{monitoringStore: s.monitoringStore, server: s}
	}
}

func (s *Server) monitorStreamHub() *monitorStreamHub {
	s.monitorHubOnce.Do(func() {
		if s.monitorHub == nil {
			s.monitorHub = newMonitorStreamHub()
		}
	})
	return s.monitorHub
}

func (s *Server) publishStream(envelope monitorStreamEnvelope) {
	if s == nil {
		return
	}
	s.monitorStreamHub().publish(envelope)
}

// publishMonitorEvents re-reads the row the insert just produced by id, so the
// payload is exactly what GET /__monitor/events returns for that row.
func (s *Server) publishMonitorEvents(id int64) {
	if id <= 0 {
		return
	}
	var rows []monitoring.Event
	rows, _ = monitorEventRows(s.monitoringStore, id-1, 1)
	if len(rows) == 0 {
		if event, found, err := s.monitoringStore.MonitorEventByID(id); err == nil && found {
			rows = []monitoring.Event{event}
		}
	}
	if len(rows) == 0 {
		return
	}
	s.publishStream(monitorStreamEnvelope{Event: monitorStreamEvents, ID: id, Data: mustStreamJSON(rows)})
}

// publishMonitorProbes announces one probe round, grouped per provider. A
// round is several rows sharing one timestamp; probe rows carry no id on the
// console's read path, so the resume cursor is that timestamp in milliseconds.
func (s *Server) publishMonitorProbes(records []monitoring.ProbeRunRecord) {
	// Grouped per provider and split off into a local copy: the slice is the
	// caller's (InsertMonitorProbeRuns' argument) and each frame outlives it.
	grouped := map[string][]monitoring.ProbeRunRecord{}
	order := make([]string, 0, len(records))
	for _, record := range records {
		if _, seen := grouped[record.ProviderID]; !seen {
			order = append(order, record.ProviderID)
		}
		grouped[record.ProviderID] = append(grouped[record.ProviderID], record)
	}
	for _, providerID := range order {
		runs := grouped[providerID]
		s.publishStream(monitorStreamEnvelope{
			Event: monitorStreamProbe, ID: runs[0].Time.UTC().UnixMilli(),
			Data: mustStreamJSON(map[string]any{"providerId": providerID, "runs": runs}),
		})
	}
}

func (s *Server) publishMonitorAlert(reason string, alertID int64) {
	if alertID <= 0 {
		return
	}
	payload := map[string]any{"id": alertID, "reason": reason}
	if state, found, err := s.monitoringStore.MonitorAlertByID(alertID); err == nil && found {
		// Same row shape GET /__monitor/alerts lists under "alert".
		payload["alert"] = state
	}
	s.publishStream(monitorStreamEnvelope{Event: monitorStreamAlert, ID: alertID, Data: mustStreamJSON(payload)})
}

// publishMonitorTraffic emits one ephemeral frame per (throttled) real request.
// Nothing here is persisted: the frame exists so the console's live tail moves
// with actual gateway traffic instead of waiting for the next probe round.
// Bodies and headers never enter the payload — provider/model/status/timing
// only, all of which the request log already shows to the same audience.
func (s *Server) publishMonitorTraffic(entry monitor.RequestLog) {
	if s == nil {
		return
	}
	if s.monitorStreamHub().subscribers() == 0 {
		return
	}
	now := time.Now()
	s.monitorTrafficMu.Lock()
	if !s.monitorTrafficAt.IsZero() && now.Sub(s.monitorTrafficAt) < monitorTrafficInterval {
		s.monitorTrafficMu.Unlock()
		return
	}
	s.monitorTrafficAt = now
	s.monitorTrafficMu.Unlock()

	payload := map[string]any{
		"time":         entry.Time.UTC().Format(time.RFC3339Nano),
		"providerId":   entry.ProviderID,
		"model":        entry.Model,
		"status":       entry.Status,
		"latencyMs":    entry.LatencyMillis,
		"ttftMs":       entry.TTFTMillis,
		"inputTokens":  entry.InputTokens,
		"outputTokens": entry.OutputTokens,
		"costMicros":   entry.CostMicros,
		"priced":       entry.Priced,
	}
	s.publishStream(monitorStreamEnvelope{Event: monitorStreamTraffic, Data: mustStreamJSON(payload)})
}

func mustStreamJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return data
}

// ── snapshot ────────────────────────────────────────────────────────────

type monitorStreamSnapshot struct {
	GeneratedAt string                     `json:"generatedAt"`
	Overview    monitorOverviewView        `json:"overview"`
	Accounts    []monitorAccountView       `json:"accounts"`
	Alerts      []monitoring.AlertView     `json:"alerts"`
	Probes      monitorStreamProbeSnapshot `json:"probes"`
	// EventCursor is the resume point this snapshot was built against, so the
	// snapshot frame carries the same id. A client that loses the connection
	// resumes from here and gets exactly the rows the snapshot does not have.
	EventCursor int64 `json:"eventCursor"`
}

type monitorStreamProbeSnapshot struct {
	Accounts []monitorProbeAccount       `json:"accounts"`
	Runs     []monitoring.ProbeRunRecord `json:"runs"`
	Config   monitoring.ProbeConfig      `json:"config"`
}

func (s *Server) buildMonitorStreamSnapshot() (monitorStreamSnapshot, error) {
	snapshot := monitorStreamSnapshot{GeneratedAt: time.Now().UTC().Format(time.RFC3339)}
	// Taken first: it is also the resume cursor, and an event landing while
	// the sections are built must fall after it, not vanish between them.
	if events, err := s.monitoringStore.QueryMonitorEvents(monitoring.EventQuery{Page: 1, PageSize: 1}); err == nil && len(events.Items) > 0 {
		snapshot.EventCursor = events.Items[0].ID
	}
	// Build from a frozen copy of the provider list: the overview, the account
	// rows and the probe rows must describe the same set of accounts, and a
	// concurrent provider edit must not make them disagree.
	providers := s.monitorProviders()
	overview, err := s.monitorOverviewOf(providers)
	if err != nil {
		return snapshot, err
	}
	snapshot.Overview = overview
	snapshot.Accounts = s.monitorAccountViewsOf(providers)

	alerts, err := s.monitoringStore.QueryMonitorAlerts(monitoring.AlertQuery{
		Status: monitoring.StatusFiring, Page: 1, PageSize: monitorStreamSnapshotLimit,
	})
	if err != nil {
		return snapshot, err
	}
	snapshot.Alerts = alerts.Items
	if snapshot.Alerts == nil {
		snapshot.Alerts = []monitoring.AlertView{}
	}

	probes, err := s.monitorProbeSnapshotOf(providers)
	if err != nil {
		return snapshot, err
	}
	snapshot.Probes = probes
	return snapshot, nil
}

// monitorProbeSnapshot is the body of GET /__monitor/probes, shared by that
// handler and the stream snapshot.
func (s *Server) monitorProbeSnapshotOf(providers []domain.Provider) (monitorStreamProbeSnapshot, error) {
	snapshot := monitorStreamProbeSnapshot{
		Accounts: []monitorProbeAccount{}, Runs: []monitoring.ProbeRunRecord{},
		Config: monitoring.NewConfig(s.monitoringStore).Probe(),
	}
	day := monitorProbeDay(s, time.Now().UTC())
	for _, provider := range providers {
		if provider.Deleted {
			continue
		}
		row := monitorProbeAccount{
			ProviderID: provider.ID, Name: provider.Name, Vendor: providerVendorKey(provider),
			Disabled: provider.Disabled, MaxPerDay: snapshot.Config.EventDriven.MaxPerDayPerAccount,
		}
		outcome, used, err := s.monitoringStore.MonitorProbeOutcome(provider.ID, day)
		if err != nil {
			return snapshot, err
		}
		row.Probed = outcome.Known
		row.ConsecutiveFailures = outcome.Consecutive
		row.LastProbeAt = outcome.LastAt
		row.LastStatus = outcome.LastStatus
		row.Skipped = outcome.Skipped
		row.UsedToday = used
		snapshot.Accounts = append(snapshot.Accounts, row)
	}
	runs, err := s.monitoringStore.LatestMonitorProbeRuns("", monitorStreamSnapshotLimit)
	if err != nil {
		return snapshot, err
	}
	if runs != nil {
		snapshot.Runs = runs
	}
	return snapshot, nil
}

// ── handler ─────────────────────────────────────────────────────────────

// handleMonitorStream is the SSE endpoint. Wire contract:
//
//	snapshot  {generatedAt, overview, accounts, alerts, probes, eventCursor}
//	events    [Event, ...]            id = newest event row id
//	alert     {id, reason, alert?}    id = alert id
//	probe     {providerId, runs}      id = round timestamp (unix ms)
//	traffic   {time, providerId, model, status, latencyMs, ttftMs, ...}  ephemeral
//	resync    {reason, cursor}        id = event cursor the client resumes from
//	": ping"                          heartbeat comment, every sseHeartbeatInterval
func (s *Server) handleMonitorStream(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	hub := s.monitorStreamHub()
	sub, ok := hub.subscribe()
	if !ok {
		writeOpenAIError(w, http.StatusServiceUnavailable, "监控流连接数已达上限")
		return
	}
	// Every exit path — client gone, write error, panic — unregisters here.
	defer hub.unsubscribe(sub)

	// The heartbeat and the frame writer share one lock so a keep-alive can
	// never interleave with half a frame.
	stream := newSSEHeartbeatResponseWriter(w)
	header := stream.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache, no-transform")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	stream.WriteHeader(http.StatusOK)
	stream.Flush()

	stopHeartbeat := startSSEHeartbeat(stream, sseHeartbeatInterval)
	defer stopHeartbeat()

	write := func(envelope monitorStreamEnvelope) error {
		frame := formatMonitorStreamFrame(envelope)
		if _, err := stream.Write(frame); err != nil {
			return err
		}
		stream.Flush()
		return nil
	}

	// Replay the gap first, then snapshot: on connect the client ends up
	// holding the latest rows and the latest state both.
	eventCursor := monitorResumeCursor(r)
	probeCursor := int64(0)
	var err error
	if eventCursor > 0 {
		if eventCursor, err = s.replayMonitorEvents(r.Context(), eventCursor, write); err != nil {
			return
		}
	}
	snapshot, err := s.buildMonitorStreamSnapshot()
	if err != nil {
		_ = write(monitorStreamEnvelope{
			Event: monitorStreamResync, ID: eventCursor,
			Data: mustStreamJSON(map[string]any{"reason": "snapshot_failed", "cursor": eventCursor}),
		})
		return
	}
	// The resume cursor and the snapshot frame carry the same id: a client
	// that reconnects from it gets rows that are all newer than the snapshot.
	if snapshot.EventCursor > eventCursor {
		eventCursor = snapshot.EventCursor
	}
	if err := write(monitorStreamEnvelope{Event: monitorStreamSnapshotEvent, ID: eventCursor, Data: mustStreamJSON(snapshot)}); err != nil {
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case envelope := <-sub.ch:
			if dropped := hub.takeDropped(sub); dropped > 0 {
				// The queue lost frames: ids are still the truth, so read the
				// gap back from the store instead of pretending the stream
				// is whole.
				if eventCursor, err = s.replayMonitorEvents(r.Context(), eventCursor, write); err != nil {
					return
				}
				if err := write(monitorStreamEnvelope{
					Event: monitorStreamResync, ID: eventCursor,
					Data: mustStreamJSON(map[string]any{"reason": "slow_consumer", "dropped": dropped, "cursor": eventCursor}),
				}); err != nil {
					return
				}
				continue
			}
			switch envelope.Event {
			case monitorStreamEvents:
				if envelope.ID <= eventCursor {
					continue
				}
				eventCursor = envelope.ID
			case monitorStreamAlert, monitorStreamResync:
				// Alert ids are not comparable with event ids; both are
				// last-write-wins state, so they always go out.
			case monitorStreamProbe:
				// One round publishes one frame per case row, all with the
				// same round cursor: equal is not stale.
				if envelope.ID < probeCursor {
					continue
				}
				probeCursor = envelope.ID
			}
			if err := write(envelope); err != nil {
				return
			}
		}
	}
}

func (h *monitorStreamHub) takeDropped(sub *monitorStreamSubscriber) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	dropped := sub.dropped
	sub.dropped = 0
	return dropped
}

// replayMonitorEvents writes every event row newer than `after`, oldest first,
// in pages, and returns the newest id written.
func (s *Server) replayMonitorEvents(ctx context.Context, after int64, write func(monitorStreamEnvelope) error) (int64, error) {
	if after <= 0 {
		return after, nil
	}
	cursor := after
	for page := 0; page < monitorStreamCatchUpPages; page++ {
		if err := ctx.Err(); err != nil {
			return cursor, err
		}
		rows, err := monitorEventRows(s.monitoringStore, cursor, monitorStreamSnapshotLimit)
		if err != nil || len(rows) == 0 {
			return cursor, err
		}
		cursor = rows[len(rows)-1].ID
		if err := write(monitorStreamEnvelope{Event: monitorStreamEvents, ID: cursor, Data: mustStreamJSON(rows)}); err != nil {
			return cursor, err
		}
		if len(rows) < monitorStreamSnapshotLimit {
			break
		}
	}
	return cursor, nil
}

// monitorResumeCursor reads the resume point. EventSource sends Last-Event-ID
// verbatim, but anything encoded as UTF-8 (EventSource spec appendix, and some
// proxies) arrives as base64, so both forms are accepted; ?lastEventId= is the
// escape hatch for clients that cannot set headers.
func monitorResumeCursor(r *http.Request) int64 {
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("lastEventId"))
	}
	if raw == "" {
		return 0
	}
	if cursor, ok := parseMonitorCursor(raw); ok {
		return cursor
	}
	// base64OfCursor is how a browser re-sends an id per the EventSource spec;
	// the padding is optional on the wire.
	if decoded, err := base64.StdEncoding.DecodeString(raw + strings.Repeat("=", (4-len(raw)%4)%4)); err == nil {
		if cursor, ok := parseMonitorCursor(string(decoded)); ok {
			return cursor
		}
	}
	return 0
}

func parseMonitorCursor(raw string) (int64, bool) {
	cursor, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || cursor <= 0 {
		return 0, false
	}
	return cursor, true
}

// formatMonitorStreamFrame renders one SSE frame. encoding/json never emits a
// raw newline, so `data:` is always a single line.
func formatMonitorStreamFrame(envelope monitorStreamEnvelope) []byte {
	var builder strings.Builder
	if envelope.ID > 0 {
		builder.WriteString("id: ")
		builder.WriteString(strconv.FormatInt(envelope.ID, 10))
		builder.WriteString("\n")
	}
	builder.WriteString("event: ")
	builder.WriteString(envelope.Event)
	builder.WriteString("\ndata: ")
	builder.Write(envelope.Data)
	builder.WriteString("\n\n")
	return []byte(builder.String())
}
