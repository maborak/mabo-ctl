package console

import (
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// focusArea is which half of the console the arrow keys drive.
type focusArea int

// The two focusable areas. The service list has focus at startup because
// selecting a service is what the console is opened to do.
const (
	focusList focusArea = iota
	focusLog
)

// Default terminal geometry, used until the first tea.WindowSizeMsg arrives
// and in tests, which never get one unless they send it. Rendering must
// produce something sensible before the size is known, or the first frame is
// blank.
const (
	defaultWidth  = 80
	defaultHeight = 24
)

// Model is the console's bubbletea model.
//
// It is a value type, copied on every Update, and every method that changes it
// returns the new copy — the usual bubbletea discipline. The one exception is
// [Model.live], a shared pointer holding the goroutine-owning sessions, which
// must be reachable from copies the event loop has moved past so that
// [Model.Shutdown] can stop them from a deferred call.
//
// Update is the only place the model changes and it never blocks: every
// supervisor call is a command running on another goroutine.
type Model struct {
	ctrl Controller
	live *live
	opt  Options

	// root is the directory shown in the title bar.
	root string

	// statuses is the last status snapshot, in declaration order, and sel is
	// the index of the selected row within it.
	statuses   []supervisor.Status
	sel        int
	refreshing bool

	width, height int

	focus focusArea
	help  bool
	quit  bool

	// tail is the session feeding the log pane, and tailSvc the service it
	// follows. They are always consistent with the selected row after Update
	// returns.
	tail    *tailSession
	tailSvc string

	// lines is the retained log buffer for the selected service, capped at
	// maxLogLines. offset is the first visible line of the filtered view, and
	// follow pins that view to the newest line.
	lines  []string
	offset int
	follow bool

	// filter selects which log lines are shown. filtering reports whether the
	// user is typing one right now, in which case filterDraft holds the
	// half-typed text and ordinary keys are literal.
	filter      string
	filtering   bool
	filterDraft string

	// notice is the one-line summary shown above the key hints — normally the
	// newest supervisor event — and err is the newest failure, which outranks
	// it. Nothing is retained that is not rendered: an event feed nobody can
	// see is a buffer that only grows.
	notice string
	err    error

	// pending marks services with an operation in flight, and activeOps counts
	// the operations themselves so the notice survives a per-service marker
	// being cleared.
	pending   map[string]opKind
	activeOps int
}

// New returns a console model driving ctrl.
//
// It starts nothing: no goroutine runs and no supervisor call is made until
// bubbletea calls [Model.Init]. That is what makes the model testable as a
// pure function of its messages.
//
// When [Options.Colors] is nil and ctrl can describe its instances, the
// colours declared in mabo-ctl.yaml are adopted, so the plain [Run] entry point
// still gives each service the colour its config asked for.
func New(ctrl Controller, opt Options) Model {
	if opt.Colors == nil && ctrl != nil {
		opt.Colors = serviceColors(ctrl)
	}
	return Model{
		ctrl:    ctrl,
		live:    newLive(),
		opt:     opt,
		root:    opt.Root,
		width:   defaultWidth,
		height:  defaultHeight,
		follow:  true,
		pending: make(map[string]opKind),
	}
}

// Init starts the periodic status refresh and asks for the first snapshot
// immediately, so the console is populated on its first frame rather than one
// tick later.
func (m Model) Init() tea.Cmd {
	return tea.Batch(refresh(m.ctrl), tick())
}

// Shutdown stops every goroutine the console owns: the log tail and any
// in-flight start, stop or restart. It is idempotent, safe to call from any
// goroutine, and safe to call on a model that never ran.
//
// It does NOT stop the supervised services. Cancelling an in-flight operation
// abandons mabo-ctl's own waiting — for readiness, or for a stop grace period —
// and signals nothing.
func (m Model) Shutdown() {
	if m.live != nil {
		m.live.shutdown()
	}
}

// Update folds one message into the model. It never blocks: work that can take
// longer than a frame is issued as a command and reported back as a later
// message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.width <= 0 {
			m.width = defaultWidth
		}
		if m.height <= 0 {
			m.height = defaultHeight
		}
		m.clampOffset()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tickMsg:
		var cmds []tea.Cmd
		if !m.refreshing {
			m.refreshing = true
			cmds = append(cmds, refresh(m.ctrl))
		}
		cmds = append(cmds, tick())
		return m, tea.Batch(cmds...)

	case statusMsg:
		return m.applyStatus(msg.statuses)

	case tailLineMsg:
		// A line from a superseded session is dropped, and its reader is NOT
		// re-armed: that session's drain goroutine owns the rest of its output.
		if msg.sess != m.tail {
			return m, nil
		}
		m.appendLine(msg.line)
		return m, msg.sess.next()

	case tailClosedMsg:
		if msg.sess != m.tail {
			return m, nil
		}
		m.tail = nil
		m.tailSvc = ""
		if msg.err != nil {
			m.err = msg.err
		}
		return m, nil

	case opEventMsg:
		m.recordEvent(msg.ev)
		return m, msg.sess.next()

	case opDoneMsg:
		return m.finishOp(msg)
	}

	return m, nil
}

// applyStatus adopts a fresh status snapshot, keeping the cursor on the same
// service where it still exists, and re-points the log tail if the selection
// now names a different service.
func (m Model) applyStatus(sts []supervisor.Status) (tea.Model, tea.Cmd) {
	prev := m.selectedName()
	m.refreshing = false
	m.statuses = sts

	m.sel = 0
	for i, st := range sts {
		if st.Name == prev {
			m.sel = i
			break
		}
	}
	if m.sel >= len(sts) {
		m.sel = max(0, len(sts)-1)
	}
	if m.root == "" {
		m.root = deriveRoot(sts)
	}
	return m.syncTailModel()
}

// syncTail makes the log pane follow the selected service, cancelling whatever
// it was following before. It returns the command that reads the new session,
// or nil when there is nothing to follow.
func (m Model) syncTail() (Model, tea.Cmd) {
	svc := m.selectedName()
	if svc == "" {
		if m.tail != nil {
			m.live.clearTail()
			m.tail, m.tailSvc = nil, ""
			m.lines, m.offset = nil, 0
		}
		return m, nil
	}
	if (m.tail != nil && m.tailSvc == svc) || m.ctrl == nil {
		return m, nil
	}

	sess := newTail(m.ctrl, svc)
	if !m.live.setTail(sess) {
		// Shutting down; the session has already been stopped for us.
		m.tail, m.tailSvc = nil, ""
		return m, nil
	}
	m.tail, m.tailSvc = sess, svc
	m.lines, m.offset = nil, 0
	m.follow = true
	return m, sess.next()
}

// syncTailModel adapts [Model.syncTail] to the (tea.Model, tea.Cmd) pair that
// Update returns.
func (m Model) syncTailModel() (tea.Model, tea.Cmd) {
	mm, cmd := m.syncTail()
	return mm, cmd
}

// selectedName is the name of the selected service, or "" when there are none.
func (m Model) selectedName() string {
	if m.sel < 0 || m.sel >= len(m.statuses) {
		return ""
	}
	return m.statuses[m.sel].Name
}

// SelectedService reports the service the cursor is on, or "" when the console
// has no services. It exists for tests and for callers reasoning about the
// model without reaching into it.
func (m Model) SelectedService() string { return m.selectedName() }

// Quitting reports whether the model has accepted a quit key and asked
// bubbletea to stop.
func (m Model) Quitting() bool { return m.quit }

// Filter reports the log filter currently applied, which is "" when no filter
// is set. Text still being typed is not part of it until it is committed.
func (m Model) Filter() string { return m.filter }

// appendLine adds one log line to the buffer, trimming the oldest lines past
// maxLogLines, and keeps the viewport pinned to the bottom while following.
func (m *Model) appendLine(line string) {
	m.lines = append(m.lines, line)
	if n := len(m.lines) - maxLogLines; n > 0 {
		m.lines = append(m.lines[:0], m.lines[n:]...)
		m.offset = max(0, m.offset-n)
	}
	if m.follow {
		m.offset = m.bottomOffset()
	} else {
		m.clampOffset()
	}
}

// recordEvent folds one supervisor event into the notice line and, when it
// carries one, into the error line. An event's error is never dropped: a
// supervisor that reported a problem the console did not show is the exact
// silent failure this tool exists to stop.
func (m *Model) recordEvent(ev supervisor.Event) {
	if line := eventLine(ev); line != "" {
		m.notice = line
	}
	if ev.Err != nil {
		m.err = ev.Err
	}
}

// finishOp clears an operation's bookkeeping and refreshes the status, since
// the world just changed and waiting a full tick to say so looks broken.
func (m Model) finishOp(msg opDoneMsg) (tea.Model, tea.Cmd) {
	m.live.removeOp(msg.sess)
	m.activeOps = max(0, m.activeOps-1)
	for _, n := range m.pendingNames(msg.sess) {
		delete(m.pending, n)
	}
	if msg.err != nil {
		m.err = msg.err
		m.notice = string(msg.sess.kind) + " failed"
	} else if m.activeOps == 0 {
		m.notice = msg.sess.label() + ": done"
	}

	if m.refreshing {
		return m, nil
	}
	m.refreshing = true
	return m, refresh(m.ctrl)
}

// pendingNames is the set of services an operation marked as busy: the ones it
// named, or every known service when it named none.
func (m Model) pendingNames(s *opSession) []string {
	if len(s.names) > 0 {
		return s.names
	}
	names := make([]string, 0, len(m.statuses))
	for _, st := range m.statuses {
		names = append(names, st.Name)
	}
	return names
}

// launch starts a supervisor operation over names (empty means every service)
// without blocking. It marks the affected services busy so the list shows the
// operation is under way before its first event arrives.
func (m Model) launch(kind opKind, names []string) (tea.Model, tea.Cmd) {
	if m.ctrl == nil {
		return m, nil
	}
	sess := newOp(m.ctrl, kind, names)
	if !m.live.addOp(sess) {
		return m, nil
	}
	m.activeOps++
	for _, n := range m.pendingNames(sess) {
		m.pending[n] = kind
	}
	m.notice = sess.label() + "…"
	m.err = nil
	return m, sess.next()
}

// launchSelected runs kind against the selected service, or does nothing when
// there is no selection.
func (m Model) launchSelected(kind opKind) (tea.Model, tea.Cmd) {
	svc := m.selectedName()
	if svc == "" {
		return m, nil
	}
	return m.launch(kind, []string{svc})
}

// handleKey dispatches one keypress. The filter editor swallows almost
// everything while it is open, because a log filter has to be able to contain
// the letters that are otherwise commands.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		return m.quitNow()
	}
	if m.filtering {
		return m.editFilter(msg)
	}
	if m.help {
		// Any key closes the overlay; a quit key still quits.
		if k := msg.String(); k == "q" {
			return m.quitNow()
		}
		m.help = false
		return m, nil
	}

	switch msg.String() {
	case "q":
		return m.quitNow()

	case "esc":
		// Escape backs out of the log pane rather than quitting, so the key
		// that cancels a filter cannot also end the session by one keystroke.
		if m.focus == focusLog {
			m.focus = focusList
			return m, nil
		}
		return m.quitNow()

	case "?":
		m.help = true
		return m, nil

	case "up", "k":
		if m.focus == focusLog {
			m.scroll(-1)
			return m, nil
		}
		return m.move(-1)

	case "down", "j":
		if m.focus == focusLog {
			m.scroll(1)
			return m, nil
		}
		return m.move(1)

	case "pgup":
		m.scroll(-m.logHeight())
		return m, nil

	case "pgdown":
		m.scroll(m.logHeight())
		return m, nil

	case "tab":
		if m.focus == focusList {
			m.focus = focusLog
		} else {
			m.focus = focusList
		}
		return m, nil

	case "l":
		m.focus = focusLog
		return m, nil

	case "h":
		m.focus = focusList
		return m, nil

	case "g":
		m.follow = false
		m.offset = 0
		return m, nil

	case "G":
		m.follow = true
		m.offset = m.bottomOffset()
		return m, nil

	case "/":
		m.filtering = true
		m.filterDraft = m.filter
		m.focus = focusLog
		return m, nil

	case "s":
		return m.launchSelected(opStart)

	case "x":
		return m.launchSelected(opStop)

	case "r":
		return m.launchSelected(opRestart)

	case "a":
		return m.launch(opStart, nil)

	case "S":
		return m.launch(opStop, nil)
	}

	return m, nil
}

// editFilter handles a keypress while the filter line is open. Enter applies
// the draft, escape abandons it, and an applied empty filter clears filtering
// altogether.
func (m Model) editFilter(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		m.filter = m.filterDraft
		m.filtering = false
		m.filterDraft = ""
		m.follow = true
		m.offset = m.bottomOffset()
		return m, nil

	case tea.KeyEsc:
		m.filtering = false
		m.filterDraft = ""
		return m, nil

	case tea.KeyBackspace:
		if r := []rune(m.filterDraft); len(r) > 0 {
			m.filterDraft = string(r[:len(r)-1])
		}
		return m, nil

	case tea.KeyRunes, tea.KeySpace:
		m.filterDraft += string(msg.Runes)
		return m, nil
	}
	return m, nil
}

// quitNow marks the model finished, releases every goroutine it owns and asks
// bubbletea to stop. The supervised services are untouched: mabo-ctl is not
// their parent and quitting the console is closing a window.
func (m Model) quitNow() (tea.Model, tea.Cmd) {
	m.quit = true
	m.Shutdown()
	m.tail, m.tailSvc = nil, ""
	return m, tea.Quit
}

// move steps the selection by delta, clamped to the list, and re-points the
// log tail when the selection actually changed.
func (m Model) move(delta int) (tea.Model, tea.Cmd) {
	if len(m.statuses) == 0 {
		return m, nil
	}
	sel := min(max(m.sel+delta, 0), len(m.statuses)-1)
	if sel == m.sel {
		return m, nil
	}
	m.sel = sel
	// The filter belonged to the log it was typed against; carrying it to the
	// next service silently hides everything that service says.
	m.filter = ""
	return m.syncTailModel()
}

// scroll moves the log viewport by delta lines. Scrolling away from the bottom
// stops following; scrolling back to the bottom resumes it, which is what a
// pager-shaped muscle memory expects.
func (m *Model) scroll(delta int) {
	if delta == 0 {
		return
	}
	m.offset += delta
	m.clampOffset()
	m.follow = m.offset >= m.bottomOffset()
}

// visibleLines is the log buffer after the filter, which is a case-insensitive
// substring match — the filter exists to find "error" in a wall of output, not
// to be a regexp engine that can fail to compile mid-keystroke.
func (m Model) visibleLines() []string {
	if m.filter == "" {
		return m.lines
	}
	needle := strings.ToLower(m.filter)
	out := make([]string, 0, len(m.lines))
	for _, l := range m.lines {
		if strings.Contains(strings.ToLower(l), needle) {
			out = append(out, l)
		}
	}
	return out
}

// bottomOffset is the offset that shows the last screenful of log.
func (m Model) bottomOffset() int {
	return max(0, len(m.visibleLines())-m.logHeight())
}

// clampOffset keeps the viewport inside the buffer.
func (m *Model) clampOffset() {
	m.offset = min(max(m.offset, 0), m.bottomOffset())
}

// deriveRoot recovers the config root from a status log path, which is
// <root>/.dev/logs/<svc>.log. It is the fallback for a caller that did not set
// [Options.Root]; it returns "" when no status carries a log path.
func deriveRoot(sts []supervisor.Status) string {
	for _, st := range sts {
		if st.LogPath == "" {
			continue
		}
		// <root>/.dev/logs/<svc>.log → up three levels.
		root := filepath.Dir(filepath.Dir(filepath.Dir(st.LogPath)))
		if root != "" && root != "." && root != string(filepath.Separator) {
			return root
		}
	}
	return ""
}
