package console

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/ui"
)

// errRenderer formats errors for the status line. Its zero value renders plain
// text with no escape sequences, which is what this package wants: colour here
// is applied by lipgloss, and two styling systems fighting over one string
// produces a broken line rather than a colourful one.
var errRenderer = &ui.Renderer{}

// Styles. They are package-level because they are constants in everything but
// name; lipgloss resolves them against the terminal's colour profile, and
// degrades to plain text when there is none — as in a test.
var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("238"))
	dimStyle      = lipgloss.NewStyle().Faint(true)
	cursorStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13"))
	selectedStyle = lipgloss.NewStyle().Bold(true)
	noticeStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	errorStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
	filterStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	helpKeyStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
)

// Rows the layout always reserves, whatever the terminal height: the title
// bar, the log-pane header, the notice line and the key hints.
const chromeRows = 4

// View renders the whole console as one string.
//
// It is a pure function of the model: no clock is read, no file is opened and
// nothing is measured against the real terminal, so a test can assert on a
// frame at any size. Every line is truncated to the model's width, because a
// wrapped line would push the key hints off the bottom of the screen.
func (m Model) View() string {
	if m.quit {
		return ""
	}
	switch {
	case m.help:
		return m.helpView()
	case m.showEvents:
		return m.eventsView()
	case m.showConfig:
		return m.configView()
	}

	lines := make([]string, 0, m.height)
	lines = append(lines, m.titleView())
	lines = append(lines, m.listView()...)
	lines = append(lines, m.detailView()...)
	lines = append(lines, m.logHeaderView())
	lines = append(lines, m.logView()...)
	lines = append(lines, m.noticeView())
	lines = append(lines, m.hintsView())
	// Below about six rows even the minimum layout does not fit. Cutting the
	// frame is better than overflowing it: an overflowing frame scrolls the
	// alternate screen and the title bar walks off the top.
	if m.height > 0 && len(lines) > m.height {
		lines = lines[:m.height]
	}
	return strings.Join(lines, "\n")
}

// titleView is the top bar: the tool, the config root it is driving, and a
// count of what is running. The root is there because a console started from
// the wrong directory looks exactly like one started from the right one.
func (m Model) titleView() string {
	root := m.root
	if root == "" {
		root = "(config root unknown)"
	}
	left := "mabo-ctl · " + root

	ready := 0
	for _, st := range m.statuses {
		// "Up" counts processes that EXIST, degraded ones included: a service
		// that is alive and not answering is still one mabo-ctl would have to
		// stop, and moving it out of the count would make a stack look emptier
		// than it is. The three states below the line are the ones with no
		// process behind them at all.
		switch st.Phase {
		case supervisor.PhaseReady, supervisor.PhaseRunning,
			supervisor.PhaseSlow, supervisor.PhaseDegraded:
			ready++
		case supervisor.PhaseStopped, supervisor.PhaseFailed, supervisor.PhaseExited:
		}
	}
	right := fmt.Sprintf("%d/%d up", ready, len(m.statuses))
	// An edited mabo-ctl.yaml is announced here rather than on the notice
	// line: it stays true until the operator acts, and on the notice line it
	// either hid every operation's progress or was hidden by it.
	if m.configChanged && m.reload == nil {
		right = "YAML changed · L reloads · " + right
	}

	// The root gives way first: the right-hand side is the part that changes.
	left = truncate(left, max(1, m.width-utf8.RuneCountInString(right)-1))
	gap := m.width - utf8.RuneCountInString(left) - utf8.RuneCountInString(right)
	text := left + strings.Repeat(" ", max(gap, 1)) + right
	return titleStyle.Render(truncate(text, m.width))
}

// listHeight is how many service rows fit. The list keeps at least one row
// even on a very short terminal, and never takes so much space that the log
// pane disappears entirely. A list filter that is open or set takes one more
// row for its own line.
func (m Model) listHeight() int {
	n := max(1, len(m.listIndices())) + m.listFilterRows()
	avail := max(1, m.height-chromeRows-1)
	return min(n, avail)
}

// listFilterRows is 1 when the list shows its filter line, else 0.
func (m Model) listFilterRows() int {
	if m.nameFiltering || m.nameFilter != "" || m.phaseFilter != "" {
		return 1
	}
	return 0
}

// detailRows is the number of rows the detail block takes: detailLines when
// it is open and the terminal is tall enough to keep a useful log pane.
func (m Model) detailRows() int {
	if !m.showDetail || m.selectedName() == "" || m.height-chromeRows-m.listHeight()-detailLines < 3 {
		return 0
	}
	return detailLines
}

// detailLines is the fixed height of the detail block, so the log pane does
// not jump as services with more or fewer facts are selected.
const detailLines = 5

// logHeight is how many log rows fit under the list and the detail block.
func (m Model) logHeight() int {
	return max(1, m.height-chromeRows-m.listHeight()-m.detailRows())
}

// listWindow is the first shown row of idx, scrolled so the selection stays
// visible when the list is taller than the space for it.
func (m Model) listWindow(idx []int, h int) int {
	if len(idx) <= h {
		return 0
	}
	at := 0
	for i, n := range idx {
		if n == m.sel {
			at = i
		}
	}
	return min(max(at-h/2, 0), len(idx)-h)
}

// listView renders the service rows, padded to exactly listHeight lines so the
// panes below never move as services appear and disappear.
func (m Model) listView() []string {
	h := m.listHeight()
	if len(m.statuses) == 0 {
		rows := make([]string, h)
		rows[0] = dimStyle.Render(truncate("  no services configured", m.width))
		for i := 1; i < h; i++ {
			rows[i] = ""
		}
		return rows
	}

	wName, wPhase, wPort, wPID := 0, 0, 0, 0
	for _, st := range m.statuses {
		glyph, word := ui.PhaseLabel(st.Phase)
		wName = max(wName, utf8.RuneCountInString(st.Name))
		wPhase = max(wPhase, utf8.RuneCountInString(glyph)+1+utf8.RuneCountInString(word))
		wPort = max(wPort, utf8.RuneCountInString(portCell(st)))
		wPID = max(wPID, utf8.RuneCountInString(pidCell(st)))
	}

	rows := make([]string, 0, h)
	if m.listFilterRows() > 0 {
		rows = append(rows, m.listFilterLine())
	}
	idx := m.listIndices()
	if len(idx) == 0 {
		rows = append(rows, dimStyle.Render(truncate("  no service matches the list filter · f edits it, 0 clears the phase", m.width)))
	}
	room := h - len(rows)
	start := m.listWindow(idx, room)
	for j := start; j < len(idx) && len(rows) < h; j++ {
		i := idx[j]
		rows = append(rows, m.serviceRow(m.statuses[i], i == m.sel, wName, wPhase, wPort, wPID))
	}
	for len(rows) < h {
		rows = append(rows, "")
	}
	return rows[:h]
}

// listFilterLine shows the list filters in force, or the name being typed.
func (m Model) listFilterLine() string {
	var parts []string
	switch {
	case m.nameFiltering:
		parts = append(parts, "name: "+m.nameDraft+"█")
	case m.nameFilter != "":
		parts = append(parts, "name: "+m.nameFilter)
	}
	if m.phaseFilter != "" {
		_, word := ui.PhaseLabel(m.phaseFilter)
		parts = append(parts, "phase: "+word)
	}
	shown := len(m.listIndices())
	parts = append(parts, fmt.Sprintf("%d of %d shown", shown, len(m.statuses)))
	return filterStyle.Render(truncate("  "+strings.Join(parts, " · "), m.width))
}

// detailView is the selected service's detail block (i): what the web
// console's detail pane shows, in detailLines rows. Everything comes from the
// redacted status snapshot and the redacted configuration view.
func (m Model) detailView() []string {
	n := m.detailRows()
	if n == 0 {
		return nil
	}
	st := m.statuses[m.sel]
	var cs ui.ConfigService
	for _, s := range m.cfgView.Services {
		if s.Name == st.Name {
			cs = s
		}
	}
	or := func(v, alt string) string {
		if v == "" {
			return alt
		}
		return v
	}
	facts := []string{
		"pid " + or(strings.TrimPrefix(pidCell(st), "pid "), "-"),
		"port " + portCell(st),
		"uptime " + or(ui.ShortDuration(st.Uptime), "-"),
		"ready in " + or(ui.ShortDuration(st.Elapsed), "-"),
		"runtime " + or(cs.Runtime, "system"),
	}
	if st.PID > 0 && st.Usage.RSS > 0 {
		facts = append(facts, "cpu "+ui.FormatCPU(st.Usage.CPU)+" · mem "+ui.FormatBytes(st.Usage.RSS))
	}
	health := or(st.Health, "no probe declared")
	if st.HTTP != 0 {
		health += fmt.Sprintf(" · HTTP %d", st.HTTP)
	}
	env := make([]string, 0, len(cs.Env))
	for _, v := range cs.Env {
		env = append(env, v.Key+"="+v.Value)
	}
	lines := []string{
		strings.Join(facts, " · "),
		"health  " + health,
		"cmd     " + or(cs.CmdLine, "-"),
		"dir     " + or(cs.Dir, "-") + " · log " + or(st.LogPath, "-"),
		"deps    " + or(strings.Join(cs.DependsOn, ", "), "none") + " · env " + or(strings.Join(env, " "), "none"),
	}
	out := make([]string, 0, n)
	for _, l := range lines[:n] {
		out = append(out, dimStyle.Render(truncate("  "+sanitize(l), m.width)))
	}
	return out
}

// serviceRow renders one service: cursor, name in the service's colour, the
// phase glyph AND word — colour is never the only signal — then port, pid and
// whatever the supervisor had to say.
func (m Model) serviceRow(st supervisor.Status, selected bool, wName, wPhase, wPort, wPID int) string {
	glyph, word := ui.PhaseLabel(st.Phase)

	cursor := "  "
	if selected {
		cursor = cursorStyle.Render("▸ ")
	}

	name := m.nameStyle(st.Name).Render(pad(st.Name, wName))
	phase := phaseStyle(st.Phase).Render(pad(glyph+" "+word, wPhase))
	port := pad(portCell(st), wPort)
	pid := pad(pidCell(st), wPID)

	head := cursor + name + "  " + phase + "  " + port + "  " + pid
	used := lipgloss.Width(head)

	detail := detailCell(st)
	if kind, ok := m.pending[st.Name]; ok {
		detail = strings.TrimSpace(string(kind) + "… " + detail)
	}
	if detail != "" {
		if avail := m.width - used - 2; avail > 4 {
			head += "  " + dimStyle.Render(truncate(detail, avail))
		}
	}
	if selected {
		return selectedStyle.Render(truncateStyled(head, m.width))
	}
	return truncateStyled(head, m.width)
}

// portCell is the port column: "-" for a portless service, never "0", which
// reads as a real port number.
func portCell(st supervisor.Status) string {
	if st.Port <= 0 {
		return "-"
	}
	return ":" + strconv.Itoa(st.Port)
}

// pidCell is the pid column, "-" when nothing is running.
func pidCell(st supervisor.Status) string {
	if st.PID <= 0 {
		return "-"
	}
	return "pid " + strconv.Itoa(st.PID)
}

// detailCell is the one-line summary of a status. A multi-line Detail — the
// log tail the supervisor attaches to a failed start — contributes only its
// first line here; the whole thing is in the log pane, which is what the log
// pane is for.
func detailCell(st supervisor.Status) string {
	detail := strings.TrimSpace(st.Detail)
	if i := strings.IndexByte(detail, '\n'); i >= 0 {
		detail = strings.TrimSpace(detail[:i]) + " …"
	}
	switch {
	case detail != "" && st.HTTP != 0:
		return fmt.Sprintf("HTTP %d %s", st.HTTP, detail)
	case detail != "":
		return detail
	case st.HTTP != 0:
		return fmt.Sprintf("HTTP %d", st.HTTP)
	case st.Phase == supervisor.PhaseSlow && st.Health != "":
		return "waiting for " + st.Health
	case st.Phase == supervisor.PhaseDegraded && st.Health != "":
		return "not answering " + st.Health
	default:
		return ""
	}
}

// logHeaderView is the separator between the list and the log pane. It carries
// the state a reader needs to trust what is below it: which service, whether
// the pane is following the newest line, and which filter is hiding lines.
func (m Model) logHeaderView() string {
	svc := m.tailSvc
	if svc == "" {
		svc = m.selectedName()
	}
	label := "logs"
	switch {
	case m.all:
		label = "logs: all services"
	case svc != "":
		label = "logs: " + svc
	}

	var flags []string
	if m.focus == focusLog {
		flags = append(flags, "focus")
	}
	switch {
	case m.paused:
		flags = append(flags, fmt.Sprintf("PAUSED +%d", m.pausedNew))
	case m.follow:
		flags = append(flags, "follow")
	default:
		flags = append(flags, fmt.Sprintf("line %d/%d", m.offset+1, max(len(m.rows()), 1)))
	}
	if m.levelFilter != "" {
		flags = append(flags, "level "+m.levelFilter)
	}
	if m.pretty {
		flags = append(flags, "pretty")
	}
	if m.separators {
		flags = append(flags, "sep")
	}

	// The pieces are measured and clipped as plain text, then styled: styling
	// first would mean measuring escape sequences, and a header one column too
	// wide wraps and pushes the key hints off the bottom of the screen.
	right := ""
	switch {
	case m.filtering:
		right = "/" + m.filterDraft + "█"
	case m.filter != "":
		right = "filter: " + m.filter
	}

	left := "── " + label + " (" + strings.Join(flags, ", ") + ") "
	if utf8.RuneCountInString(left) >= m.width {
		return dimStyle.Render(truncate(left, m.width))
	}
	room := m.width - utf8.RuneCountInString(left)
	right = truncate(right, room)
	fill := room - utf8.RuneCountInString(right)
	return dimStyle.Render(left+strings.Repeat("─", fill)) + filterStyle.Render(right)
}

// logView renders exactly logHeight lines of the selected service's log,
// padding with blanks so the notice and hint lines stay where they were.
func (m Model) logView() []string {
	h := m.logHeight()
	visible := m.rows()

	out := make([]string, 0, h)
	if len(visible) == 0 {
		msg := "  (no output yet)"
		if (m.filter != "" || m.levelFilter != "") && len(m.recs) > 0 {
			msg = fmt.Sprintf("  (no line matches the filter; %d hidden)", len(m.recs))
		}
		out = append(out, dimStyle.Render(truncate(msg, m.width)))
	}

	labelW := 0
	if m.all {
		for _, st := range m.statuses {
			labelW = max(labelW, utf8.RuneCountInString(st.Name))
		}
	}
	start := min(max(m.offset, 0), max(0, len(visible)-1))
	for i := start; i < len(visible) && len(out) < h; i++ {
		out = append(out, m.logRowView(visible[i], labelW))
	}
	for len(out) < h {
		out = append(out, "")
	}
	return out[:h]
}

// logRowView draws one row: a separator rule, or the text — in merged mode
// after the service label on an entry's first row and indented past it on
// the rest, so a pretty-printed record reads as one block.
func (m Model) logRowView(r logRow, labelW int) string {
	if r.sep {
		return dimStyle.Render(strings.Repeat("┄", max(1, m.width)))
	}
	text := sanitize(r.text)
	if labelW == 0 {
		return truncate(text, m.width)
	}
	room := m.width - labelW - 3
	if room < 4 {
		return truncate(text, m.width)
	}
	if !r.first {
		return strings.Repeat(" ", labelW+3) + truncate(text, room)
	}
	return m.nameStyle(r.svc).Render(pad(r.svc, labelW)) + dimStyle.Render(" · ") + truncate(text, room)
}

// noticeView is the single line above the key hints: the reload question
// while it is open, else the newest error — an operation's, then a reload's —
// else the newest supervisor event. An error is never dropped silently — that
// is the failure mode this whole tool exists to stop.
func (m Model) noticeView() string {
	if m.confirmReload {
		return errorStyle.Render(truncate(
			"Reload mabo-ctl.yaml? y reloads, any other key cancels · changed running services restart, removed ones stop",
			m.width))
	}
	for _, err := range []error{m.err, m.reloadErr} {
		if err == nil {
			continue
		}
		text := errRenderer.Error(err)
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[:i] + " …"
		}
		return errorStyle.Render(truncate(text, m.width))
	}
	if m.notice != "" {
		return noticeStyle.Render(truncate(m.notice, m.width))
	}
	return dimStyle.Render(truncate("ready · ? for help", m.width))
}

// hints is the key legend, longest form first. The first one that fits the
// terminal is used, so a narrow window loses hints rather than wrapping the
// layout.
var hints = []string{
	"↑/↓ select · s/x/r start/stop/restart · a/R/S all · m merge · p pause · / filter · v level · J json · i detail · C config · ? help · q quit",
	"↑/↓ select · s/x/r start/stop/restart · a/R/S all · m merge · / filter · i detail · ? help · q quit",
	"↑/↓ · s/x/r · a/R/S · / filter · ? help · q quit",
	"? help · q quit",
}

// hintsView renders the key legend that fits the current width.
func (m Model) hintsView() string {
	for _, h := range hints {
		if utf8.RuneCountInString(h) <= m.width {
			return dimStyle.Render(h)
		}
	}
	return dimStyle.Render(truncate(hints[len(hints)-1], m.width))
}

// helpLines is the help overlay. The last paragraph is the important one: a
// user who believes quitting stops their services spends the next session
// wondering who is holding every port.
var helpLines = []struct{ key, what string }{
	{"↑/k, ↓/j", "select a service (scroll the log pane when it has focus)"},
	{"s, x, r", "start, stop or restart the selected service"},
	{"a, R, S", "start every service (autostart or not) · restart the running ones · stop all"},
	{"L", "reload mabo-ctl.yaml after asking: restarts the services an edit changes, stops removed ones"},
	{"l, tab", "focus the log pane; tab or esc returns to the list"},
	{"/, v", "filter log lines (enter applies, esc cancels) · cycle the level: all, error, warn, info, debug"},
	{"m", "merge every service's log into one labelled pane; m again returns to one service"},
	{"p, c", "pause the pane (lines keep arriving within the same cap) · clear it"},
	{"J, |", "pretty-print JSON lines (whitespace only) · separators between entries"},
	{"g, G, pgup/dn", "top or bottom of the log (G resumes following) · page it"},
	{"i", "the selected service's details: uptime, health, command, dir, deps, env"},
	{"f, 1-7, 0", "filter the list by name · show one phase (again clears) · 0 clears it"},
	{"e, C", "activity: recent operations and errors · the configuration and start order"},
	{"?", "toggle this help"},
	{"q, ctrl-c", "quit the console (during a reload: once it finishes; press again to quit now)"},
}

// helpNote is printed under the key table, and is the reason the overlay
// exists at all.
const helpNote = `Quitting does NOT stop the supervised services. They run detached (setsid):
closing this console closes a window. Stop them with x, S or ` + "`mabo-ctl stop`" + `.
Starting never blocks this console; readiness is waited for in the background.`

// overlayView frames body lines as a full-screen overlay under the title bar,
// newest last when the body is taller than the screen and tail is set, with
// footer as the last line.
func (m Model) overlayView(body []string, offset int, footer string) string {
	lines := []string{m.titleView()}
	room := max(1, m.height-2)
	offset = min(max(offset, 0), max(0, len(body)-room))
	for i := offset; i < len(body) && len(lines) < room+1; i++ {
		lines = append(lines, truncate(sanitize(body[i]), m.width))
	}
	for len(lines) < m.height-1 {
		lines = append(lines, "")
	}
	lines = append(lines, dimStyle.Render(truncate(footer, m.width)))
	return strings.Join(lines[:max(1, min(len(lines), m.height))], "\n")
}

// eventsView is the activity overlay (e): the newest events that fit.
func (m Model) eventsView() string {
	body := []string{"  activity — the newest last"}
	if len(m.events) == 0 {
		body = append(body, "  nothing has happened in this console yet")
	}
	for _, e := range m.events {
		body = append(body, "  "+e)
	}
	return m.overlayView(body, len(body), "any key returns · q quits")
}

// configView is the configuration overlay (C): the same view `mabo-ctl
// config` prints, redacted, then the start order.
func (m Model) configView() string {
	r := &ui.Renderer{}
	text := r.ConfigBlock(m.cfgView, "") + "\n\n" + r.ConfigGraph(m.cfgView)
	if m.cfgView.Source.Path == "" {
		text = "this console cannot describe its configuration"
	}
	return m.overlayView(strings.Split(text, "\n"), m.cfgOffset,
		"↑/↓ pgup/pgdn scroll · L reloads · any other key returns · q quits")
}

// helpView renders the help overlay over the whole screen.
func (m Model) helpView() string {
	lines := []string{m.titleView(), ""}

	wKey := 0
	for _, h := range helpLines {
		wKey = max(wKey, utf8.RuneCountInString(h.key))
	}
	for _, h := range helpLines {
		lines = append(lines, "  "+helpKeyStyle.Render(pad(h.key, wKey))+"   "+truncate(h.what, max(1, m.width-wKey-5)))
	}
	lines = append(lines, "")
	for _, l := range strings.Split(helpNote, "\n") {
		lines = append(lines, truncate("  "+l, m.width))
	}
	for len(lines) < m.height-1 {
		lines = append(lines, "")
	}
	lines = append(lines, dimStyle.Render(truncate("any key returns · q quits", m.width)))
	return strings.Join(lines[:max(1, min(len(lines), m.height))], "\n")
}

// eventLine renders one supervisor event as the console's notice line. It
// deliberately does not use ui.Renderer.Event: that renders a padded, coloured
// stream line for the CLI, and this is a single line inside a laid-out pane.
func eventLine(ev supervisor.Event) string {
	var parts []string
	if ev.Service != "" {
		parts = append(parts, ev.Service)
	}
	if ev.Phase != "" {
		_, word := ui.PhaseLabel(ev.Phase)
		parts = append(parts, word)
	}
	if ev.Msg != "" {
		parts = append(parts, ev.Msg)
	}
	if ev.Err != nil {
		parts = append(parts, "error: "+ev.Err.Error())
	}
	return strings.Join(parts, ": ")
}

// phaseStyle is the colour of a phase. It only ever reinforces the glyph and
// the word from ui.PhaseLabel, which carry the meaning on their own.
func phaseStyle(p supervisor.Phase) lipgloss.Style {
	switch p {
	case supervisor.PhaseReady:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	case supervisor.PhaseRunning:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	case supervisor.PhaseSlow:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	case supervisor.PhaseDegraded:
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	case supervisor.PhaseFailed:
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
	case supervisor.PhaseExited:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	case supervisor.PhaseStopped:
		return dimStyle
	default:
		return dimStyle
	}
}

// namedColors maps the colour vocabulary of mabo-ctl.yaml onto lipgloss colours.
//
// internal/ui has its own copy of this vocabulary because it emits raw SGR
// parameters; lipgloss needs a lipgloss.Color and resolves it against the
// terminal's profile, so the mapping cannot be shared. The vocabulary is the
// same on both sides, which is what actually matters to a user reading
// mabo-ctl.yaml.
var namedColors = map[string]int{
	"black": 0, "red": 1, "green": 2, "yellow": 3,
	"blue": 4, "magenta": 5, "cyan": 6, "white": 7,
	"gray": 8, "grey": 8,
}

// fallbackPalette gives a service with no declared colour a stable one, so the
// same service is the same colour in every session.
var fallbackPalette = []string{"6", "2", "3", "4", "5", "14", "10"}

// nameStyle is the colour of a service label: the one declared in mabo-ctl.yaml
// when it is recognised, otherwise a stable choice derived from the name.
func (m Model) nameStyle(name string) lipgloss.Style {
	if c, ok := m.opt.Colors[name]; ok {
		if col, ok := parseColor(c); ok {
			return lipgloss.NewStyle().Foreground(col)
		}
	}
	h := fnv.New32a()
	// Hash.Write never returns an error; the interface keeps the signature.
	_, _ = h.Write([]byte(name))
	return lipgloss.NewStyle().Foreground(lipgloss.Color(fallbackPalette[int(h.Sum32())%len(fallbackPalette)]))
}

// parseColor turns a declared colour into a lipgloss colour. It accepts a name
// from namedColors, that name prefixed "bright-", a 0..255 palette index and
// #rrggbb. It reports false for anything else, including "", so the caller
// falls back rather than rendering a broken cell.
func parseColor(c string) (lipgloss.Color, bool) {
	c = strings.ToLower(strings.TrimSpace(c))
	if c == "" {
		return "", false
	}
	bright := false
	for _, p := range []string{"bright-", "bright_", "bright "} {
		if rest, ok := strings.CutPrefix(c, p); ok {
			c, bright = rest, true
			break
		}
	}
	if n, ok := namedColors[c]; ok {
		if bright {
			n += 8
		}
		return lipgloss.Color(strconv.Itoa(n)), true
	}
	if hex, ok := strings.CutPrefix(c, "#"); ok && len(hex) == 6 {
		if _, err := strconv.ParseUint(hex, 16, 32); err == nil {
			return lipgloss.Color("#" + hex), true
		}
		return "", false
	}
	if n, err := strconv.Atoi(c); err == nil && n >= 0 && n <= 255 {
		return lipgloss.Color(strconv.Itoa(n)), true
	}
	return "", false
}

// pad right-pads s to w columns.
func pad(s string, w int) string {
	if n := w - utf8.RuneCountInString(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// truncate shortens plain text to at most w columns, marking the cut with an
// ellipsis. A w of 0 or less yields "".
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	return string([]rune(s)[:w-1]) + "…"
}

// truncateStyled shortens a string that may already carry style escapes. It
// only ever cuts, never re-styles, so a row that fits is returned untouched.
func truncateStyled(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}

// sanitize makes a line from a supervised process safe to draw.
//
// Log output is untrusted input as far as the screen is concerned: a dev
// server that emits a colour reset, a cursor move or a clear-screen would
// otherwise repaint or corrupt the console's own layout. Escape sequences and
// other control characters are dropped and tabs become spaces, so a log line
// occupies exactly one row of exactly the width it appears to be.
func sanitize(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == 0x1b {
			i = skipEscape(s, i)
			continue
		}
		switch {
		case r == '\t':
			b.WriteString("    ")
		case r == utf8.RuneError && size == 1:
			b.WriteRune('�')
		case r < 0x20 || r == 0x7f:
			// Drop the remaining C0 controls, including a stray CR that would
			// otherwise rewind the cursor to the start of the row.
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// skipEscape returns the index just past the escape sequence beginning at the
// ESC byte at i, handling the two shapes a dev server actually emits: CSI
// (colour, cursor movement, clear screen) and OSC (window title).
func skipEscape(s string, i int) int {
	i++ // the ESC itself
	if i >= len(s) {
		return i
	}
	switch s[i] {
	case '[': // CSI: parameter and intermediate bytes, then one final byte.
		i++
		for i < len(s) && s[i] >= 0x20 && s[i] < 0x40 {
			i++
		}
		if i < len(s) {
			i++
		}
	case ']': // OSC: runs to BEL or to the ST string terminator.
		i++
		for i < len(s) && s[i] != 0x07 {
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
			i++
		}
		if i < len(s) {
			i++
		}
	default: // A two-byte escape such as ESC ( B.
		i++
	}
	return i
}
