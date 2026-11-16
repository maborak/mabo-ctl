package console

import (
	"errors"
	"strings"
	"testing"

	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// The tests in this file cover the log and list controls the console shares
// with the web console's toolbar: they are the same features, reached by key.

func TestPrettyJSONExpandsRecordsIntoRows(t *testing.T) {
	t.Parallel()
	m := booted(t, &fakeCtrl{})
	m.setLines(`{"level":"info","n":12345678901234567890,"a":[1]}`, "plain")

	if got := len(m.rows()); got != 2 {
		t.Fatalf("rows before J = %d, want 2", got)
	}
	m, _ = send(t, m, key("J"))
	got := strings.Join(m.visibleTexts(), "\n")
	want := "{\n  \"level\": \"info\",\n  \"n\": 12345678901234567890,\n  \"a\": [\n    1\n  ]\n}\nplain"
	if got != want {
		t.Errorf("pretty rows =\n%s\nwant\n%s", got, want)
	}
	if !m.follow || m.viewOffset(len(m.rows())) != m.bottomOffset() {
		t.Error("toggling pretty JSON unpinned a following pane")
	}
	if !strings.Contains(m.logHeaderView(), "pretty") {
		t.Error("the header does not say pretty JSON is on")
	}
}

func TestLevelFilterCyclesAndMatchesTheWebClassification(t *testing.T) {
	t.Parallel()
	m := booted(t, &fakeCtrl{})
	m.setLines(`{"level":"ERROR","msg":"a"}`, `{"severity":"warning"}`, `{"lvl":"debug"}`, "plain text")

	for _, want := range []struct {
		level string
		rows  int
	}{{"error", 1}, {"warn", 1}, {"info", 0}, {"debug", 1}, {"", 4}} {
		m, _ = send(t, m, key("v"))
		if m.levelFilter != want.level || len(m.rows()) != want.rows {
			t.Errorf("after v: level %q with %d rows, want %q with %d", m.levelFilter, len(m.rows()), want.level, want.rows)
		}
	}
}

func TestPauseFreezesThePaneWithinTheSameCap(t *testing.T) {
	t.Parallel()
	m := booted(t, &fakeCtrl{})
	m.setLines("one", "two")
	m, _ = send(t, m, key("p"))
	m.appendLine("three")
	if got := m.visibleTexts(); len(got) != 2 {
		t.Errorf("paused pane shows %v, want the two lines from before the pause", got)
	}
	if !strings.Contains(m.logHeaderView(), "PAUSED +1") {
		t.Errorf("header does not count the held line: %s", m.logHeaderView())
	}
	m, _ = send(t, m, key("p"))
	if got := m.visibleTexts(); len(got) != 3 {
		t.Errorf("resumed pane shows %v, want all three", got)
	}
	m, _ = send(t, m, key("c"))
	if len(m.recs) != 0 || m.recBytes != 0 {
		t.Error("c did not clear the buffer")
	}
}

func TestLogBufferIsCappedByBytes(t *testing.T) {
	t.Parallel()
	m := booted(t, &fakeCtrl{})
	line := strings.Repeat("x", 256<<10)
	for range 3 * maxLogBytes / len(line) {
		m.appendLine(line)
	}
	if m.recBytes > maxLogBytes {
		t.Errorf("buffer holds %d bytes, over the %d cap", m.recBytes, maxLogBytes)
	}
	if want := maxLogBytes / len(line); len(m.recs) != want {
		t.Errorf("buffer holds %d lines of 256 KiB, want %d", len(m.recs), want)
	}
}

func TestSeparatorsFallBetweenEntriesNotContinuations(t *testing.T) {
	t.Parallel()
	m := booted(t, &fakeCtrl{})
	m.setLines("Traceback (most recent call last):", `  File "x.py", line 1`, "next entry")
	m, _ = send(t, m, key("|"))
	seps := 0
	for _, r := range m.rows() {
		if r.sep {
			seps++
		}
	}
	if seps != 1 {
		t.Errorf("got %d separators, want one between the traceback and the next entry", seps)
	}
}

func TestMergedPaneTailsEveryServiceWithLabels(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{}
	m := booted(t, f)
	m, cmd := send(t, m, key("m"))
	if cmd == nil || m.tailSvc != allKey {
		t.Fatalf("m did not start a merged tail (key %q)", m.tailSvc)
	}
	for _, svc := range []string{"website", "backend", "worker"} {
		f.tailFor(t, svc)
	}
	f.tailFor(t, "backend").out <- "from backend"
	m, _ = send(t, m, runCmd(t, m.tail.next()))
	if len(m.recs) != 1 || m.recs[0].svc != "backend" {
		t.Fatalf("merged buffer = %+v, want one backend line", m.recs)
	}
	if !strings.Contains(m.View(), "backend · from backend") {
		t.Errorf("merged line is not labelled with its service:\n%s", m.View())
	}

	m, _ = send(t, m, key("m"))
	if m.tailSvc != "website" {
		t.Errorf("m again followed %q, want the selected service", m.tailSvc)
	}
	waitFor(t, "the merged tails to stop", func() bool { return f.liveTails() == 1 })
}

func TestRestartAllNamesOnlyRunningServices(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{block: make(chan struct{})}
	m := booted(t, f)
	_, _ = send(t, m, key("R"))
	waitFor(t, "the restart to reach the supervisor", func() bool { return len(f.opCalls()) == 1 })
	if got := f.opCalls()[0]; got.kind != opRestart || strings.Join(got.names, ",") != "website,backend" {
		t.Errorf("R ran %s %v, want restart of the two running services", got.kind, got.names)
	}
	close(f.block)

	stopped := &fakeCtrl{statuses: []supervisor.Status{{Name: "worker", Phase: supervisor.PhaseStopped}}}
	m = booted(t, stopped)
	m, cmd := send(t, m, key("R"))
	if cmd != nil || !strings.Contains(m.notice, "nothing is running") {
		t.Errorf("R with nothing running: notice %q, cmd %v", m.notice, cmd != nil)
	}
}

func TestListFiltersHideRowsAndKeepTheSelectionShown(t *testing.T) {
	t.Parallel()
	m := booted(t, &fakeCtrl{})
	for _, k := range []string{"f", "w", "o", "r", "enter"} {
		m, _ = send(t, m, key(k))
	}
	if m.SelectedService() != "worker" || len(m.listIndices()) != 1 {
		t.Errorf("name filter: selected %q, %d shown; want worker alone", m.SelectedService(), len(m.listIndices()))
	}
	if !strings.Contains(m.View(), "name: wor") {
		t.Error("the list does not show its filter")
	}

	m = booted(t, &fakeCtrl{})
	ready := 0
	for i, p := range supervisor.Phases() {
		if p == supervisor.PhaseReady {
			ready = i + 1
		}
	}
	m, _ = send(t, m, key(string(rune('0'+ready))))
	if m.phaseFilter != supervisor.PhaseReady || len(m.listIndices()) != 1 || m.SelectedService() != "website" {
		t.Errorf("phase filter: %q, %d shown, selected %q", m.phaseFilter, len(m.listIndices()), m.SelectedService())
	}
	m, _ = send(t, m, key("0"))
	if m.phaseFilter != "" || len(m.listIndices()) != 3 {
		t.Error("0 did not clear the phase filter")
	}
}

func TestDetailBlockRedactsTheHealthURL(t *testing.T) {
	t.Parallel()
	sts := threeServices()
	sts[0].Health = "http://admin:hunter2@localhost:7100/health"
	sts[0].Detail = "probe http://admin:hunter2@localhost:7100/health refused"
	m := booted(t, &fakeCtrl{statuses: sts})
	m, _ = send(t, m, key("i"))
	view := m.View()
	if strings.Contains(view, "hunter2") {
		t.Errorf("the console showed a health-URL credential:\n%s", view)
	}
	if !strings.Contains(view, "health  http://") || !strings.Contains(view, "uptime") {
		t.Errorf("the detail block is missing:\n%s", view)
	}
}

func TestOverlaysOpenAndClose(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{events: []supervisor.Event{{Service: "website", Phase: supervisor.PhaseReady, Msg: "up"}}}
	m := booted(t, f)
	m, cmd := send(t, m, key("s"))
	for cmd != nil {
		var msg = runCmd(t, cmd)
		m, cmd = send(t, m, msg)
		if _, done := msg.(opDoneMsg); done {
			break
		}
	}
	m, _ = send(t, m, key("e"))
	if v := m.View(); !strings.Contains(v, "website: ready: up") || !strings.Contains(v, "starting website: done") {
		t.Errorf("activity overlay lacks the start:\n%s", v)
	}
	m, _ = send(t, m, key("x"))
	if m.showEvents {
		t.Error("a key did not close the activity overlay")
	}

	m, _ = send(t, m, key("C"))
	if !m.showConfig || !strings.Contains(m.View(), "cannot describe its configuration") {
		t.Errorf("C on a controller without a config:\n%s", m.View())
	}
	m, _ = send(t, m, key("esc"))
	if m.showConfig {
		t.Error("esc did not close the configuration view")
	}
}

// TestPausedPaneSurvivesEviction: a paused pane is a snapshot. Output that
// keeps arriving at the buffer cap evicts records from the buffer, never from
// what the paused pane shows.
func TestPausedPaneSurvivesEviction(t *testing.T) {
	t.Parallel()
	m := booted(t, &fakeCtrl{})
	for range maxLogLines {
		m.appendLine("before")
	}
	m, _ = send(t, m, key("p"))
	for range maxLogLines + 500 {
		m.appendLine("after")
	}
	if got := len(m.rows()); got != maxLogLines {
		t.Errorf("paused pane shows %d rows after the buffer turned over, want %d", got, maxLogLines)
	}
	if len(m.recs) != maxLogLines {
		t.Errorf("buffer holds %d, want the cap %d", len(m.recs), maxLogLines)
	}
}

// BenchmarkAppendWhileFollowing guards ingest cost: a following pane must not
// rebuild its rows for every arriving line.
func BenchmarkAppendWhileFollowing(b *testing.B) {
	m := New(&fakeCtrl{}, Options{})
	for range maxLogLines {
		m.appendRecord("svc", "warm")
	}
	b.ResetTimer()
	for range b.N {
		m.appendRecord("svc", `{"level":"info","msg":"line"}`)
	}
}

// TestAFailedTailIsNotRetriedEveryTick: a tail that ended in an error stays
// ended until the selection changes, instead of being rebuilt — and the pane
// wiped — on every status refresh.
func TestAFailedTailIsNotRetriedEveryTick(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{tailErr: errors.New("permission denied")}
	m := booted(t, f)
	m, _ = send(t, m, runCmd(t, m.tail.next()))
	if m.tail != nil || m.tailFailed != "website" {
		t.Fatalf("after the failure: tail %v, failed %q", m.tail != nil, m.tailFailed)
	}
	for range 3 {
		m, _ = send(t, m, statusMsg{statuses: threeServices()})
	}
	if m.tail != nil {
		t.Error("a status refresh restarted the failed tail")
	}
	m, _ = send(t, m, key("j"))
	if m.tail == nil {
		t.Error("selecting another service did not start its tail")
	}
}
