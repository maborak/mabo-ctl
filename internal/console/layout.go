package console

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/ui"
)

// The console is organised the way htop and glances are, top to bottom:
//
//	header      the project, the phase counts, liveness
//	meters      the stack's CPU and memory, the sort and filters, the config
//	table       one row per service under a column header, with a selection
//	            bar the arrow keys move
//	pane        the selected service's OUTPUT or DETAIL, or the CONFIG, under
//	            a header that says which and how it is filtered
//	notice      the newest operation or error
//	key bar     F1 … F10, each with a letter, as htop's bottom row
//
// Everything is laid out by [Model.frame], a pure function of the model. View
// draws into its regions and the mouse handler hit-tests the same regions and
// the same spans, so a click always lands on what is drawn.

// rect is a region of the terminal, in cells.
type rect struct{ x, y, w, h int }

// has reports whether the cell (x, y) is inside r.
func (r rect) has(x, y int) bool {
	return x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h
}

// frame is where every part of the console sits for the current size. A part
// that does not show has a zero height.
type frame struct {
	head, meters, thead, table, phead, pane, notice, keys rect
}

// frame lays the console out for the model's size. The table takes the rows
// its services need, up to two fifths of what is left, and the pane takes the
// rest; a terminal too short for the meters loses them first.
func (m Model) frame() frame {
	w, h := max(m.width, 1), max(m.height, 1)
	var f frame
	y := 0
	row := func(r *rect, n int) {
		*r = rect{0, y, w, n}
		y += n
	}
	row(&f.head, 1)
	if h >= 12 {
		row(&f.meters, 1)
	}
	row(&f.thead, 1)
	avail := max(2, h-y-3) // pane header, notice, key bar
	n := max(1, len(m.listIndices()))
	row(&f.table, min(n, max(1, avail*2/5)))
	row(&f.phead, 1)
	row(&f.pane, max(1, h-y-2))
	row(&f.notice, 1)
	row(&f.keys, 1)
	return f
}

// span is the columns [x0, x1) of a clickable item in a row, and what a click
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
// w columns, one space apart. Items that do not fit are dropped — the right
// group's last first, then the left group's — and it returns the line,
// exactly w columns wide, with the spans of the clickable items.
func bar(left, right []item, w int) (string, []span) {
	for len(right) > 0 && barWidth(right) > w {
		right = right[:len(right)-1]
	}
	room := w - barWidth(right)
	if len(right) > 0 {
		room--
	}
	for len(left) > 1 && barWidth(left) > room {
		left = left[:len(left)-1]
	}
	var b strings.Builder
	var spans []span
	x := 0
	limit := room // the left group stops where the right one begins
	put := func(i int, it item) {
		if i > 0 {
			b.WriteByte(' ')
			x++
		}
		text := it.text
		if rest := limit - x; ansi.StringWidth(text) > rest {
			text = ansi.Truncate(text, max(rest, 0), "…")
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
		gap := max(1, w-x-barWidth(right))
		b.WriteString(strings.Repeat(" ", gap))
		x += gap
		limit = w
		for i, it := range right {
			put(i, it)
		}
	}
	if x < w {
		b.WriteString(strings.Repeat(" ", w-x))
	}
	return b.String(), spans
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

// Looks. Colours follow htop's: a green column header, a cyan selection bar,
// cyan function-key labels.
var (
	labelStyle   = lipgloss.NewStyle().Faint(true)
	brandStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	liveStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
	theadStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("2"))
	theadOnStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("6"))
	selStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("6"))
	selIdleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("240"))
	paneOnStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	keyNumStyle  = lipgloss.NewStyle().Bold(true)
	keyLblStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("6"))
	menuStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("236"))
	menuSelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("6"))
	ruleStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	warnLogStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	errLogStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
)

// headLine is the first row: the project, the counts per phase, liveness.
func (m Model) headLine(w int) string {
	project := filepath.Base(m.root)
	if m.root == "" {
		project = "(config root unknown)"
	}
	count := map[supervisor.Phase]int{}
	for _, st := range m.statuses {
		count[st.Phase]++
	}
	left := []item{
		{text: "mabo-ctl", style: brandStyle},
		{text: "· " + project},
		{text: " " + strconv.Itoa(len(m.statuses)) + " services", style: labelStyle},
	}
	for _, p := range supervisor.Phases() {
		if count[p] == 0 {
			continue
		}
		glyph, word := ui.PhaseLabel(p)
		left = append(left, item{text: fmt.Sprintf("%s %d %s", glyph, count[p], word), style: phaseStyle(p)})
	}
	var right []item
	if m.configChanged && m.reload == nil {
		right = append(right, item{text: "YAML changed · L reloads", style: noticeStyle})
	}
	up := 0
	for _, st := range m.statuses {
		if st.Phase.Alive() {
			up++
		}
	}
	right = append(right, item{text: fmt.Sprintf("%d/%d up", up, len(m.statuses))}, item{text: "● LIVE", style: liveStyle})
	line, _ := bar(left, right, w)
	return line
}

// meterLine is the second row: the stack's CPU as a meter, its memory, the
// sort and the list filters in force, and the config file.
func (m Model) meterLine(w int) string {
	var cpu float64
	var rss int64
	for _, st := range m.statuses {
		if st.PID > 0 {
			cpu += st.Usage.CPU
			rss += st.Usage.RSS
		}
	}
	const meterW = 20
	fill := min(meterW, int(cpu/100*meterW+0.5))
	style := liveStyle
	switch {
	case cpu >= 90:
		style = errorStyle
	case cpu >= 50:
		style = noticeStyle
	}
	value := lpad(ui.FormatCPU(cpu), 6)
	plain := "CPU[" + strings.Repeat("|", fill) + strings.Repeat(" ", meterW-fill) + value + "]"
	left := []item{{text: plain}, {text: " MEM " + ui.FormatBytes(rss)}}
	var state []string
	if m.sortBy != "" {
		state = append(state, "sort "+m.sortBy)
	}
	if m.nameFilter != "" || m.nameFiltering {
		name := m.nameFilter
		if m.nameFiltering {
			name = m.nameDraft + "█"
		}
		state = append(state, "name: "+name)
	}
	if m.phaseFilter != "" {
		_, word := ui.PhaseLabel(m.phaseFilter)
		state = append(state, "phase: "+word)
	}
	if len(state) > 0 {
		left = append(left, item{text: " " + strings.Join(state, " · "), style: filterStyle})
	}
	path := m.cfgView.Source.Path
	if path == "" {
		path = m.root
	}
	right := []item{{text: "config " + truncateLeft(path, max(12, w-barWidth(left)-9)), style: labelStyle}}
	line, _ := bar(left, right, w)
	// The meter is laid out as plain text, so its width is exact, then
	// coloured in place.
	meter := "CPU[" + style.Render(strings.Repeat("|", fill)) + strings.Repeat(" ", meterW-fill) + labelStyle.Render(value) + "]"
	return strings.Replace(line, plain, meter, 1)
}

// truncateLeft keeps the end of s within w columns, marking the cut: the end
// of a path is the part that tells two checkouts apart.
func truncateLeft(s string, w int) string {
	if ansi.StringWidth(s) <= w {
		return s
	}
	return ansi.TruncateLeft(s, ansi.StringWidth(s)-w+1, "…")
}

// column is one column of the service table.
type column struct {
	title string
	width int
	right bool
	sort  string
}

// columns are the table's columns for a terminal w wide. DETAIL takes what is
// left; a narrow terminal drops columns from the right first.
func (m Model) columns(w int) []column {
	nameW := 7
	for _, st := range m.statuses {
		nameW = max(nameW, ansi.StringWidth(st.Name))
	}
	cols := []column{
		{"SERVICE", min(nameW, 24), false, "name"},
		{"STATE", 10, false, "state"},
		{"PORT", 5, true, "port"},
		{"PID", 6, true, "pid"},
		{"UPTIME", 7, true, "uptime"},
		{"CPU%", 6, true, "cpu"},
		{"MEM", 6, true, "mem"},
		{"PROBE", 6, false, ""},
	}
	used := 1 // the leading space
	for i, c := range cols {
		if used+c.width+1 > w {
			cols = cols[:i]
			break
		}
		used += c.width + 1
	}
	if rest := w - used; rest >= 8 {
		cols = append(cols, column{"DETAIL", rest, false, ""})
	}
	return cols
}

// cells lays texts into the columns: one leading space, one between columns,
// each text cut or padded to its column. styles, when given, colour a cell.
func cells(cols []column, texts []string, styles []lipgloss.Style) string {
	var b strings.Builder
	b.WriteByte(' ')
	for i, c := range cols {
		if i > 0 {
			b.WriteByte(' ')
		}
		t := ansi.Truncate(texts[i], c.width, "…")
		if c.right {
			t = lpad(t, c.width)
		} else {
			t = pad(t, c.width)
		}
		if styles != nil {
			t = styles[i].Render(t)
		}
		b.WriteString(t)
	}
	return b.String()
}

// theadLine is the table's column header, green as htop's, with the sort
// column picked out. The spans let a click sort by a column.
func (m Model) theadLine(w int) (string, []span) {
	cols := m.columns(w)
	var b strings.Builder
	var spans []span
	b.WriteString(theadStyle.Render(" "))
	x := 1
	for i, c := range cols {
		if i > 0 {
			b.WriteString(theadStyle.Render(" "))
			x++
		}
		title, style := c.title, theadStyle
		if c.sort != "" && c.sort == m.sortBy {
			title, style = title+"▼", theadOnStyle
		}
		title = ansi.Truncate(title, c.width, "")
		if c.right {
			title = lpad(title, c.width)
		} else {
			title = pad(title, c.width)
		}
		b.WriteString(style.Render(title))
		if c.sort != "" {
			spans = append(spans, span{x, x + c.width, "sort:" + c.sort})
		}
		x += c.width
	}
	if x < w {
		b.WriteString(theadStyle.Render(strings.Repeat(" ", w-x)))
	}
	return b.String(), spans
}

// tableLines draws the service rows into r, scrolled so the selection shows.
func (m Model) tableLines(r rect) []string {
	idx := m.listIndices()
	out := make([]string, 0, r.h)
	switch {
	case len(m.statuses) == 0:
		out = append(out, fit(dimStyle.Render("  no services configured"), r.w))
	case len(idx) == 0:
		out = append(out, fit(dimStyle.Render("  no service matches the filter · F4 or f edits the name, 0 clears the phase"), r.w))
	}
	cols := m.columns(r.w)
	start := m.listWindow(idx, r.h)
	for j := start; j < len(idx) && len(out) < r.h; j++ {
		i := idx[j]
		out = append(out, m.tableRow(cols, m.statuses[i], i == m.sel, r.w))
	}
	for len(out) < r.h {
		out = append(out, strings.Repeat(" ", r.w))
	}
	return out
}

// tableRow is one service. The selected row is drawn as htop's selection bar
// across the whole width — cyan while the table has the keyboard, grey while
// the pane does — and carries no other colour, so the bar reads as one piece.
func (m Model) tableRow(cols []column, st supervisor.Status, selected bool, w int) string {
	glyph, word := ui.PhaseLabel(st.Phase)
	port, pid, uptime, cpu, mem := "-", "-", "-", "-", "-"
	if st.Port > 0 {
		port = strconv.Itoa(st.Port)
	}
	if st.PID > 0 {
		pid = strconv.Itoa(st.PID)
		uptime = ui.ShortDuration(st.Uptime)
		if st.Usage.RSS > 0 {
			cpu = strconv.FormatFloat(st.Usage.CPU, 'f', 1, 64)
			mem = ui.FormatBytes(st.Usage.RSS)
		}
	}
	probe, probeSt := probeCell(st)
	detail := sanitize(detailCell(st))
	if kind, ok := m.pending[st.Name]; ok {
		detail = strings.TrimSpace(string(kind) + "… " + detail)
	}
	texts := []string{st.Name, glyph + " " + word, port, pid, uptime, cpu, mem, probe, detail}[:len(cols)]
	if selected {
		style := selStyle
		if m.focus != focusList {
			style = selIdleStyle
		}
		return style.Render(fit(cells(cols, texts, nil), w))
	}
	detailSt := dimStyle
	switch st.Phase {
	case supervisor.PhaseFailed, supervisor.PhaseExited, supervisor.PhaseDegraded:
		detailSt = errorStyle
	}
	plain := lipgloss.NewStyle()
	styles := []lipgloss.Style{m.nameStyle(st.Name), phaseStyle(st.Phase), plain, plain, plain, plain, plain, probeSt, detailSt}
	return fit(cells(cols, texts, styles[:len(cols)]), w)
}

// probeCell is the PROBE column and its colour: the probe's HTTP status,
// "none" for a live service with no probe, "…" for one still waiting.
func probeCell(st supervisor.Status) (string, lipgloss.Style) {
	switch {
	case st.HTTP >= 400:
		return strconv.Itoa(st.HTTP), errorStyle
	case st.HTTP != 0:
		return strconv.Itoa(st.HTTP), phaseStyle(supervisor.PhaseReady)
	case st.Phase.Alive() && st.Health == "":
		return "none", labelStyle
	case st.Phase.Alive():
		return "…", noticeStyle
	}
	return "-", labelStyle
}

// paneNames are the three panes as the pane header lists them.
var paneNames = []struct {
	pane   paneKind
	name   string
	action string
}{
	{paneOutput, "Output", "pane:output"},
	{paneDetail, "Detail", "pane:detail"},
	{paneConfig, "Config", "pane:config"},
}

// pheadLine is the pane's header: which service and pane, the output's state
// and filters, and the three panes to switch between. It is drawn bright
// while the pane has the keyboard (Tab moves it).
func (m Model) pheadLine(w int) (string, []span) {
	svc := m.selectedName()
	if m.all && m.pane == paneOutput {
		svc = "all services"
	}
	style := labelStyle
	if m.focus == focusLog {
		style = paneOnStyle
	}
	left := []item{{text: "─ " + svc + " · " + strings.ToUpper(paneNames[m.pane].name), style: style, action: "focus"}}
	if m.pane == paneOutput {
		var flags []string
		switch {
		case m.paused:
			flags = append(flags, fmt.Sprintf("PAUSED +%d", m.pausedNew))
		case m.follow:
			flags = append(flags, "following")
		default:
			n := len(m.rows())
			flags = append(flags, fmt.Sprintf("line %d/%d", m.viewOffset(n)+1, max(n, 1)))
		}
		level := "all"
		if m.levelFilter != "" {
			level = m.levelFilter
		}
		flags = append(flags, "level "+level)
		if m.pretty {
			flags = append(flags, "pretty")
		}
		if m.separators {
			flags = append(flags, "sep")
		}
		left = append(left, item{text: "─ " + strings.Join(flags, " · "), style: style})
		switch {
		case m.filtering:
			left = append(left, item{text: "─ filter: " + m.filterDraft + "█", style: filterStyle, action: "/"})
		case m.filter != "":
			left = append(left, item{text: "─ filter: " + m.filter, style: filterStyle, action: "/"})
		}
	}
	var right []item
	for _, p := range paneNames {
		st := labelStyle
		if p.pane == m.pane {
			st = paneOnStyle.Underline(true)
		}
		right = append(right, item{text: p.name, style: st, action: p.action})
	}
	right = append(right, item{text: "Tab ─", style: labelStyle, action: "focus"})
	if fill := w - barWidth(left) - barWidth(right) - 2; fill > 0 {
		left = append(left, item{text: strings.Repeat("─", fill), style: ruleStyle})
	}
	return bar(left, right, w)
}

// paneLines draws the pane into r.
func (m Model) paneLines(r rect) []string {
	if m.pane == paneOutput {
		return m.logLines(r)
	}
	text := m.paneText()
	out := make([]string, 0, r.h)
	off := min(max(m.paneOffset, 0), max(0, len(text)-r.h))
	for i := off; i < len(text) && len(out) < r.h; i++ {
		out = append(out, fit(text[i], r.w))
	}
	for len(out) < r.h {
		out = append(out, strings.Repeat(" ", r.w))
	}
	return out
}

// paneText is the DETAIL or CONFIG pane's text, one entry per row.
func (m Model) paneText() []string {
	switch m.pane {
	case paneDetail:
		return m.detailText()
	case paneConfig:
		if m.cfgView.Source.Path == "" {
			return []string{" this console cannot describe its configuration"}
		}
		r := &ui.Renderer{}
		lines := strings.Split(r.ConfigBlock(m.cfgView, "")+"\n\n"+r.ConfigGraph(m.cfgView), "\n")
		for i, l := range lines {
			lines[i] = " " + sanitize(l)
		}
		return lines
	}
	return nil
}

// detailText is the DETAIL pane: everything the web console's service card
// shows, one fact a row, labels in a column. Everything comes from the
// redacted status snapshot and the redacted configuration view.
func (m Model) detailText() []string {
	if m.selectedName() == "" {
		return []string{" no service selected"}
	}
	st := m.statuses[m.sel]
	cs := m.configService(st.Name)
	or := func(v, alt string) string {
		if v == "" {
			return alt
		}
		return v
	}
	glyph, word := ui.PhaseLabel(st.Phase)
	pid, port := "-", "-"
	if st.PID > 0 {
		pid = strconv.Itoa(st.PID)
	}
	if st.Port > 0 {
		port = strconv.Itoa(st.Port)
	}
	usage := "-"
	if st.PID > 0 && st.Usage.RSS > 0 {
		usage = ui.FormatCPU(st.Usage.CPU) + " cpu · " + ui.FormatBytes(st.Usage.RSS) + " mem"
	}
	health := or(st.Health, "no probe declared")
	if st.HTTP != 0 {
		health += " → HTTP " + strconv.Itoa(st.HTTP)
	}
	env := make([]string, 0, len(cs.Env))
	for _, v := range cs.Env {
		env = append(env, v.Key+"="+v.Value)
	}
	row := func(label, value string) string {
		return " " + labelStyle.Render(pad(label, 10)) + " " + sanitize(value)
	}
	out := []string{
		" " + m.nameStyle(st.Name).Bold(true).Render(st.Name) + "  " + phaseStyle(st.Phase).Render(glyph+" "+word),
		"",
		row("pid", pid),
		row("port", port),
		row("uptime", or(ui.ShortDuration(st.Uptime), "-")),
		row("ready in", or(ui.ShortDuration(st.Elapsed), "-")),
		row("usage", usage),
		row("health", health),
		row("runtime", or(cs.Runtime, "system")),
		row("log", or(st.LogPath, "-")),
		row("cmd", or(cs.CmdLine, "-")),
		row("dir", or(cs.Dir, "-")),
		row("deps", or(strings.Join(cs.DependsOn, ", "), "none")),
		row("env", or(strings.Join(env, " "), "none")),
	}
	if d := strings.TrimSpace(st.Detail); d != "" {
		out = append(out, "", " "+labelStyle.Render("detail"))
		for _, l := range strings.Split(d, "\n") {
			out = append(out, "   "+sanitize(l))
		}
	}
	return out
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

// noticeLine is the row above the key bar: the reload question while it is
// open, else the newest error, else the newest event. An error is never
// dropped silently — that is the failure mode this tool exists to stop.
func (m Model) noticeLine(w int) string {
	if m.confirmReload {
		return fit(errorStyle.Render(" Reload mabo-ctl.yaml? y reloads, any other key cancels · changed running services restart, removed ones stop"), w)
	}
	for _, err := range []error{m.err, m.reloadErr} {
		if err == nil {
			continue
		}
		text := errRenderer.Error(err)
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[:i] + " …"
		}
		return fit(errorStyle.Render(" "+sanitize(text)), w)
	}
	if m.notice != "" {
		return fit(noticeStyle.Render(" "+sanitize(m.notice)), w)
	}
	return fit(dimStyle.Render(" ready · F1 or ? help · F9 or enter: actions for the selected service"), w)
}

// fkeys is the key bar, htop's bottom row: each function key, its label, and
// the letter that does the same where the terminal keeps the F-keys.
var fkeys = []struct{ key, label, letter string }{
	{"F1", "Help", "?"},
	{"F2", "Config", "C"},
	{"F3", "Search", "/"},
	{"F4", "Filter", "f"},
	{"F5", "Merge", "m"},
	{"F6", "Sort", "<"},
	{"F7", "Detail", "i"},
	{"F8", "Pause", "p"},
	{"F9", "Action", "enter"},
	{"F10", "Quit", "q"},
}

// keyBar draws the key bar into w columns: htop's "F1Help  F2Setup …", the key
// plain and the label on cyan. The labels narrow before any key is dropped.
func (m Model) keyBar(w int) (string, []span) {
	labelW := 7 // htop's six, plus a column between the labels
	total := func(lw int) int {
		n := 0
		for _, k := range fkeys {
			n += len(k.key) + lw
		}
		return n
	}
	for labelW > 2 && total(labelW) > w {
		labelW--
	}
	var b strings.Builder
	var spans []span
	x := 0
	for _, k := range fkeys {
		cell := len(k.key) + labelW
		if x+cell > w {
			break
		}
		b.WriteString(keyNumStyle.Render(k.key))
		b.WriteString(keyLblStyle.Render(pad(ansi.Truncate(k.label, labelW, ""), labelW)))
		spans = append(spans, span{x, x + cell, strings.ToLower(k.key)})
		x += cell
	}
	if x < w {
		b.WriteString(keyLblStyle.Render(strings.Repeat(" ", w-x)))
	}
	return b.String(), spans
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
