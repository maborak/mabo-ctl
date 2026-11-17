package console

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/ui"
)

// The console mirrors the web console's structure: a sidebar listing the
// services, and a main column with a top bar (config, the all-services
// buttons, liveness), a tab bar (OUTPUT and CONFIG, then a count chip per
// phase), the selected service's card (name, phase, its buttons, the detail
// grid), the output toolbar (merge, pause, follow, clear, filter; level chips,
// separators, pretty JSON), a system line, and the log.
//
// Everything is laid out by [Model.frame], a pure function of the model. View
// draws into the regions it returns and the mouse handler hit-tests against
// the same regions and the same toolbar spans, so a click always lands on what
// is drawn.

// wideMin is the narrowest terminal that gets the sidebar. Below it the same
// parts stack vertically, the service table above the card.
const wideMin = 120

// rect is a region of the terminal, in cells.
type rect struct{ x, y, w, h int }

// has reports whether the cell (x, y) is inside r.
func (r rect) has(x, y int) bool {
	return x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h
}

// frame is where every part of the console sits for the current size. A part
// that does not show has a zero height.
type frame struct {
	wide bool
	// side is the sidebar (wide) or the service table (narrow).
	side rect
	// top and tabs are the main column's two bars.
	top, tabs rect
	// card, tool, sys and log make up the OUTPUT tab; body is the CONFIG tab.
	card, tool, sys, log, body rect
	// hints is the key legend under a narrow layout.
	hints rect
}

// Rows in the wide sidebar above the service rows (brand, filter, column
// header) and below them (the keys footer).
const (
	sideHead = 3
	sideFoot = 1
)

// frame lays the console out for the model's size.
func (m Model) frame() frame {
	w, h := max(m.width, 1), max(m.height, 1)
	var f frame
	var mx, mw, y, bottom int
	if w >= wideMin && h >= 12 {
		f.wide = true
		sw := m.sideWidth()
		f.side = rect{0, 0, sw, h}
		mx, mw = sw+1, w-sw-1
		f.top = rect{mx, 0, mw, 1}
		f.tabs = rect{mx, 1, mw, 1}
		y, bottom = 2, h
	} else {
		mx, mw = 0, w
		f.top = rect{0, 0, w, 1}
		f.tabs = rect{0, 1, w, 1}
		rows := 1 + max(1, len(m.listIndices()))
		sh := min(rows, max(2, (h-3)/3))
		f.side = rect{0, 2, w, sh}
		f.hints = rect{0, h - 1, w, 1}
		y, bottom = 2+sh, h-1
	}
	if m.showConfig {
		f.body = rect{mx, y, mw, max(0, bottom-y)}
		return f
	}
	ch := m.cardHeight(bottom - y)
	f.card = rect{mx, y, mw, ch}
	y += ch
	tool := min(2, max(0, bottom-y-2))
	f.tool = rect{mx, y, mw, tool}
	y += tool
	sys := min(1, max(0, bottom-y-1))
	f.sys = rect{mx, y, mw, sys}
	y += sys
	f.log = rect{mx, y, mw, max(1, bottom-y)}
	return f
}

// cardHeight is how many rows the service card takes out of avail: the full
// card with the detail grid, plus the command block when it is open; a
// one-line compact card when the terminal is short; none when there is no
// service or no room. The log keeps at least five rows.
func (m Model) cardHeight(avail int) int {
	if m.selectedName() == "" {
		return 0
	}
	const rest = 3 + 5 // toolbar, system line, and a usable log
	full := 5
	if m.showDetail {
		full += len(m.cardExtra())
	}
	switch {
	case avail-full >= rest:
		return full
	case avail-5 >= rest:
		return 5
	case avail-3 >= rest:
		return 3
	}
	return 0
}

// sideWidth is the wide sidebar's width: the service table's columns, within
// a third of the terminal.
func (m Model) sideWidth() int {
	nameW := 7
	for _, st := range m.statuses {
		nameW = max(nameW, ansi.StringWidth(st.Name))
	}
	nameW = min(nameW, 20)
	table := 3 + nameW + 1 + colPort + 1 + colPID + 1 + colUptime + 1 + colProbe + 1
	return min(max(table, 36), max(36, m.width/3))
}

// span is the columns [x0, x1) of a clickable item in a bar, and what a click
// on it does.
type span struct {
	x0, x1 int
	action string
}

// item is one piece of a bar: its text, its look, and the action a click on
// it runs ("" for plain text).
type item struct {
	text   string
	style  lipgloss.Style
	action string
}

// bar lays out left items from the left edge and right items flush right in
// w columns, one space apart. Items that do not fit are dropped, the left
// group's last first, so a narrow bar keeps its leading, most important
// controls. It returns the drawn line, exactly w columns wide, and the spans
// of the clickable items.
func bar(left, right []item, w int) (string, []span) {
	width := func(items []item) int {
		n := 0
		for i, it := range items {
			if i > 0 {
				n++
			}
			n += ansi.StringWidth(it.text)
		}
		return n
	}
	for len(right) > 0 && width(right) > w {
		right = right[:len(right)-1]
	}
	room := w - width(right)
	if len(right) > 0 {
		room-- // at least one space between the groups
	}
	for len(left) > 1 && width(left) > room {
		left = left[:len(left)-1]
	}
	var b strings.Builder
	var spans []span
	x := 0
	put := func(i int, it item) {
		if i > 0 {
			b.WriteByte(' ')
			x++
		}
		text := it.text
		if left := w - x; ansi.StringWidth(text) > left {
			text = ansi.Truncate(text, max(left, 0), "…")
		}
		n := ansi.StringWidth(text)
		if it.action != "" {
			spans = append(spans, span{x, x + n, it.action})
		}
		b.WriteString(it.style.Render(text))
		x += n
	}
	for i, it := range left {
		put(i, it)
	}
	if len(right) > 0 {
		gap := max(1, w-x-width(right))
		b.WriteString(strings.Repeat(" ", gap))
		x += gap
		for i, it := range right {
			put(i, it)
		}
	}
	if x < w {
		b.WriteString(strings.Repeat(" ", w-x))
	}
	return b.String(), spans
}

// Looks shared by the bars.
var (
	labelStyle   = lipgloss.NewStyle().Faint(true)
	brandStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	buttonStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	activeStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	tabStyle     = lipgloss.NewStyle().Faint(true)
	tabOnStyle   = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(lipgloss.Color("12"))
	liveStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
	ruleStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	systemStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	warnLogStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	errLogStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
)

// button is a clickable "[k Label]", its key in bold. A button whose action
// does not apply right now is drawn faint but still answers its key.
func button(key, label, action string, on, enabled bool) item {
	style := buttonStyle
	switch {
	case on:
		style = activeStyle
	case !enabled:
		style = labelStyle
	}
	return item{text: "[" + key + " " + label + "]", style: style, action: action}
}

// check is a clickable "[x] Label k" toggle.
func check(label, key, action string, on bool) item {
	box, style := "[ ]", buttonStyle
	if on {
		box, style = "[x]", activeStyle
	}
	return item{text: box + " " + label + " " + key, style: style, action: action}
}

// topBar is the main column's first row: the config in force, the
// all-services buttons, liveness and the up count.
func (m Model) topBar(w int) (string, []span) {
	path := m.cfgView.Source.Path
	if path == "" {
		path = m.root
	}
	if path == "" {
		path = "(config root unknown)"
	}
	up := 0
	for _, st := range m.statuses {
		if st.Phase.Alive() {
			up++
		}
	}
	left := []item{{text: "CONFIG", style: labelStyle}, {text: path}}
	right := []item{
		button("a", "Start all", "a", false, true),
		button("R", "Restart all", "R", false, up > 0),
		button("S", "Stop all", "S", false, up > 0),
	}
	if m.configChanged && m.reload == nil {
		right = append(right, item{text: "YAML changed · L reloads", style: noticeStyle, action: "L"})
	}
	right = append(right, item{text: fmt.Sprintf("%d/%d up", up, len(m.statuses))}, item{text: "● LIVE", style: liveStyle})
	// The path shrinks first, from its start, then the right group loses its
	// last pieces until the path keeps a readable tail.
	const minPath = 12
	for len(right) > 1 && w-barWidth(right)-1-ansi.StringWidth("CONFIG ") < minPath {
		right = right[:len(right)-1]
	}
	left[1].text = truncateLeft(path, max(w-barWidth(right)-1-ansi.StringWidth("CONFIG "), 1))
	return bar(left, right, w)
}

// barWidth is the drawn width of items laid out one space apart.
func barWidth(items []item) int {
	n := 0
	for i, it := range items {
		if i > 0 {
			n++
		}
		n += ansi.StringWidth(it.text)
	}
	return n
}

// truncateLeft keeps the end of s within w columns, marking the cut: the end
// of a path is the part that tells two checkouts apart.
func truncateLeft(s string, w int) string {
	if ansi.StringWidth(s) <= w {
		return s
	}
	return ansi.TruncateLeft(s, ansi.StringWidth(s)-w+1, "…")
}

// tabBar is the main column's second row: the two tabs, then a count chip per
// phase, which filters the service list like the web console's.
func (m Model) tabBar(w int) (string, []span) {
	out, cfg := tabOnStyle, tabStyle
	if m.showConfig {
		out, cfg = tabStyle, tabOnStyle
	}
	left := []item{
		{text: "OUTPUT", style: out, action: "tab:output"},
		{text: "CONFIG", style: cfg, action: "tab:config"},
		{text: "│", style: ruleStyle},
	}
	count := map[supervisor.Phase]int{}
	for _, st := range m.statuses {
		count[st.Phase]++
	}
	chips := func(compact bool) []item {
		out := make([]item, 0, len(supervisor.Phases()))
		for i, p := range supervisor.Phases() {
			glyph, word := ui.PhaseLabel(p)
			style := phaseStyle(p)
			if count[p] == 0 {
				style = labelStyle
			}
			if m.phaseFilter == p {
				style = style.Reverse(true)
			}
			text := fmt.Sprintf("%d %s %s", count[p], glyph, word)
			if compact {
				// Glyph and count only, so every phase still fits: dropping
				// the last chips dropped "running" and "ready", the two that
				// matter most.
				text = fmt.Sprintf("%s%d", glyph, count[p])
			}
			out = append(out, item{text: text, style: style, action: "key:" + strconv.Itoa(i+1)})
		}
		return out
	}
	right := []item{{text: fmt.Sprintf("%d services", len(m.statuses)), style: labelStyle}}
	full := append(append([]item(nil), left...), chips(false)...)
	if barWidth(full)+1+barWidth(right) <= w {
		return bar(full, right, w)
	}
	compact := append(left, chips(true)...)
	if barWidth(compact)+1+barWidth(right) > w {
		right = nil
	}
	return bar(compact, right, w)
}

// sideLines draws the sidebar (wide) or the service table (narrow) into r.
func (m Model) sideLines(f frame) []string {
	r := f.side
	out := make([]string, 0, r.h)
	if f.wide {
		out = append(out, fit(brandStyle.Render("▣ mabo-ctl")+" "+labelStyle.Render("CONSOLE"), r.w))
		line, _ := m.sideFilterBar(r.w)
		out = append(out, line)
	}
	out = append(out, fit(labelStyle.Render(m.tableHeader()), r.w))
	if !f.wide && (m.nameFiltering || m.nameFilter != "" || m.phaseFilter != "") {
		// Narrow: the filter state rides on the header row.
		line, _ := m.sideFilterBar(r.w)
		out[len(out)-1] = line
	}
	rowsTop := len(out)
	room := r.h - rowsTop
	if f.wide {
		room -= sideFoot
	}
	idx := m.listIndices()
	switch {
	case len(m.statuses) == 0:
		out = append(out, fit(dimStyle.Render("  no services configured"), r.w))
	case len(idx) == 0:
		out = append(out, fit(dimStyle.Render("  no service matches the list filter · f edits it, 0 clears the phase"), r.w))
	}
	start := m.listWindow(idx, room)
	for j := start; j < len(idx) && len(out) < rowsTop+room; j++ {
		i := idx[j]
		out = append(out, m.tableRow(m.statuses[i], i == m.sel, r.w))
	}
	for len(out) < r.h-boolInt(f.wide)*sideFoot {
		out = append(out, strings.Repeat(" ", r.w))
	}
	if f.wide {
		out = append(out, fit(dimStyle.Render("? keys · e activity · q quit"), r.w))
	}
	return out[:min(len(out), r.h)]
}

// sideRowsTop is the first service row of the sidebar, relative to its top.
func (f frame) sideRowsTop() int {
	if f.wide {
		return sideHead
	}
	return 1
}

// boolInt is 1 for true.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// sideFilterBar is the "SERVICES [filter]" row: the name filter as a box to
// click or type into, and the phase filter when one is set.
func (m Model) sideFilterBar(w int) (string, []span) {
	box := "[f filter by name]"
	style := labelStyle
	switch {
	case m.nameFiltering:
		box, style = "[name: "+m.nameDraft+"█]", filterStyle
	case m.nameFilter != "":
		box, style = "[name: "+m.nameFilter+"]", filterStyle
	}
	left := []item{{text: "SERVICES", style: labelStyle}, {text: box, style: style, action: "f"}}
	if m.phaseFilter != "" {
		_, word := ui.PhaseLabel(m.phaseFilter)
		left = append(left, item{text: "phase: " + word + " ✕", style: filterStyle, action: "key:0"})
	}
	if m.nameFilter != "" || m.phaseFilter != "" {
		left = append(left, item{text: fmt.Sprintf("%d of %d", len(m.listIndices()), len(m.statuses)), style: labelStyle})
	}
	return bar(left, nil, w)
}

// Service table columns, after the name.
const (
	colPort   = 5
	colPID    = 6
	colUptime = 6
	colProbe  = 8
)

// tableNameWidth is the name column's width.
func (m Model) tableNameWidth() int {
	n := 7
	for _, st := range m.statuses {
		n = max(n, ansi.StringWidth(st.Name))
	}
	return min(n, 20)
}

// tableHeader is the service table's column header.
func (m Model) tableHeader() string {
	return "   " + pad("SERVICE", m.tableNameWidth()) + " " + lpad("PORT", colPort) + " " + lpad("PID", colPID) +
		" " + lpad("UPTIME", colUptime) + " " + pad("PROBE", colProbe)
}

// tableRow is one service in the table: cursor, phase glyph, name in the
// service's colour, port, pid, uptime and the probe's answer. The detail the
// supervisor attached goes on the card, where there is room to read it.
func (m Model) tableRow(st supervisor.Status, selected bool, w int) string {
	glyph, _ := ui.PhaseLabel(st.Phase)
	cursor := " "
	if selected {
		cursor = cursorStyle.Render("▸")
	}
	port, pid, uptime := "-", "-", "-"
	if st.Port > 0 {
		port = strconv.Itoa(st.Port)
	}
	if st.PID > 0 {
		pid = strconv.Itoa(st.PID)
		uptime = ui.ShortDuration(st.Uptime)
	}
	line := cursor + phaseStyle(st.Phase).Render(glyph) + " " +
		m.nameStyle(st.Name).Render(pad(ansi.Truncate(st.Name, m.tableNameWidth(), "…"), m.tableNameWidth())) + " " +
		lpad(port, colPort) + " " + lpad(pid, colPID) + " " + lpad(uptime, colUptime) + " " + m.probeCell(st)
	if selected {
		return fit(selectedStyle.Render(line), w)
	}
	return fit(line, w)
}

// probeCell is the PROBE column: an operation under way; the probe's HTTP
// status; "none" for a live service with no probe; otherwise the phase in a
// word, so a stopped, exited or failed service never rests on its glyph's
// colour alone.
func (m Model) probeCell(st supervisor.Status) string {
	if kind, ok := m.pending[st.Name]; ok {
		return noticeStyle.Render(string(kind) + "…")
	}
	switch {
	case st.HTTP != 0:
		style := phaseStyle(supervisor.PhaseReady)
		if st.HTTP >= 400 {
			style = errorStyle
		}
		return style.Render(strconv.Itoa(st.HTTP))
	case st.Phase.Alive() && st.Health == "":
		return labelStyle.Render("none")
	}
	_, word := ui.PhaseLabel(st.Phase)
	return phaseStyle(st.Phase).Render(word)
}

// cardLines draws the selected service's card into r: a border carrying the
// name, the phase and the service's buttons; the detail grid; the command
// block's toggle (and the block, when open) with the supervisor's detail.
func (m Model) cardLines(r rect) []string {
	if r.h == 0 {
		return nil
	}
	st := m.statuses[m.sel]
	head, _ := m.cardHead(r.w)
	out := []string{head}
	inner := max(1, r.w-4)
	row := func(s string) string { return ruleStyle.Render("│ ") + fit(s, inner) + ruleStyle.Render(" │") }
	labels, values := m.cardGrid(st, inner)
	if r.h >= 5 {
		out = append(out, row(labelStyle.Render(labels)), row(values))
		toggle := "▸ log file, command & arguments  i"
		if m.showDetail {
			toggle = "▾ log file, command & arguments  i"
		}
		line := labelStyle.Render(toggle)
		if d := detailCell(st); d != "" {
			style := dimStyle
			if st.Phase == supervisor.PhaseFailed || st.Phase == supervisor.PhaseExited || st.Phase == supervisor.PhaseDegraded {
				style = errorStyle
			}
			line += "   " + style.Render(sanitize(d))
		}
		out = append(out, row(line))
		if m.showDetail && r.h > 5 {
			for _, l := range m.cardExtra() {
				out = append(out, row(dimStyle.Render(sanitize(l))))
			}
		}
	} else {
		out = append(out, row(values))
	}
	out = append(out, ruleStyle.Render("└"+strings.Repeat("─", max(0, r.w-2))+"┘"))
	return out[:min(len(out), r.h)]
}

// cardHead is the card's top border: name, phase, then the service's buttons
// flush right inside the corner.
func (m Model) cardHead(w int) (string, []span) {
	st := m.statuses[m.sel]
	glyph, word := ui.PhaseLabel(st.Phase)
	alive := st.Phase.Alive()
	left := []item{
		{text: "┌", style: ruleStyle},
		{text: st.Name, style: m.nameStyle(st.Name).Bold(true)},
		{text: glyph + " " + word, style: phaseStyle(st.Phase)},
	}
	right := []item{
		button("s", "Start", "s", false, !alive),
		button("x", "Stop", "x", false, alive),
		button("r", "Restart", "r", false, true),
		{text: "┐", style: ruleStyle},
	}
	line, spans := bar(left, right, w)
	// Draw the border through the gap between the two groups.
	used := barWidth(left)
	fill := w - used - barWidth(right) - 2
	if fill > 0 {
		line, spans = bar(append(left, item{text: strings.Repeat("─", fill), style: ruleStyle}), right, w)
	}
	return line, spans
}

// cardGrid is the detail grid: a row of labels over a row of values, each
// column as wide as the wider of the two, laid out until the width runs out.
func (m Model) cardGrid(st supervisor.Status, w int) (string, string) {
	cs := m.configService(st.Name)
	or := func(v, alt string) string {
		if v == "" {
			return alt
		}
		return v
	}
	pid, port := "-", "-"
	if st.PID > 0 {
		pid = strconv.Itoa(st.PID)
	}
	if st.Port > 0 {
		port = strconv.Itoa(st.Port)
	}
	usage := "-"
	if st.PID > 0 && st.Usage.RSS > 0 {
		usage = ui.FormatCPU(st.Usage.CPU) + " · " + ui.FormatBytes(st.Usage.RSS)
	}
	health := or(st.Health, "none")
	if st.HTTP != 0 {
		health += " " + strconv.Itoa(st.HTTP)
	}
	cols := [][2]string{
		{"PID", pid},
		{"PORT", port},
		{"UPTIME", or(ui.ShortDuration(st.Uptime), "-")},
		{"READY IN", or(ui.ShortDuration(st.Elapsed), "-")},
		{"CPU · MEM", usage},
		{"RUNTIME", or(cs.Runtime, "system")},
		{"HEALTH", health},
	}
	var labels, values strings.Builder
	used := 0
	for _, c := range cols {
		cw := max(ansi.StringWidth(c[0]), ansi.StringWidth(c[1]))
		if used > 0 && used+3+cw > w {
			break
		}
		if used > 0 {
			labels.WriteString("   ")
			values.WriteString("   ")
			used += 3
		}
		labels.WriteString(pad(c[0], cw))
		values.WriteString(pad(sanitize(c[1]), cw))
		used += cw
	}
	return labels.String(), values.String()
}

// configService is the redacted configuration of the named service.
func (m Model) configService(name string) ui.ConfigService {
	for _, s := range m.cfgView.Services {
		if s.Name == name {
			return s
		}
	}
	return ui.ConfigService{}
}

// cardExtra is the command block the card opens with i: the web console's
// "log file, command & arguments".
func (m Model) cardExtra() []string {
	st := m.statuses[m.sel]
	cs := m.configService(st.Name)
	or := func(v, alt string) string {
		if v == "" {
			return alt
		}
		return v
	}
	env := make([]string, 0, len(cs.Env))
	for _, v := range cs.Env {
		env = append(env, v.Key+"="+v.Value)
	}
	return []string{
		"log   " + or(st.LogPath, "-"),
		"cmd   " + or(cs.CmdLine, "-"),
		"dir   " + or(cs.Dir, "-"),
		"deps  " + or(strings.Join(cs.DependsOn, ", "), "none"),
		"env   " + or(strings.Join(env, " "), "none"),
	}
}

// toolBar is the output toolbar's first row: what the pane shows and its
// state, the merge, pause, follow and clear buttons, and the filter box.
func (m Model) toolBar(w int) (string, []span) {
	svc := m.tailSvc
	if svc == "" || svc == allKey {
		svc = m.selectedName()
	}
	label := "OUTPUT · " + svc
	if m.all {
		label = "OUTPUT · all services"
	}
	labelSt := labelStyle
	if m.focus == focusLog {
		label = "▶ " + label
		labelSt = activeStyle
	}
	state := item{text: "● live", style: liveStyle}
	switch {
	case m.paused:
		state = item{text: fmt.Sprintf("❚❚ paused +%d", m.pausedNew), style: noticeStyle, action: "p"}
	case !m.follow:
		n := len(m.rows())
		state = item{text: fmt.Sprintf("line %d/%d", m.viewOffset(n)+1, max(n, 1)), style: labelStyle, action: "follow"}
	}
	box, boxSt := "[/ filter lines…]", labelStyle
	switch {
	case m.filtering:
		box, boxSt = "[/ "+m.filterDraft+"█]", filterStyle
	case m.filter != "":
		box, boxSt = "[/ "+m.filter+"]", filterStyle
	}
	left := []item{
		{text: label, style: labelSt, action: "focus"},
		state,
		button("m", "All services", "m", m.all, true),
		button("p", "Pause", "p", m.paused, true),
		button("G", "Follow tail", "follow", m.follow && !m.paused, true),
		button("c", "Clear", "c", false, true),
		{text: box, style: boxSt, action: "/"},
	}
	return bar(left, nil, w)
}

// levelBar is the output toolbar's second row: the level chips, the
// separators and pretty-JSON toggles, and the line count.
func (m Model) levelBar(w int) (string, []span) {
	chip := func(text, level string) item {
		if m.levelFilter == level {
			return item{text: "[✓ " + text + "]", style: activeStyle, action: "level:" + level}
		}
		return item{text: "[" + text + "]", style: buttonStyle, action: "level:" + level}
	}
	left := []item{
		{text: "v", style: labelStyle, action: "v"},
		chip("All", ""),
		chip("Err", "error"),
		chip("Warn", "warn"),
		chip("Info", "info"),
		chip("Dbg", "debug"),
		{text: " ", style: labelStyle},
		check("Separators", "|", "|", m.separators),
		check("Pretty JSON", "J", "J", m.pretty),
	}
	right := []item{{text: fmt.Sprintf("%d lines", len(m.shownRecords())), style: labelStyle}}
	return bar(left, right, w)
}

// systemLine is the strip above the log: the reload question while it is
// open, else the newest error, else the newest event — what the web console
// calls its system commands.
func (m Model) systemLine(w int) string {
	label := systemStyle.Render("SYSTEM") + " "
	room := max(1, w-7)
	if m.confirmReload {
		return fit(label+errorStyle.Render(ansi.Truncate(
			"Reload mabo-ctl.yaml? y reloads, any other key cancels · changed running services restart, removed ones stop", room, "…")), w)
	}
	for _, err := range []error{m.err, m.reloadErr} {
		if err == nil {
			continue
		}
		text := errRenderer.Error(err)
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[:i] + " …"
		}
		return fit(label+errorStyle.Render(ansi.Truncate(sanitize(text), room, "…")), w)
	}
	if m.notice != "" {
		return fit(label+noticeStyle.Render(ansi.Truncate(sanitize(m.notice), room, "…")), w)
	}
	return fit(label+dimStyle.Render("no commands yet · e shows the activity history"), w)
}

// logLines draws the log rows into r, coloured by level as the web console
// colours them, padded to r's height.
func (m Model) logLines(r rect) []string {
	visible := m.rows()
	out := make([]string, 0, r.h)
	if len(visible) == 0 {
		msg := "  (no output yet)"
		if (m.filter != "" || m.levelFilter != "") && len(m.recs) > 0 {
			msg = fmt.Sprintf("  (no line matches the filter; %d hidden)", len(m.recs))
		}
		out = append(out, fit(dimStyle.Render(msg), r.w))
	}
	labelW := 0
	if m.all {
		for _, st := range m.statuses {
			labelW = max(labelW, ansi.StringWidth(st.Name))
		}
	}
	start := m.viewOffset(len(visible))
	for i := start; i < len(visible) && len(out) < r.h; i++ {
		out = append(out, fit(m.logRowView(visible[i], labelW, r.w), r.w))
	}
	for len(out) < r.h {
		out = append(out, strings.Repeat(" ", r.w))
	}
	return out[:r.h]
}

// levelStyle colours a log row by its record's level.
func levelStyle(lvl string) (lipgloss.Style, bool) {
	switch lvl {
	case "error":
		return errLogStyle, true
	case "warn":
		return warnLogStyle, true
	case "debug":
		return dimStyle, true
	}
	return lipgloss.Style{}, false
}

// configLines draws the CONFIG tab into r: the same view `mabo-ctl config`
// prints, redacted, then the start order, scrolled to cfgOffset.
func (m Model) configLines(r rect) []string {
	text := "this console cannot describe its configuration"
	if m.cfgView.Source.Path != "" {
		rd := &ui.Renderer{}
		text = rd.ConfigBlock(m.cfgView, "") + "\n\n" + rd.ConfigGraph(m.cfgView)
	}
	body := strings.Split(text, "\n")
	out := []string{fit(labelStyle.Render("↑/↓ pgup/pgdn scroll · L reloads · C or esc returns to OUTPUT"), r.w)}
	room := max(0, r.h-1)
	off := min(max(m.cfgOffset, 0), max(0, len(body)-room))
	for i := off; i < len(body) && len(out) < r.h; i++ {
		out = append(out, fit(sanitize(body[i]), r.w))
	}
	for len(out) < r.h {
		out = append(out, strings.Repeat(" ", r.w))
	}
	return out[:r.h]
}

// fit makes s exactly w columns: cut with an ellipsis when wider, padded with
// spaces when narrower. s may carry style escapes.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	n := ansi.StringWidth(s)
	if n > w {
		s = ansi.Truncate(s, w, "…")
		n = ansi.StringWidth(s)
	}
	return s + strings.Repeat(" ", max(0, w-n))
}

// lpad right-aligns s in w columns.
func lpad(s string, w int) string {
	if n := w - ansi.StringWidth(s); n > 0 {
		return strings.Repeat(" ", n) + s
	}
	return s
}
