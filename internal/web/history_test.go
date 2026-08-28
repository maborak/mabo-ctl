package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// TestConsolePageConsumesHistory pins the one frontend dependency of
// /api/history: the embedded page must actually render it. The route table
// proves the endpoint exists; this proves the page did not quietly stop
// using it.
func TestConsolePageConsumesHistory(t *testing.T) {
	t.Parallel()
	if !strings.Contains(consoleHTML, "/api/history") {
		t.Errorf("embedded console page no longer references /api/history")
	}
}

func TestHistoryServesRecordedEventsOldestFirst(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())

	s.events.publish(supervisor.Event{Service: "api", Phase: supervisor.PhaseRunning, Msg: "starting…"})
	s.events.publish(supervisor.Event{Service: "api", Phase: supervisor.PhaseReady, Msg: "ready in 1ms"})
	s.events.publish(supervisor.Event{Service: "worker", Err: errors.New("boom"), Msg: "pre_start hook failed: boom"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://"+recorderAddr+"/api/history", nil)
	s.handleHistory(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Events []eventJSON `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(body.Events))
	}
	if body.Events[0].Service != "api" || body.Events[0].Phase != "running" {
		t.Errorf("first event = %+v, want api/running", body.Events[0])
	}
	if body.Events[2].Service != "worker" || body.Events[2].Error != "boom" {
		t.Errorf("last event = %+v, want worker carrying the hook failure", body.Events[2])
	}
}

func TestHistoryRingIsBounded(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())

	const extra = 10
	for i := 0; i < historyCapacity+extra; i++ {
		s.events.publish(supervisor.Event{Service: "api", Msg: fmt.Sprintf("tick-%d", i)})
	}
	got := s.events.history()
	if len(got) != historyCapacity {
		t.Fatalf("history length = %d, want the %d cap", len(got), historyCapacity)
	}
	// The ring must keep the NEWEST events, delivered in publish order: every
	// message is distinct, so got[0] being tick-10 proves the ten oldest were
	// evicted, and got[last] being tick-59 proves the newest survived.
	if got[0].Msg != fmt.Sprintf("tick-%d", extra) {
		t.Fatalf("ring kept old events: first survivor = %q, want tick-%d", got[0].Msg, extra)
	}
	if got[len(got)-1].Msg != fmt.Sprintf("tick-%d", extra+historyCapacity-1) {
		t.Fatalf("ring lost new events: last survivor = %q, want tick-%d",
			got[len(got)-1].Msg, extra+historyCapacity-1)
	}
}
