package console

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

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
	dimStyle      = lipgloss.NewStyle().Faint(true)
	cursorStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13"))
	selectedStyle = lipgloss.NewStyle().Bold(true)
	noticeStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	errorStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
	filterStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	helpKeyStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
)

// View renders the whole console as one string.
//
// It is a pure function of the model: no clock is read, no file is opened and
// nothing is measured against the real terminal, so a test can assert on a
// frame at any size. Every line is exactly the model's width, because a
// wrapped line would push the bottom of the layout off the screen.
func (m Model) View() string {
	if m.quit {
		return ""
	}
	switch {
	case m.help:
		return m.helpView()
	case m.showEvents:
		return m.eventsView()
	}

	f := m.frame()
	var main []string
	top, _ := m.topBar(f.top.w)
	tabs, _ := m.tabBar(f.tabs.w)
	main = append(main, top, tabs)
	if !f.wide {
		main = append(main, m.sideLines(f)...)
	}
	if m.showConfig {
		main = append(main, m.configLines(f.body)...)
	} else {
		main = append(main, m.cardLines(f.card)...)
		if f.tool.h > 0 {
			l1, _ := m.toolBar(f.tool.w)
			main = append(main, l1)
		}
		if f.tool.h > 1 {
			l2, _ := m.levelBar(f.tool.w)
			main = append(main, l2)
		}
		if f.sys.h > 0 {
			main = append(main, m.systemLine(f.sys.w))
		}
		main = append(main, m.logLines(f.log)...)
	}
	if !f.wide {
		main = append(main, fit(m.hintsView(), f.hints.w))
		return strings.Join(clip(main, m.height), "\n")
	}

	side := m.sideLines(f)
	rule := ruleStyle.Render("│")
	lines := make([]string, m.height)
	for y := range lines {
		l, r := strings.Repeat(" ", f.side.w), strings.Repeat(" ", f.top.w)
		if y < len(side) {
			l = side[y]
		}
		if y < len(main) {
			r = main[y]
		}
		lines[y] = l + rule + r
	}
	return strings.Join(lines, "\n")
}

// clip keeps at most n lines. Cutting the frame is better than overflowing
// it: an overflowing frame scrolls the alternate screen and the top bar walks
// off the top.
func clip(lines []string, n int) []string {
	if n > 0 && len(lines) > n {
		return lines[:n]
	}
	return lines
}

// titleView is the top bar's text, for callers that want the console's
// heading as one line.
func (m Model) titleView() string {
	s, _ := m.topBar(m.width)
	return s
}

// logHeight is how many log rows show.
func (m Model) logHeight() int {
	return max(1, m.frame().log.h)
}

// listWindow is the first shown row of idx, scrolled so the selection stays
// visible when the list is taller than the space for it.
func (m Model) listWindow(idx []int, h int) int {
	if len(idx) <= h || h <= 0 {
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
	case st.Phase == supervisor.PhaseSlow && st.Health != "":
		return "waiting for " + st.Health
	case st.Phase == supervisor.PhaseDegraded && st.Health != "":
		return "not answering " + st.Health
	default:
		return ""
	}
}

// logRowView draws one row: a separator rule, or the text coloured by its
// level — in merged mode after the service label on an entry's first row and
// indented past it on the rest, so a pretty-printed record reads as one
// block.
func (m Model) logRowView(r logRow, labelW, w int) string {
	if r.sep {
		return dimStyle.Render(strings.Repeat("┄", max(1, w)))
	}
	text := sanitize(r.text)
	paint := func(s string) string {
		if st, ok := levelStyle(r.lvl); ok {
			return st.Render(s)
		}
		return s
	}
	if labelW == 0 {
		return paint(truncate(text, w))
	}
	room := w - labelW - 3
	if room < 4 {
		return paint(truncate(text, w))
	}
	if !r.first {
		return strings.Repeat(" ", labelW+3) + paint(truncate(text, room))
	}
	return m.nameStyle(r.svc).Render(pad(r.svc, labelW)) + dimStyle.Render(" · ") + paint(truncate(text, room))
}

// hints is the narrow layout's key legend, longest form first. The first one
// that fits the terminal is used, so a narrow window loses hints rather than
// wrapping the layout. The wide layout needs none: every control is drawn,
// with its key.
var hints = []string{
	"↑/↓ select · tab log · s/x/r · a/R/S all · m merge · p pause · / filter · v level · J json · i more · C config · ? help · q quit",
	"↑/↓ select · s/x/r · a/R/S all · m merge · / filter · C config · ? help · q quit",
	"↑/↓ · s/x/r · a/R/S · / filter · ? help · q quit",
	"? help · q quit",
}

// hintsView renders the key legend that fits the current width.
func (m Model) hintsView() string {
	for _, h := range hints {
		if ansi.StringWidth(h) <= m.width {
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
	{"i", "open the card's log file, command & arguments: log, cmd, dir, deps, env"},
	{"f, 1-7, 0", "filter the list by name · show one phase (again clears) · 0 clears it"},
	{"e, C", "activity: recent operations and errors · the CONFIG tab (C or esc returns)"},
	{"mouse", "click any row, tab, chip, button or toggle; the wheel scrolls · hold Shift (Option on macOS) to select text"},
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
	if n := w - ansi.StringWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// truncate shortens s to at most w columns, marking the cut with an ellipsis.
// Columns, not characters: an emoji or a CJK character takes two, and
// counting it as one pushed every line it was on past the edge. A w of 0 or
// less yields "".
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "…")
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
