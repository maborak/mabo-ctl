package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// deadlineRecorder records every write deadline the SSE writer sets. Embedding
// the real recorder keeps Write, Flush and Header behaviour intact; only the
// deadline method is ours.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func newDeadlineRecorder() *deadlineRecorder {
	return &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (r *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	r.deadlines = append(r.deadlines, deadline)
	return nil
}

func TestSSEWriteArmsAndClearsAWriteDeadline(t *testing.T) {
	rec := newDeadlineRecorder()
	sse, ok := newSSE(rec)
	if !ok {
		t.Fatal("newSSE refused a writer that can flush")
	}
	if err := sse.comment("heartbeat"); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if len(rec.deadlines) != 2 {
		t.Fatalf("SetWriteDeadline calls = %d, want 2 (arm, clear)", len(rec.deadlines))
	}
	if rec.deadlines[0].IsZero() {
		t.Fatal("the write was not given a deadline")
	}
	if !rec.deadlines[1].IsZero() {
		t.Fatalf("the deadline was not cleared after the write: %v", rec.deadlines[1])
	}
	if d := time.Until(rec.deadlines[0]); d <= 0 || d > sseWriteTimeout+time.Second {
		t.Fatalf("deadline is %v away, want within (0, %v]", d, sseWriteTimeout)
	}
}

func TestStatusRecorderUnwrapsForResponseController(t *testing.T) {
	rec := newDeadlineRecorder()
	sr := &statusRecorder{ResponseWriter: rec}
	if err := http.NewResponseController(sr).SetWriteDeadline(time.Now()); err != nil {
		t.Fatalf("SetWriteDeadline through statusRecorder: %v", err)
	}
	if len(rec.deadlines) != 1 {
		t.Fatalf("SetWriteDeadline calls = %d, want 1", len(rec.deadlines))
	}
}
