package console

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/state"
	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/ui"
)

// The console is tested as a pure function of its messages: a fake Controller
// stands in for the supervisor, [Model.Update] is driven with synthetic
// messages, and the assertions are on model state and on the frame [Model.View]
// produces. No terminal is opened and no process is spawned.

// fakeTail records one Tail call and stays alive until its context is
// cancelled, exactly like a real following tail.
type fakeTail struct {
	svc  string
	out  chan<- string
	done chan struct{} // closed when Tail returns
}

// cancelled reports whether this tail has returned.
func (t *fakeTail) cancelled() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// opCall records one start/stop/restart request.
type opCall struct {
	kind  opKind
	names []string
}

// fakeCtrl is a Controller that records what it was asked to do.
type fakeCtrl struct {
	mu       sync.Mutex
	statuses []supervisor.Status
	tails    []*fakeTail
	calls    []opCall

	// events is emitted by every operation before it returns.
	events []supervisor.Event
	// opErr is returned by every operation.
	opErr error
	// block, when non-nil, holds every operation until it is closed.
	block chan struct{}
	// tailErr, when non-nil, is returned by Tail immediately.
	tailErr error
}

func (f *fakeCtrl) Status(context.Context) []supervisor.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statuses
}

// Tail mirrors the real supervisor's contract exactly, including the part that
// matters most to the console: Tail closes out when it returns, so the console
// must never close it itself.
func (f *fakeCtrl) Tail(ctx context.Context, svc string, _ int, _ bool, out chan<- string) error {
	defer close(out)

	f.mu.Lock()
	if f.tailErr != nil {
		err := f.tailErr
		f.mu.Unlock()
		return err
	}
	t := &fakeTail{svc: svc, out: out, done: make(chan struct{})}
	f.tails = append(f.tails, t)
	f.mu.Unlock()

	defer close(t.done)
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeCtrl) Start(ctx context.Context, names []string, ev chan<- supervisor.Event) error {
	return f.op(ctx, opStart, names, ev)
}

func (f *fakeCtrl) Stop(ctx context.Context, names []string, ev chan<- supervisor.Event) error {
	return f.op(ctx, opStop, names, ev)
}

func (f *fakeCtrl) Restart(ctx context.Context, names []string, ev chan<- supervisor.Event) error {
	return f.op(ctx, opRestart, names, ev)
}

func (f *fakeCtrl) op(ctx context.Context, kind opKind, names []string, ev chan<- supervisor.Event) error {
	f.mu.Lock()
	f.calls = append(f.calls, opCall{kind: kind, names: names})
	events, opErr, block := f.events, f.opErr, f.block
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, e := range events {
		select {
		case ev <- e:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return opErr
}

// liveTails is the number of tails that have not yet returned. A console that
// leaks a tail per selection change shows up here immediately.
func (f *fakeCtrl) liveTails() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.tails {
		if !t.cancelled() {
			n++
		}
	}
	return n
}

// tailFor waits for a tail of svc to be registered and returns it.
func (f *fakeCtrl) tailFor(t *testing.T, svc string) *fakeTail {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for i := len(f.tails) - 1; i >= 0; i-- {
			if f.tails[i].svc == svc && !f.tails[i].cancelled() {
				tail := f.tails[i]
				f.mu.Unlock()
				return tail
			}
		}
		f.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no live tail for %q was started", svc)
	return nil
}

func (f *fakeCtrl) opCalls() []opCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]opCall(nil), f.calls...)
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// send folds one message into m and returns the new model and its command.
func send(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	mm, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want console.Model", next)
	}
	return mm, cmd
}

// runCmd executes cmd on its own goroutine and returns the message it
// produced, failing the test if it does not produce one quickly.
func runCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command, got nil")
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("command produced no message within 2s")
		return nil
	}
}

// key builds a synthetic keypress. Anything not named here is sent as runes,
// which is how bubbletea delivers ordinary characters.
func key(s string) tea.KeyMsg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// threeServices is the fixture most tests start from.
func threeServices() []supervisor.Status {
	return []supervisor.Status{
		{Name: "website", Phase: supervisor.PhaseReady, PID: 101, Port: 7100, HTTP: 200, LogPath: "/repo/.dev/logs/website.log"},
		{Name: "backend", Phase: supervisor.PhaseSlow, PID: 102, Port: 7102, Health: "http://localhost:7102/health", LogPath: "/repo/.dev/logs/backend.log"},
		{Name: "worker", Phase: supervisor.PhaseStopped, LogPath: "/repo/.dev/logs/worker.log"},
	}
}

// booted returns a model that has already received one status snapshot, with
// its first log tail running.
func booted(t *testing.T, f *fakeCtrl) Model {
	t.Helper()
	f.mu.Lock()
	if f.statuses == nil {
		f.statuses = threeServices()
	}
	sts := f.statuses
	f.mu.Unlock()

	m := New(f, Options{Root: "/repo"})
	t.Cleanup(m.Shutdown)
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 100, Height: 24})
	m, _ = send(t, m, statusMsg{statuses: sts})
	return m
}

func TestInitAsksForStatusImmediatelyAndSchedulesTheTick(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{statuses: threeServices()}
	m := New(f, Options{})
	t.Cleanup(m.Shutdown)

	batch, ok := runCmd(t, m.Init()).(tea.BatchMsg)
	if !ok {
		t.Fatal("Init did not batch the first refresh with the periodic tick")
	}
	if len(batch) != 2 {
		t.Fatalf("Init issued %d commands, want the refresh and the tick", len(batch))
	}

	// The console must be populated on its first frame, not one tick later, so
	// one of the two commands has to produce a status right away.
	msgs := make(chan tea.Msg, len(batch))
	for _, cmd := range batch {
		go func() { msgs <- cmd() }()
	}
	select {
	case msg := <-msgs:
		st, ok := msg.(statusMsg)
		if !ok {
			t.Fatalf("the first message was %T, want an immediate status", msg)
		}
		if len(st.statuses) != 3 {
			t.Errorf("status carried %d services, want 3", len(st.statuses))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Init produced no status within 2s")
	}
}

func TestSelectionMovesWithArrowsAndVimKeys(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{}
	m := booted(t, f)

	if got := m.SelectedService(); got != "website" {
		t.Fatalf("initial selection = %q, want website", got)
	}

	for _, tc := range []struct {
		keys []string
		want string
	}{
		{[]string{"down"}, "backend"},
		{[]string{"down", "down"}, "worker"},
		{[]string{"down", "down", "down"}, "worker"}, // clamped at the end
		{[]string{"j", "j", "k"}, "backend"},
		{[]string{"k"}, "website"}, // clamped at the start
	} {
		mm := booted(t, f)
		for _, k := range tc.keys {
			mm, _ = send(t, mm, key(k))
		}
		if got := mm.SelectedService(); got != tc.want {
			t.Errorf("after %v: selection = %q, want %q", tc.keys, got, tc.want)
		}
	}
}

func TestSelectionChangeCancelsThePreviousTail(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{}
	m := booted(t, f)

	first := f.tailFor(t, "website")
	if n := f.liveTails(); n != 1 {
		t.Fatalf("live tails after boot = %d, want 1", n)
	}

	// Every selection change must hand the log pane over, not stack another
	// follower behind it. A leaked tail per keypress is the bug this asserts.
	for _, want := range []string{"backend", "worker"} {
		m, _ = send(t, m, key("down"))
		if got := m.SelectedService(); got != want {
			t.Fatalf("selection = %q, want %q", got, want)
		}
		f.tailFor(t, want)
		waitFor(t, "exactly one live tail", func() bool { return f.liveTails() == 1 })
	}

	waitFor(t, "the first tail to be cancelled", first.cancelled)

	m.Shutdown()
	waitFor(t, "every tail to stop after Shutdown", func() bool { return f.liveTails() == 0 })
}

func TestTailLinesReachTheLogPaneAndStaleOnesDoNot(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{}
	m := booted(t, f)

	tail := f.tailFor(t, "website")
	tail.out <- "website line one"

	// One arm of the reader delivers one line.
	m, cmd := send(t, m, statusMsg{statuses: f.statuses}) // no-op refresh; keeps the tail
	_ = cmd
	msg := runCmd(t, m.tail.next())
	m, _ = send(t, m, msg)

	if got := strings.Join(m.lines, "\n"); !strings.Contains(got, "website line one") {
		t.Fatalf("log buffer = %q, want it to contain the tailed line", got)
	}

	// A line tagged with a superseded session is dropped rather than shown
	// under the wrong service.
	stale := &tailSession{svc: "gone", lines: make(chan string, 1), done: make(chan error, 1), cancel: func() {}}
	before := len(m.lines)
	m, cmd = send(t, m, tailLineMsg{sess: stale, line: "output from the previous selection"})
	if cmd != nil {
		t.Error("a stale tail line re-armed its reader; the drain goroutine owns it")
	}
	if len(m.lines) != before {
		t.Errorf("stale line was appended: %v", m.lines)
	}
}

func TestFilterAppliesToTheLogPane(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{}
	m := booted(t, f)
	m.lines = []string{"listening on 7100", "ERROR: boom", "still fine"}

	m, _ = send(t, m, key("/"))
	if !m.filtering {
		t.Fatal("`/` did not open the filter editor")
	}
	// While the editor is open, ordinary command letters are literal text.
	for _, r := range []string{"e", "r", "r"} {
		m, _ = send(t, m, key(r))
	}
	if m.Filter() != "" {
		t.Errorf("filter applied before enter: %q", m.Filter())
	}
	if got := f.opCalls(); len(got) != 0 {
		t.Errorf("typing in the filter ran operations: %v", got)
	}

	m, _ = send(t, m, key("enter"))
	if got := m.Filter(); got != "err" {
		t.Fatalf("filter = %q, want err", got)
	}
	if got := m.visibleLines(); len(got) != 1 || got[0] != "ERROR: boom" {
		t.Fatalf("visible lines = %v, want the matching line only (match is case-insensitive)", got)
	}

	view := m.View()
	if !strings.Contains(view, "ERROR: boom") || strings.Contains(view, "still fine") {
		t.Errorf("view did not honour the filter:\n%s", view)
	}

	// Backspace edits the draft, escape abandons it and keeps the old filter.
	m, _ = send(t, m, key("/"))
	m, _ = send(t, m, key("x"))
	m, _ = send(t, m, key("backspace"))
	m, _ = send(t, m, key("esc"))
	if m.filtering {
		t.Error("esc did not close the filter editor")
	}
	if got := m.Filter(); got != "err" {
		t.Errorf("esc changed the applied filter to %q", got)
	}

	// An empty filter clears filtering altogether.
	m, _ = send(t, m, key("/"))
	m, _ = send(t, m, key("backspace"))
	m, _ = send(t, m, key("backspace"))
	m, _ = send(t, m, key("backspace"))
	m, _ = send(t, m, key("enter"))
	if got := m.Filter(); got != "" {
		t.Errorf("filter = %q, want it cleared", got)
	}
	if got := len(m.visibleLines()); got != 3 {
		t.Errorf("visible lines = %d, want all 3 back", got)
	}
}

func TestQuitSetsTheFlagAndReleasesTheTail(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"q", "ctrl+c", "esc"} {
		f := &fakeCtrl{}
		m := booted(t, f)
		f.tailFor(t, "website")

		m, cmd := send(t, m, key(k))
		if !m.Quitting() {
			t.Fatalf("%q did not set the quit flag", k)
		}
		if msg := runCmd(t, cmd); msg != tea.Quit() {
			t.Errorf("%q returned %#v, want tea.Quit", k, msg)
		}
		waitFor(t, "the tail to stop on quit", func() bool { return f.liveTails() == 0 })

		// Quitting closes a window. It must never have asked the supervisor to
		// stop anything: the services are detached and outlive the console.
		if got := f.opCalls(); len(got) != 0 {
			t.Errorf("%q asked the supervisor for %v; quitting must not stop services", k, got)
		}
	}
}

func TestEscapeLeavesTheLogPaneInsteadOfQuitting(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{}
	m := booted(t, f)

	m, _ = send(t, m, key("tab"))
	if m.focus != focusLog {
		t.Fatal("tab did not focus the log pane")
	}
	m, cmd := send(t, m, key("esc"))
	if m.Quitting() {
		t.Fatal("esc quit while the log pane had focus")
	}
	if cmd != nil {
		t.Errorf("esc returned a command: %#v", cmd)
	}
	if m.focus != focusList {
		t.Error("esc did not return focus to the service list")
	}
}

func TestOperationKeysCallTheSupervisorWithoutBlocking(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		key   string
		kind  opKind
		names []string
	}{
		{"s", opStart, []string{"website"}},
		{"x", opStop, []string{"website"}},
		{"r", opRestart, []string{"website"}},
		{"a", opStart, nil},
		{"S", opStop, nil},
	} {
		f := &fakeCtrl{block: make(chan struct{})}
		m := booted(t, f)

		// The operation is held open for its whole life, so if Update waited
		// on it this call would never return.
		done := make(chan struct{})
		var cmd tea.Cmd
		go func() {
			m, cmd = send(t, m, key(tc.key))
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%q blocked Update while the supervisor was busy", tc.key)
		}
		if cmd == nil {
			t.Fatalf("%q returned no command", tc.key)
		}

		waitFor(t, "the operation to reach the supervisor", func() bool { return len(f.opCalls()) == 1 })
		got := f.opCalls()[0]
		if got.kind != tc.kind {
			t.Errorf("%q ran %q, want %q", tc.key, got.kind, tc.kind)
		}
		if strings.Join(got.names, ",") != strings.Join(tc.names, ",") {
			t.Errorf("%q targeted %v, want %v", tc.key, got.names, tc.names)
		}
		close(f.block)
		m.Shutdown()
	}
}

func TestOperationEventsStreamIntoTheModel(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{events: []supervisor.Event{
		{Service: "website", Phase: supervisor.PhaseRunning, Msg: "spawned pid 4242"},
		{Service: "website", Phase: supervisor.PhaseReady, Msg: "ready in 1.2s"},
	}}
	m := booted(t, f)

	m, cmd := send(t, m, key("s"))
	if _, busy := m.pending["website"]; !busy {
		t.Error("the started service was not marked busy")
	}

	// Each event lands as it happens, so a slow start is narrated rather than
	// reported once at the end.
	wantNotices := []string{"spawned pid 4242", "ready in 1.2s"}
	for _, want := range wantNotices {
		m, cmd = send(t, m, runCmd(t, cmd))
		if !strings.Contains(m.notice, want) {
			t.Fatalf("notice = %q, want it to carry %q", m.notice, want)
		}
		if !strings.Contains(m.View(), want) {
			t.Errorf("the frame does not show the event %q:\n%s", want, m.View())
		}
	}

	// The final message closes the operation and asks for a fresh status.
	m, cmd = send(t, m, runCmd(t, cmd))
	if _, busy := m.pending["website"]; busy {
		t.Error("the service stayed marked busy after the operation finished")
	}
	if m.activeOps != 0 {
		t.Errorf("activeOps = %d, want 0", m.activeOps)
	}
	if cmd == nil {
		t.Error("a finished operation did not trigger a status refresh")
	}
}

func TestOperationErrorSurfaces(t *testing.T) {
	t.Parallel()
	boom := errors.New("port 7100 held by pid 812 (node)")
	f := &fakeCtrl{opErr: boom}
	m := booted(t, f)

	m, cmd := send(t, m, key("s"))
	m, _ = send(t, m, runCmd(t, cmd))

	if !errors.Is(m.err, boom) {
		t.Fatalf("model error = %v, want it to wrap the supervisor error", m.err)
	}
	if !strings.Contains(m.View(), "held by pid 812") {
		t.Errorf("the error is not visible in the frame:\n%s", m.View())
	}
}

func TestViewIsNonEmptyAndFitsTheTerminal(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{}
	m := booted(t, f)
	m.lines = []string{"a log line that is quite a lot longer than a narrow terminal is wide"}

	for _, size := range []tea.WindowSizeMsg{
		{Width: 20, Height: 6},
		{Width: 40, Height: 10},
		{Width: 100, Height: 24},
		{Width: 200, Height: 60},
	} {
		mm, _ := send(t, m, size)
		view := mm.View()
		if strings.TrimSpace(view) == "" {
			t.Fatalf("view at %dx%d is empty", size.Width, size.Height)
		}
		lines := strings.Split(view, "\n")
		if len(lines) > size.Height {
			t.Errorf("view at %dx%d has %d lines, want at most %d", size.Width, size.Height, len(lines), size.Height)
		}
		for i, l := range lines {
			if w := lipgloss.Width(l); w > size.Width {
				t.Errorf("view at %dx%d: line %d is %d columns wide: %q", size.Width, size.Height, i, w, l)
			}
		}
		if !strings.Contains(view, "website") {
			t.Errorf("view at %dx%d does not name the selected service:\n%s", size.Width, size.Height, view)
		}
	}
}

func TestViewShowsRootPhaseAndHints(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{}
	m := booted(t, f)
	view := m.View()

	for _, want := range []string{"/repo", "ready", "slow", "stopped", ":7100", "pid 101", "q quit"} {
		if !strings.Contains(view, want) {
			t.Errorf("view does not contain %q:\n%s", want, view)
		}
	}
}

func TestHelpOverlaySaysQuittingDoesNotStopServices(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{}
	m := booted(t, f)

	m, _ = send(t, m, key("?"))
	if !m.help {
		t.Fatal("`?` did not open the help overlay")
	}
	view := m.View()
	if !strings.Contains(view, "Quitting does NOT stop the supervised services") {
		t.Errorf("help does not state that quitting leaves services running:\n%s", view)
	}
	for _, want := range []string{"start the selected service", "filter log lines", "setsid"} {
		if !strings.Contains(view, want) {
			t.Errorf("help does not mention %q:\n%s", want, view)
		}
	}

	m, _ = send(t, m, key("j"))
	if m.help {
		t.Error("a key did not dismiss the help overlay")
	}

	// q still quits from the overlay.
	m, _ = send(t, m, key("?"))
	m, _ = send(t, m, key("q"))
	if !m.Quitting() {
		t.Error("q did not quit from the help overlay")
	}
}

func TestScrollingStopsAndResumesFollow(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{}
	m := booted(t, f)
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 80, Height: 12})
	for i := range 200 {
		m.appendLine(strings.Repeat("x", 3) + string(rune('a'+i%26)))
	}

	if !m.follow || m.offset != m.bottomOffset() {
		t.Fatalf("following broke while lines arrived: follow=%v offset=%d bottom=%d", m.follow, m.offset, m.bottomOffset())
	}

	m, _ = send(t, m, key("g"))
	if m.follow || m.offset != 0 {
		t.Errorf("g did not pin the pane to the top: follow=%v offset=%d", m.follow, m.offset)
	}

	m, _ = send(t, m, key("tab"))
	m, _ = send(t, m, key("down"))
	if m.offset != 1 {
		t.Errorf("scrolling the focused log pane moved to %d, want 1", m.offset)
	}
	if m.SelectedService() != "website" {
		t.Error("scrolling the log pane moved the service selection")
	}

	m, _ = send(t, m, key("G"))
	if !m.follow || m.offset != m.bottomOffset() {
		t.Errorf("G did not resume following: follow=%v offset=%d", m.follow, m.offset)
	}

	m, _ = send(t, m, key("pgup"))
	if m.follow {
		t.Error("paging up left the pane following")
	}
	m, _ = send(t, m, key("pgdown"))
	if !m.follow {
		t.Error("paging back to the bottom did not resume following")
	}
}

func TestLogBufferIsCapped(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{}
	m := booted(t, f)
	for i := range maxLogLines + 500 {
		m.appendLine(strings.Repeat("l", 4) + string(rune('a'+i%26)))
	}
	if len(m.lines) != maxLogLines {
		t.Errorf("log buffer holds %d lines, want it capped at %d", len(m.lines), maxLogLines)
	}
	if m.offset > m.bottomOffset() {
		t.Errorf("offset %d escaped the trimmed buffer (bottom %d)", m.offset, m.bottomOffset())
	}
}

func TestWindowResizeGivesTheLogPaneTheRemainingHeight(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{}
	m := booted(t, f)

	for _, size := range []tea.WindowSizeMsg{{Width: 80, Height: 24}, {Width: 80, Height: 40}, {Width: 80, Height: 8}} {
		mm, _ := send(t, m, size)
		want := size.Height - chromeRows - mm.listHeight()
		if got := mm.logHeight(); got != max(1, want) {
			t.Errorf("at height %d: log pane is %d rows, want %d", size.Height, got, max(1, want))
		}
	}

	// A degenerate size must not produce a negative or zero-height pane.
	mm, _ := send(t, m, tea.WindowSizeMsg{Width: 0, Height: 0})
	if mm.width != defaultWidth || mm.height != defaultHeight {
		t.Errorf("a zero size was adopted verbatim: %dx%d", mm.width, mm.height)
	}
}

func TestStatusRefreshKeepsTheCursorOnTheSameService(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{}
	m := booted(t, f)
	m, _ = send(t, m, key("j"))
	if m.SelectedService() != "backend" {
		t.Fatalf("selection = %q, want backend", m.SelectedService())
	}

	// A refresh that reorders nothing keeps the cursor where it was.
	m, _ = send(t, m, statusMsg{statuses: threeServices()})
	if got := m.SelectedService(); got != "backend" {
		t.Errorf("selection after refresh = %q, want backend", got)
	}

	// A refresh that drops the selected service falls back to the first.
	m, _ = send(t, m, statusMsg{statuses: threeServices()[:1]})
	if got := m.SelectedService(); got != "website" {
		t.Errorf("selection after the service disappeared = %q, want website", got)
	}

	// An empty config selects nothing and renders rather than panicking.
	m, _ = send(t, m, statusMsg{})
	if got := m.SelectedService(); got != "" {
		t.Errorf("selection with no services = %q, want empty", got)
	}
	if !strings.Contains(m.View(), "no services") {
		t.Errorf("empty console does not say so:\n%s", m.View())
	}
}

func TestTickRefreshesWithoutStackingCalls(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{}
	m := booted(t, f)

	m, cmd := send(t, m, tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("a tick produced no command")
	}
	if !m.refreshing {
		t.Fatal("a tick did not mark a refresh in flight")
	}
	// A second tick while the first refresh is still running must not issue
	// another one; that is how a wedged health probe becomes a goroutine leak.
	m2, _ := send(t, m, tickMsg(time.Now()))
	if !m2.refreshing {
		t.Fatal("the refresh flag was cleared by a tick")
	}
	m3, _ := send(t, m2, statusMsg{statuses: f.statuses})
	if m3.refreshing {
		t.Error("the refresh flag survived the status it was waiting for")
	}
}

func TestTailFailureIsReportedNotSwallowed(t *testing.T) {
	t.Parallel()
	boom := errors.New("no such log file")
	f := &fakeCtrl{tailErr: boom}
	m := booted(t, f)

	if m.tail == nil {
		t.Fatal("no tail session was created")
	}
	m, _ = send(t, m, runCmd(t, m.tail.next()))
	if !errors.Is(m.err, boom) {
		t.Fatalf("model error = %v, want the tail error", m.err)
	}
}

func TestDeriveRootFromLogPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		sts  []supervisor.Status
		want string
	}{
		{"from a log path", []supervisor.Status{{LogPath: "/srv/repo/.dev/logs/backend.log"}}, "/srv/repo"},
		{"skips empty paths", []supervisor.Status{{}, {LogPath: "/srv/repo/.dev/logs/web.log"}}, "/srv/repo"},
		{"nothing to derive", []supervisor.Status{{}}, ""},
		{"no statuses", nil, ""},
	} {
		if got := deriveRoot(tc.sts); got != tc.want {
			t.Errorf("%s: deriveRoot = %q, want %q", tc.name, got, tc.want)
		}
	}

	// A console told nothing about its root picks it up from the first status.
	f := &fakeCtrl{}
	m := New(f, Options{})
	t.Cleanup(m.Shutdown)
	m, _ = send(t, m, statusMsg{statuses: threeServices()})
	if !strings.Contains(m.View(), "/repo") {
		t.Errorf("the title bar did not adopt the derived root:\n%s", m.View())
	}
}

func TestSanitizeStripsTerminalControlSequences(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, in, want string }{
		{"colour", "\x1b[31mred\x1b[0m", "red"},
		{"clear screen", "\x1b[2Jgone", "gone"},
		{"cursor home", "\x1b[Hhome", "home"},
		{"osc title", "\x1b]0;title\x07text", "text"},
		{"osc st", "\x1b]0;title\x1b\\text", "text"},
		{"carriage return", "progress\rdone", "progressdone"},
		{"tab", "a\tb", "a    b"},
		{"bell", "ding\x07", "ding"},
		{"plain", "listening on :7100", "listening on :7100"},
		{"utf8 kept", "café ✓", "café ✓"},
	} {
		if got := sanitize(tc.in); got != tc.want {
			t.Errorf("%s: sanitize(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}

	// A log line that repaints the screen must not reach the frame intact.
	f := &fakeCtrl{}
	m := booted(t, f)
	m.appendLine("\x1b[2J\x1b[Hhostile output")
	if strings.Contains(m.View(), "\x1b[2J") {
		t.Error("a clear-screen sequence from a supervised process reached the frame")
	}
	if !strings.Contains(m.View(), "hostile output") {
		t.Error("sanitising dropped the visible text as well as the escape")
	}
}

func TestParseColorVocabulary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want lipgloss.Color
		ok   bool
	}{
		{"green", lipgloss.Color("2"), true},
		{"GREEN", lipgloss.Color("2"), true},
		{"bright-green", lipgloss.Color("10"), true},
		{"grey", lipgloss.Color("8"), true},
		{"213", lipgloss.Color("213"), true},
		{"#ff8800", lipgloss.Color("#ff8800"), true},
		{"", "", false},
		{"chartreuse", "", false},
		{"#zzzzzz", "", false},
		{"999", "", false},
	} {
		got, ok := parseColor(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseColor(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRunRejectsANilSupervisor(t *testing.T) {
	t.Parallel()
	if err := Run(nil); !errors.Is(err, ErrNoSupervisor) {
		t.Errorf("Run(nil) = %v, want ErrNoSupervisor", err)
	}
	if err := RunWith(nil, Options{}); !errors.Is(err, ErrNoSupervisor) {
		t.Errorf("RunWith(nil, …) = %v, want ErrNoSupervisor", err)
	}
}

func TestShutdownIsIdempotentAndSafeOnAFreshModel(t *testing.T) {
	t.Parallel()
	m := New(&fakeCtrl{}, Options{})
	m.Shutdown()
	m.Shutdown()

	f := &fakeCtrl{}
	m2 := booted(t, f)
	f.tailFor(t, "website")
	m2.Shutdown()
	m2.Shutdown()
	waitFor(t, "every tail to stop", func() bool { return f.liveTails() == 0 })

	// After shutdown, a late command must not resurrect a follower. The updated
	// model is discarded on purpose: the assertion is about the side effect on
	// the controller, not about what the model became.
	_, _ = send(t, m2, key("j"))
	waitFor(t, "no tail to be adopted after shutdown", func() bool { return f.liveTails() == 0 })
}

// TestConsoleFollowsARealSupervisorTail exercises the console's log pane
// against the real supervisor rather than the fake, because the two sides of
// the tail channel are owned by different packages: the supervisor closes it
// when it returns, and a console that also closed it would panic the process
// with the terminal in raw mode. Only a real Tail can prove that agreement
// holds. It spawns no processes; it only reads a log file.
func TestConsoleFollowsARealSupervisorTail(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	st, err := state.New(root)
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	log, err := st.TruncateLog("web")
	if err != nil {
		t.Fatalf("TruncateLog: %v", err)
	}
	if _, err := log.WriteString("listening on http://localhost:7100\n"); err != nil {
		t.Fatalf("writing the log: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}

	sup := supervisor.New(&config.Config{Root: root}, st,
		[]service.Instance{{Name: "web", Dir: root, Port: 7100, Color: "green"}})

	// The plain Run path learns the declared colours from the supervisor.
	if got := serviceColors(sup); got["web"] != "green" {
		t.Errorf("serviceColors = %v, want web declared green", got)
	}

	m := New(sup, Options{Root: root})
	t.Cleanup(m.Shutdown)
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 80, Height: 20})
	m, _ = send(t, m, statusMsg{statuses: []supervisor.Status{
		{Name: "web", Phase: supervisor.PhaseStopped, Port: 7100, LogPath: st.LogPath("web")},
	}})
	if m.tail == nil {
		t.Fatal("no tail session was started for the selected service")
	}

	msg := runCmd(t, m.tail.next())
	if _, ok := msg.(tailLineMsg); !ok {
		t.Fatalf("first tail message is %#v, want a log line", msg)
	}
	m, _ = send(t, m, msg)
	if !strings.Contains(m.View(), "listening on http://localhost:7100") {
		t.Errorf("the real log line did not reach the pane:\n%s", m.View())
	}

	// Shutdown cancels the tail's context; the supervisor returns and closes
	// the channel, which the console observes rather than duplicates.
	m.Shutdown()
	if _, ok := runCmd(t, m.tail.next()).(tailClosedMsg); !ok {
		t.Error("the cancelled tail did not report itself closed")
	}
}

func TestEventLineRendersEveryPart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ev   supervisor.Event
		want string
	}{
		{"full", supervisor.Event{Service: "backend", Phase: supervisor.PhaseReady, Msg: "up"}, "backend: ready: up"},
		{"no service", supervisor.Event{Phase: supervisor.PhaseStopped, Msg: "all stopped"}, "stopped: all stopped"},
		{"error", supervisor.Event{Service: "worker", Err: errors.New("boom")}, "worker: error: boom"},
		{"empty", supervisor.Event{}, ""},
	} {
		if got := eventLine(tc.ev); got != tc.want {
			t.Errorf("%s: eventLine = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestEveryPhaseRendersInTheServiceList checks the TUI draws every phase the
// supervisor can report, glyph and word both.
//
// This front end is the one that had no workaround: while the web console was
// relabelling "stopped" back to "failed" on the client, the TUI showed the
// server's answer verbatim, so the two disagreed about the same service at the
// same instant. The server answers the question now, and the point of that is
// that all three front ends show the same answer — which requires this one to
// have a row for every phase.
func TestEveryPhaseRendersInTheServiceList(t *testing.T) {
	t.Parallel()

	phases := supervisor.Phases()
	sts := make([]supervisor.Status, 0, len(phases))
	for _, p := range phases {
		sts = append(sts, supervisor.Status{
			Name:    string(p) + "-svc",
			Phase:   p,
			LogPath: "/repo/.dev/logs/x.log",
		})
	}

	f := &fakeCtrl{statuses: sts}
	m := booted(t, f)
	m.height = 40 // room for every row
	view := m.View()

	for _, p := range phases {
		glyph, word := ui.PhaseLabel(p)
		if !strings.Contains(view, glyph) {
			t.Errorf("the list has no glyph %q for phase %q:\n%s", glyph, p, view)
		}
		if !strings.Contains(view, word) {
			t.Errorf("the list has no word %q for phase %q:\n%s", word, p, view)
		}
	}
}

// TestUpCountsProcessesThatExist checks the title bar's "N/M up". A degraded
// service is one mabo-ctl would still have to stop, so it counts; a service that
// exited has no process behind it, so it does not.
func TestUpCountsProcessesThatExist(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{statuses: []supervisor.Status{
		{Name: "a", Phase: supervisor.PhaseReady},
		{Name: "b", Phase: supervisor.PhaseDegraded},
		{Name: "c", Phase: supervisor.PhaseExited},
		{Name: "d", Phase: supervisor.PhaseStopped},
	}}
	m := booted(t, f)
	if view := m.View(); !strings.Contains(view, "2/4 up") {
		t.Fatalf("title does not report 2/4 up:\n%s", view)
	}
}
