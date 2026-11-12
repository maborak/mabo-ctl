package console

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/ui"
)

// allKey is the tail key of the merged all-services pane. It cannot collide
// with a service, whose names the config restricts to [A-Za-z0-9_.-].
const allKey = "*all*"

// maxEvents caps the activity history the e overlay shows.
const maxEvents = 200

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

	// tail is the session feeding the log pane, and tailSvc what it follows:
	// a service name, or allKey in merged mode. They are always consistent
	// with the selection (or merged mode) after Update returns.
	tail    *tailSession
	tailSvc string

	// recs is the retained log buffer, capped at maxLogLines and maxLogBytes
	// (recBytes is its size). offset is the first visible ROW of the filtered,
	// expanded view, and follow pins that view to the newest row.
	recs     []*logRecord
	recBytes int
	offset   int
	follow   bool

	// Log presentation, the web console's toolbar in keys: all merges every
	// service into one pane; paused freezes the pane at pauseEnd records while
	// lines keep arriving (pausedNew counts them); levelFilter keeps one level;
	// pretty re-indents JSON lines; separators draws a rule between entries.
	all         bool
	paused      bool
	pauseEnd    int
	pausedNew   int
	levelFilter string
	pretty      bool
	separators  bool

	// showDetail opens the selected service's detail block under the list:
	// uptime, readiness, health, command, directory, dependencies and env.
	// cfgView is the redacted configuration it and the config view read,
	// rebuilt when the controller changes.
	showDetail bool
	cfgView    ui.ConfigView

	// The service list's filters: nameFilter is a substring of the name
	// (nameFiltering while it is typed into nameDraft), phaseFilter keeps one
	// phase. sel always indexes statuses; the filters only hide rows.
	nameFilter    string
	nameFiltering bool
	nameDraft     string
	phaseFilter   supervisor.Phase

	// events is the activity history, newest last, and showEvents its
	// overlay. showConfig is the configuration view, scrolled to cfgOffset.
	events     []string
	showEvents bool
	showConfig bool
	cfgOffset  int

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

	// ctrlGen counts the controllers a reload has swapped in, so a status
	// read from a controller since replaced is recognised and dropped.
	ctrlGen int

	// The configuration watch and reload. configChanged reports that
	// mabo-ctl.yaml, or an env_file it names, no longer matches the
	// configuration in force, and configChecking that a comparison is in
	// flight. confirmReload holds L's y/n question open; reload is the
	// reload in flight, and quitAfterReload a quit that arrived during it.
	// reloadErr is the last reload's failure, kept apart from err because it
	// alone is about the configuration.
	configChanged   bool
	configChecking  bool
	confirmReload   bool
	reload          *reloadSession
	quitAfterReload bool
	reloadErr       error
}

// New returns a console model driving ctrl.
//
// It starts nothing: no goroutine runs and no supervisor call is made until
// bubbletea calls [Model.Init]. That is what makes the model testable as a
// pure function of its messages.
//
// When [Options.Colors] is nil and ctrl can describe its instances, the
// colours declared in mabo-ctl.yaml are adopted, so a caller that passes no
// colours still gives each service the colour its config asked for.
func New(ctrl Controller, opt Options) Model {
	if opt.Colors == nil && ctrl != nil {
		opt.Colors = serviceColors(ctrl)
	}
	m := Model{
		ctrl:    ctrl,
		live:    newLive(),
		opt:     opt,
		root:    opt.Root,
		width:   defaultWidth,
		height:  defaultHeight,
		follow:  true,
		pending: make(map[string]opKind),
	}
	m.cfgView = m.buildConfigView()
	return m
}

// buildConfigView assembles the redacted configuration view for the detail
// block and the config view, from what the controller and the caller know.
func (m Model) buildConfigView() ui.ConfigView {
	d, ok := m.ctrl.(describer)
	if !ok {
		return ui.ConfigView{}
	}
	return ui.BuildConfigView(ui.ConfigInput{
		Config:    configOf(m.ctrl),
		Instances: d.Instances(),
		Origins:   m.opt.Origins,
		StateDir:  m.opt.StateDir,
		Explicit:  m.opt.ExplicitConfig,
	})
}

// Init starts the periodic status refresh and asks for the first snapshot
// immediately, so the console is populated on its first frame rather than one
// tick later.
func (m Model) Init() tea.Cmd {
	return tea.Batch(refresh(m.ctrl, m.ctrlGen), tick())
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
		// The list keeps refreshing through a reload: watching services stop
		// and come back is the progress report.
		if cfg := configOf(m.ctrl); m.opt.Reload != nil && cfg != nil && !m.configChecking && m.reload == nil {
			m.configChecking = true
			cmds = append(cmds, checkConfig(cfg))
		}
		if !m.refreshing {
			m.refreshing = true
			cmds = append(cmds, refresh(m.ctrl, m.ctrlGen))
		}
		cmds = append(cmds, tick())
		return m, tea.Batch(cmds...)

	case statusMsg:
		if msg.gen != m.ctrlGen {
			// Read from a controller a reload has replaced. The refresh
			// issued with the swap is in flight and clears refreshing.
			return m, nil
		}
		return m.applyStatus(msg.statuses)

	case configCheckMsg:
		m.configChecking = false
		if msg.cfg == configOf(m.ctrl) {
			m.configChanged = msg.changed
		}
		return m, nil

	case reloadEventMsg:
		if msg.sess != m.reload {
			return m, nil
		}
		m.recordEvent(msg.ev)
		return m, msg.sess.next()

	case reloadDoneMsg:
		return m.finishReload(msg)

	case tailLineMsg:
		// A line from a superseded session is dropped, and its reader is NOT
		// re-armed: that session's drain goroutine owns the rest of its output.
		if msg.sess != m.tail {
			return m, nil
		}
		m.appendRecord(msg.svc, msg.line)
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
	// The console is not the operator's private one-shot CLI: it is the screen
	// that gets shared and recorded, so a credential in a health URL is
	// withheld here exactly as the web console withholds it.
	ui.RedactStatuses(sts)
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
	m.keepSelectionVisible()
	return m.syncTailModel()
}

// syncTail makes the log pane follow the selected service, cancelling whatever
// it was following before. It returns the command that reads the new session,
// or nil when there is nothing to follow.
func (m Model) syncTail() (Model, tea.Cmd) {
	key, svcs, backlog := m.selectedName(), []string{m.selectedName()}, tailBacklog
	if m.all {
		key, svcs, backlog = allKey, nil, mergedBacklog
		for _, st := range m.statuses {
			svcs = append(svcs, st.Name)
		}
		if len(svcs) == 0 {
			key = ""
		}
	}
	if key == "" {
		if m.tail != nil {
			m.live.clearTail()
			m.tail, m.tailSvc = nil, ""
			m.clearLog()
		}
		return m, nil
	}
	if (m.tail != nil && m.tailSvc == key) || m.ctrl == nil {
		return m, nil
	}

	sess := newTail(m.ctrl, key, svcs, backlog)
	if !m.live.setTail(sess) {
		// Shutting down; the session has already been stopped for us.
		m.tail, m.tailSvc = nil, ""
		return m, nil
	}
	m.tail, m.tailSvc = sess, key
	m.clearLog()
	m.paused = false
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

// recordEvent folds one supervisor event into the notice line, the activity
// history and, when it carries one, the error line. An event's error is never
// dropped: a supervisor that reported a problem the console did not show is
// the exact silent failure this tool exists to stop.
func (m *Model) recordEvent(ev supervisor.Event) {
	if line := eventLine(ev); line != "" {
		m.notice = line
		m.addEvent(line)
	}
	if ev.Err != nil {
		m.err = ev.Err
	}
}

// addEvent appends one line to the activity history, stamped with the time
// it was seen, keeping the newest maxEvents.
func (m *Model) addEvent(line string) {
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i] + " …"
	}
	m.events = append(m.events, time.Now().Format("15:04:05")+"  "+line)
	if n := len(m.events) - maxEvents; n > 0 {
		m.events = append(m.events[:0], m.events[n:]...)
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
		m.addEvent(msg.sess.label() + " failed: " + errRenderer.Error(msg.err))
	} else {
		m.addEvent(msg.sess.label() + ": done")
		if m.activeOps == 0 {
			m.notice = msg.sess.label() + ": done"
		}
	}

	if m.refreshing {
		return m, nil
	}
	m.refreshing = true
	return m, refresh(m.ctrl, m.ctrlGen)
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
	if m.reload != nil {
		// The reload is stopping and starting services right now; a second
		// operation would race it. Say so — a key that does nothing reads as
		// a broken key.
		m.notice = "the configuration reload is still running; try again when it finishes"
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
	m.err, m.reloadErr = nil, nil
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
		return m.requestQuit()
	}
	if m.confirmReload {
		return m.answerReload(msg)
	}
	if m.filtering || m.nameFiltering {
		return m.editFilter(msg)
	}
	if m.help || m.showEvents {
		// Any key closes the overlay; a quit key still quits.
		if k := msg.String(); k == "q" {
			return m.requestQuit()
		}
		m.help, m.showEvents = false, false
		return m, nil
	}
	if m.showConfig {
		return m.configKey(msg)
	}

	switch k := msg.String(); k {
	case "R":
		return m.restartAlive()

	case "m":
		m.all = !m.all
		return m.syncTailModel()

	case "p":
		m.togglePause()
		return m, nil

	case "c":
		m.clearLog()
		return m, nil

	case "v":
		m.cycleLevel()
		return m, nil

	case "J":
		m.pretty = !m.pretty
		m.repin()
		return m, nil

	case "|":
		m.separators = !m.separators
		m.repin()
		return m, nil

	case "i":
		m.showDetail = !m.showDetail
		m.clampOffset()
		return m, nil

	case "e":
		m.showEvents = true
		return m, nil

	case "C":
		m.showConfig = true
		m.cfgOffset = 0
		return m, nil

	case "f":
		m.nameFiltering = true
		m.nameDraft = m.nameFilter
		return m, nil

	case "0", "1", "2", "3", "4", "5", "6", "7":
		n, _ := strconv.Atoi(k)
		phases := supervisor.Phases()
		switch {
		case n == 0 || n > len(phases):
			m.phaseFilter = ""
		case m.phaseFilter == phases[n-1]:
			m.phaseFilter = ""
		default:
			m.phaseFilter = phases[n-1]
		}
		m.keepSelectionVisible()
		return m.syncTailModel()

	case "L":
		return m.askReload()

	case "q":
		return m.requestQuit()

	case "esc":
		// Escape backs out of the log pane rather than quitting, so the key
		// that cancels a filter cannot also end the session by one keystroke.
		if m.focus == focusLog {
			m.focus = focusList
			return m, nil
		}
		return m.requestQuit()

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
		// Every service by name, not nil: nil is the autostart selection, and
		// "start all" that starts nothing because every service opted out of
		// autostart is a broken key. The web console's Start all does the same.
		return m.launch(opStart, m.allNames())

	case "S":
		return m.launch(opStop, nil)
	}

	return m, nil
}

// editFilter handles a keypress while a filter line is open — the log filter
// or the service-name filter. Enter applies the draft, escape abandons it, and
// an applied empty filter clears filtering altogether.
func (m Model) editFilter(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.nameFiltering {
		switch msg.Type {
		case tea.KeyEnter:
			m.nameFilter, m.nameFiltering, m.nameDraft = m.nameDraft, false, ""
			m.keepSelectionVisible()
			return m.syncTailModel()
		case tea.KeyEsc:
			m.nameFiltering, m.nameDraft = false, ""
		case tea.KeyBackspace:
			if r := []rune(m.nameDraft); len(r) > 0 {
				m.nameDraft = string(r[:len(r)-1])
			}
		case tea.KeyRunes, tea.KeySpace:
			m.nameDraft += string(msg.Runes)
		}
		return m, nil
	}
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

// move steps the selection by delta among the rows the list filters show,
// clamped to them, and re-points the log tail when the selection changed.
func (m Model) move(delta int) (tea.Model, tea.Cmd) {
	idx := m.listIndices()
	if len(idx) == 0 {
		return m, nil
	}
	at := 0
	for i, n := range idx {
		if n == m.sel {
			at = i
		}
	}
	sel := idx[min(max(at+delta, 0), len(idx)-1)]
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

// bottomOffset is the offset that shows the last screenful of log.
func (m Model) bottomOffset() int {
	return max(0, len(m.rows())-m.logHeight())
}

// repin keeps a following pane at the bottom after a change to how records
// expand into rows (pretty JSON, separators), and clamps one that is not.
func (m *Model) repin() {
	if m.follow {
		m.offset = m.bottomOffset()
		return
	}
	m.clampOffset()
}

// listIndices is the indices into statuses the list shows: every service, or
// those the name and phase filters keep.
func (m Model) listIndices() []int {
	needle := strings.ToLower(m.nameFilter)
	out := make([]int, 0, len(m.statuses))
	for i, st := range m.statuses {
		if m.phaseFilter != "" && st.Phase != m.phaseFilter {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(st.Name), needle) {
			continue
		}
		out = append(out, i)
	}
	return out
}

// keepSelectionVisible moves the selection to the first shown row when the
// list filters hide the selected one. With nothing shown it stays put, and
// the list says that nothing matches.
func (m *Model) keepSelectionVisible() {
	idx := m.listIndices()
	for _, i := range idx {
		if i == m.sel {
			return
		}
	}
	if len(idx) > 0 {
		m.sel = idx[0]
		m.filter = ""
	}
}

// allNames is every service, by name, in declaration order.
func (m Model) allNames() []string {
	names := make([]string, 0, len(m.statuses))
	for _, st := range m.statuses {
		names = append(names, st.Name)
	}
	return names
}

// restartAlive restarts every service that is running now, like the web
// console's Restart all. It names them rather than passing nil: a nil restart
// stops everything and starts the autostart selection, which would stop a
// running autostart:false service for good and start one deliberately left
// stopped.
func (m Model) restartAlive() (tea.Model, tea.Cmd) {
	var names []string
	for _, st := range m.statuses {
		if st.Phase.Alive() {
			names = append(names, st.Name)
		}
	}
	if len(names) == 0 {
		m.notice = "nothing is running, so there is nothing to restart"
		return m, nil
	}
	return m.launch(opRestart, names)
}

// configKey handles a keypress while the configuration view is open: the
// scroll keys scroll it, q quits, anything else closes it.
func (m Model) configKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		m.cfgOffset = max(0, m.cfgOffset-1)
	case "down", "j":
		m.cfgOffset++
	case "pgup":
		m.cfgOffset = max(0, m.cfgOffset-max(1, m.height-2))
	case "pgdown", " ":
		m.cfgOffset += max(1, m.height-2)
	case "g":
		m.cfgOffset = 0
	case "q":
		return m.requestQuit()
	case "L":
		m.showConfig = false
		return m.askReload()
	default:
		m.showConfig = false
	}
	return m, nil
}

// clampOffset keeps the viewport inside the buffer.
func (m *Model) clampOffset() {
	m.offset = min(max(m.offset, 0), m.bottomOffset())
}

// deriveRoot recovers the config root from a status log path, which is
// <root>/.mabo-ctl/logs/<svc>.log. It is the fallback for a caller that did not set
// [Options.Root]; it returns "" when no status carries a log path.
func deriveRoot(sts []supervisor.Status) string {
	for _, st := range sts {
		if st.LogPath == "" {
			continue
		}
		// <root>/.mabo-ctl/logs/<svc>.log → up three levels.
		root := filepath.Dir(filepath.Dir(filepath.Dir(st.LogPath)))
		if root != "" && root != "." && root != string(filepath.Separator) {
			return root
		}
	}
	return ""
}
