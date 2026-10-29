package gateway

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The leading frames of a turn say only that a reply began: while they are all
// the turn has written, nothing reaches the client, so an account that fails
// behind them can still be replaced by the next one (magpie holdWriter).
func TestConverterStreamWriterHoldsTheOpeningUntilContent(t *testing.T) {
	recorder := httptest.NewRecorder()
	fw := newFailoverResponseWriter(recorder, true)
	writer := newConverterStreamWriter(fw)
	opening := "event: message_start\n\ndata: {\"type\":\"message_start\"}\n\n"
	if _, err := io.WriteString(writer, opening); err != nil {
		t.Fatalf("write: %v", err)
	}
	if writer.Committed() || fw.deferring() == false {
		t.Fatalf("the opening alone must stay held: committed=%v deferring=%v", writer.Committed(), fw.deferring())
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("the opening reached the client before any content: %q", recorder.Body.String())
	}
	// A content delta lets the whole turn go, opening included.
	content := "event: content_block_delta\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"
	if _, err := io.WriteString(writer, content); err != nil {
		t.Fatalf("write: %v", err)
	}
	writer.Finish()
	body := recorder.Body.String()
	if !strings.Contains(body, "message_start") || !strings.Contains(body, `"text":"hi"`) {
		t.Fatalf("the turn was not released whole: %q", body)
	}
}

// Nothing at all is dropped: a turn that ends without ever saying anything but
// its opening still sends that opening.
func TestConverterStreamWriterSendsAHeldOpeningAtTheEnd(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := newConverterStreamWriter(newFailoverResponseWriter(recorder, true))
	if _, err := io.WriteString(writer, "event: response.created\n\ndata: {\"type\":\"response.created\"}\n\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	logged := writer.Finish()
	if !strings.Contains(recorder.Body.String(), "response.created") {
		t.Fatalf("the held opening never reached the client: %q", recorder.Body.String())
	}
	if !strings.Contains(string(logged), "response.created") {
		t.Fatalf("the held opening is missing from the log copy: %q", logged)
	}
}

// A held turn that turns out to be an error response lets the loop take another
// account: nothing of it was sent.
func TestConverterStreamWriterHeldTurnStaysDeferrable(t *testing.T) {
	recorder := httptest.NewRecorder()
	fw := newFailoverResponseWriter(recorder, true)
	writer := newConverterStreamWriter(fw)
	if _, err := io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !fw.deferring() {
		t.Fatal("a held turn must still be deferrable")
	}
	fw.Discard()
	if got := recorder.Body.String(); got != "" {
		t.Fatalf("a discarded held turn reached the client: %q", got)
	}
}

// A refusal must never be released as the answer, whatever the clock says: the
// failover loop still has accounts to try. Qoder's queued model took 1.5–2.6s
// to say "busy", and the hold deadline used to send that refusal to the client
// instead of moving on (2026-10-29).
func TestConverterStreamWriterNeverReleasesARefusal(t *testing.T) {
	recorder := httptest.NewRecorder()
	fw := newFailoverResponseWriter(recorder, true)
	writer := newConverterStreamWriter(fw)
	// Pretend the turn has been waiting past converterHoldLimit.
	writer.holdingSince = time.Now().Add(-2 * converterHoldLimit)
	refusal := "event: error\n\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"model is busy\"}}\n\n"
	if _, err := io.WriteString(writer, refusal); err != nil {
		t.Fatalf("write: %v", err)
	}
	if writer.Committed() {
		t.Fatal("a refusal must not commit the turn")
	}
	if !fw.deferring() {
		t.Fatal("a held refusal must still be deferrable")
	}
	logged := writer.Finish()
	if strings.Contains(recorder.Body.String(), "model is busy") {
		t.Fatalf("the refusal reached the client: %q", recorder.Body.String())
	}
	if !strings.Contains(string(logged), "model is busy") {
		t.Fatalf("the refusal is missing from the log copy: %q", logged)
	}
}
