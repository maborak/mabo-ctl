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
	if m.help {
		return m.helpView()
	}

	lines := make([]string, 0, m.height)
	lines = append(lines, m.titleView())
	lines = append(lines, m.listView()...)
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

	gap := m.width - utf8.RuneCountInString(left) - utf8.RuneCountInString(right)
	text := left + strings.Repeat(" ", max(gap, 1)) + right
	return titleStyle.Render(truncate(text, m.width))
}

// listHeight is how many service rows fit. The list keeps at least one row
// even on a very short terminal, and never takes so much space that the log
// pane disappears entirely.
func (m Model) listHeight() int {
	n := max(1, len(m.statuses))
	avail := max(1, m.height-chromeRows-1)
	return min(n, avail)
}

// logHeight is how many log lines fit under the list.
func (m Model) logHeight() int {
	return max(1, m.height-chromeRows-m.listHeight())
}

// listWindow is the first service row shown, scrolled so the selection stays
// visible when the list is taller than the space for it.
func (m Model) listWindow() int {
	h := m.listHeight()
	if len(m.statuses) <= h {
		return 0
	}
	start := min(max(m.sel-h/2, 0), len(m.statuses)-h)
	return start
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

	start := m.listWindow()
	rows := make([]string, 0, h)
	for i := start; i < len(m.statuses) && len(rows) < h; i++ {
		rows = append(rows, m.serviceRow(m.statuses[i], i == m.sel, wName, wPhase, wPort, wPID))
	}
	for len(rows) < h {
		rows = append(rows, "")
	}
	return rows
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
	if svc != "" {
		label = "logs: " + svc
	}

	var flags []string
	if m.focus == focusLog {
		flags = append(flags, "focus")
	}
	if m.follow {
		flags = append(flags, "follow")
	} else {
		flags = append(flags, fmt.Sprintf("line %d/%d", m.offset+1, max(len(m.visibleLines()), 1)))
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
	visible := m.visibleLines()

	out := make([]string, 0, h)
	if len(visible) == 0 {
		msg := "  (no output yet)"
		if m.filter != "" && len(m.lines) > 0 {
			msg = fmt.Sprintf("  (no line matches %q; %d hidden)", m.filter, len(m.lines))
		}
		out = append(out, dimStyle.Render(truncate(msg, m.width)))
	}

	start := min(max(m.offset, 0), max(0, len(visible)-1))
	for i := start; i < len(visible) && len(out) < h; i++ {
		out = append(out, truncate(sanitize(visible[i]), m.width))
	}
	for len(out) < h {
		out = append(out, "")
	}
	return out[:h]
}

// noticeView is the single line above the key hints: the newest supervisor
// event, or the newest error, which outranks it. An error is never dropped
// silently — that is the failure mode this whole tool exists to stop.
func (m Model) noticeView() string {
	if m.err != nil {
		text := errRenderer.Error(m.err)
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
	"↑/↓ select · s start · x stop · r restart · a start all · S stop all · tab log · / filter · g/G top/bottom · ? help · q quit",
	"↑/↓ select · s/x/r start/stop/restart · a/S all · tab log · / filter · ? help · q quit",
	"↑/↓ · s/x/r · a/S · / filter · ? help · q quit",
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
	{"s", "start the selected service"},
	{"x", "stop the selected service"},
	{"r", "restart the selected service"},
	{"a", "start every service"},
	{"S", "stop every service"},
	{"l, tab", "focus the log pane; tab or esc returns to the list"},
	{"/", "filter log lines (enter applies, esc cancels, empty clears)"},
	{"g, G", "jump to the top or the bottom of the log; G resumes following"},
	{"pgup/pgdn", "page the log pane"},
	{"?", "toggle this help"},
	{"q, ctrl-c", "quit the console"},
}

// helpNote is printed under the key table, and is the reason the overlay
// exists at all.
const helpNote = `Quitting does NOT stop the supervised services.

They are started detached (setsid), so mabo-ctl is not their parent and closing
this console is closing a window, not shutting anything down. Stop them on
purpose with x, with S, or with ` + "`mabo-ctl stop`" + `.

Starting a service never blocks this console: readiness is waited for in the
background, so the list stays live while a slow service comes up.`

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
