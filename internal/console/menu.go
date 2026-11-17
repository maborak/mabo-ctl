package console

import (
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// menuItem is one entry of a popup menu: its label, the key that runs it from
// the menu or from anywhere, and the action it runs. A header item is a
// caption the cursor skips.
type menuItem struct {
	label, key, action string
	header             bool
}

// menu is a popup — htop's F9 signal list, F6 sort list — over the console:
// ↑/↓ move, enter runs, a listed key runs its item directly, esc closes.
type menu struct {
	title  string
	items  []menuItem
	cursor int
}

// openActions opens the actions menu for the selected service: its own
// operations first, then the ones over every service, then the reload. The
// cursor starts on what the service most likely needs: Start when it is down,
// Restart when it is up.
func (m Model) openActions() (tea.Model, tea.Cmd) {
	svc := m.selectedName()
	items := []menuItem{}
	if svc != "" {
		items = append(items,
			menuItem{label: "this service", header: true},
			menuItem{label: "Start " + svc, key: "s", action: "s"},
			menuItem{label: "Stop " + svc, key: "x", action: "x"},
			menuItem{label: "Restart " + svc, key: "r", action: "r"},
			menuItem{label: "Show detail", key: "i", action: "pane:detail"},
		)
	}
	items = append(items,
		menuItem{label: "every service", header: true},
		menuItem{label: "Start all", key: "a", action: "a"},
		menuItem{label: "Restart running", key: "R", action: "R"},
		menuItem{label: "Stop all", key: "S", action: "S"},
		menuItem{label: "console", header: true},
		menuItem{label: "Reload mabo-ctl.yaml", key: "L", action: "L"},
		menuItem{label: "Activity history", key: "e", action: "e"},
	)
	mn := &menu{title: "Actions", items: items}
	want := "s"
	if svc != "" && m.statuses[m.sel].Phase.Alive() {
		want = "r"
	}
	mn.cursor = mn.find(want)
	m.menu = mn
	return m, nil
}

// sortKeys are the table's sort orders, in the sort menu's order.
var sortKeys = []struct{ key, label string }{
	{"", "Declared order"},
	{"name", "Name"},
	{"state", "State (worst first)"},
	{"port", "Port"},
	{"pid", "PID"},
	{"uptime", "Uptime (longest first)"},
	{"cpu", "CPU% (highest first)"},
	{"mem", "Memory (largest first)"},
}

// openSort opens the sort menu with the cursor on the order in force.
func (m Model) openSort() (tea.Model, tea.Cmd) {
	mn := &menu{title: "Sort by"}
	for i, k := range sortKeys {
		mn.items = append(mn.items, menuItem{label: k.label, action: "sort:" + k.key})
		if k.key == m.sortBy {
			mn.cursor = i
		}
	}
	m.menu = mn
	return m, nil
}

// find is the index of the item with key, or the first selectable item.
func (mn *menu) find(key string) int {
	first := -1
	for i, it := range mn.items {
		if it.header {
			continue
		}
		if first < 0 {
			first = i
		}
		if it.key == key {
			return i
		}
	}
	return max(first, 0)
}

// step moves the cursor by delta over the selectable items, wrapping.
func (mn *menu) step(delta int) {
	n := len(mn.items)
	for i := 1; i <= n; i++ {
		c := ((mn.cursor+delta*i)%n + n) % n
		if !mn.items[c].header {
			mn.cursor = c
			return
		}
	}
}

// menuKey handles a keypress while a menu is open.
func (m Model) menuKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	mn := m.menu
	switch k := msg.String(); k {
	case "up", "k":
		mn.step(-1)
		return m, nil
	case "down", "j", "tab":
		mn.step(1)
		return m, nil
	case "enter", " ":
		return m.runMenu(mn.cursor)
	case "esc", "f9", "f6", "left", "h":
		m.menu = nil
		return m, nil
	case "q", "f10", "ctrl+c":
		m.menu = nil
		return m.requestQuit()
	default:
		for i, it := range mn.items {
			if !it.header && it.key != "" && it.key == k {
				return m.runMenu(i)
			}
		}
	}
	return m, nil
}

// runMenu closes the menu and runs item i's action.
func (m Model) runMenu(i int) (tea.Model, tea.Cmd) {
	mn := m.menu
	m.menu = nil
	if i < 0 || i >= len(mn.items) || mn.items[i].header {
		return m, nil
	}
	return m.act(mn.items[i].action)
}

// menuBox is the menu drawn as a box, and where it sits: centred over the
// pane, or over the screen when the pane is too short.
func (m Model) menuBox() ([]string, rect) {
	mn := m.menu
	inner := ansi.StringWidth(mn.title) + 4
	for _, it := range mn.items {
		inner = max(inner, ansi.StringWidth(it.label)+6)
	}
	inner = min(inner, max(10, m.width-4))
	lines := []string{menuStyle.Render(fit("┌─ "+mn.title+" "+strings.Repeat("─", max(0, inner-len([]rune(mn.title))-3))+"┐", inner+2))}
	for i, it := range mn.items {
		if it.header {
			lines = append(lines, menuStyle.Render("│ ")+labelStyle.Render(fit("── "+it.label, inner-1))+menuStyle.Render("│"))
			continue
		}
		style := menuStyle
		if i == mn.cursor {
			style = menuSelStyle
		}
		label := fit(" "+it.label, inner-4) + pad(it.key, 3) + " "
		lines = append(lines, menuStyle.Render("│")+style.Render(fit(label, inner))+menuStyle.Render("│"))
	}
	lines = append(lines, menuStyle.Render("└"+strings.Repeat("─", inner)+"┘"))
	w, h := inner+2, len(lines)
	f := m.frame()
	area := f.pane
	if area.h < h {
		area = rect{0, 0, m.width, m.height}
	}
	x := area.x + max(0, (area.w-w)/2)
	y := area.y + max(0, (area.h-h)/2)
	return lines, rect{x, y, w, h}
}

// overlay draws box over lines at r, keeping what is left and right of it.
func overlay(lines, box []string, r rect) []string {
	for i, b := range box {
		y := r.y + i
		if y < 0 || y >= len(lines) {
			continue
		}
		line := lines[y]
		left := ansi.Truncate(line, r.x, "")
		left += strings.Repeat(" ", max(0, r.x-ansi.StringWidth(left)))
		right := ansi.TruncateLeft(line, r.x+r.w, "")
		// A wide character cut by either edge leaves the row a column long
		// or short; fitting it to the row's width puts that right.
		lines[y] = fit(left+b+right, ansi.StringWidth(line))
	}
	return lines
}

// stateRank orders phases worst first for the State sort.
var stateRank = map[supervisor.Phase]int{
	supervisor.PhaseFailed:   0,
	supervisor.PhaseExited:   1,
	supervisor.PhaseDegraded: 2,
	supervisor.PhaseSlow:     3,
	supervisor.PhaseRunning:  4,
	supervisor.PhaseReady:    5,
	supervisor.PhaseStopped:  6,
}

// sortIndices orders idx, indices into statuses, by the sort in force. The
// sort is stable, so ties keep the declared order.
func (m Model) sortIndices(idx []int) {
	if m.sortBy == "" {
		return
	}
	less := func(a, b int) bool {
		x, y := m.statuses[a], m.statuses[b]
		switch m.sortBy {
		case "name":
			return x.Name < y.Name
		case "state":
			return stateRank[x.Phase] < stateRank[y.Phase]
		case "port":
			return x.Port < y.Port
		case "pid":
			return x.PID < y.PID
		case "uptime":
			return x.Uptime > y.Uptime
		case "cpu":
			return x.Usage.CPU > y.Usage.CPU
		case "mem":
			return x.Usage.RSS > y.Usage.RSS
		}
		return false
	}
	sort.SliceStable(idx, func(i, j int) bool { return less(idx[i], idx[j]) })
}
