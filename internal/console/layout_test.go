package console

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// The tests in this file cover the htop-shaped console: header, meters,
// table, pane, notice and key bar; the function keys; the action and sort
// menus; and the mouse driving the same actions.

// sized resizes m.
func sized(t *testing.T, m Model, w, h int) Model {
	t.Helper()
	m, _ = send(t, m, tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

// clickAt is a left click at (x, y).
func clickAt(x, y int) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}
}

// spanFor finds the clickable span with action in spans.
func spanFor(t *testing.T, spans []span, action string) span {
	t.Helper()
	for _, s := range spans {
		if s.action == action {
			return s
		}
	}
	t.Fatalf("no control with action %q among %v", action, spans)
	return span{}
}

func TestLayoutIsHtopShaped(t *testing.T) {
	t.Parallel()
	m := sized(t, booted(t, &fakeCtrl{}), 130, 32)
	lines := strings.Split(m.View(), "\n")
	f := m.frame()
	for row, want := range map[int][]string{
		f.head.y:   {"mabo-ctl", "repo", "3 services", "1 ready", "1 slow", "1 stopped", "● LIVE"},
		f.meters.y: {"CPU[", "MEM", "config"},
		f.thead.y:  {"SERVICE", "STATE", "PORT", "PID", "UPTIME", "CPU%", "MEM", "PROBE", "DETAIL"},
		f.table.y:  {"website", "● ready", "7100", "101", "200"},
		f.phead.y:  {"website · OUTPUT", "following", "level all", "Output", "Detail", "Config", "Tab"},
		f.keys.y:   {"F1", "Help", "F2", "Config", "F3", "Search", "F6", "Sort", "F9", "Action", "F10", "Quit"},
	} {
		for _, w := range want {
			if !strings.Contains(lines[row], w) {
				t.Errorf("row %d lacks %q:\n%s", row, w, lines[row])
			}
		}
	}
	if f.keys.y != len(lines)-1 || f.notice.y != len(lines)-2 {
		t.Errorf("the key bar (%d) and notice (%d) are not the last two rows of %d", f.keys.y, f.notice.y, len(lines))
	}
}

// TestEveryRowIsExactlyTheTerminalWide: a row one column too wide wraps and
// pushes the key bar off the screen. Wide characters count two columns, so
// emoji and CJK in a log line or a service's detail must not shift anything.
func TestEveryRowIsExactlyTheTerminalWide(t *testing.T) {
	t.Parallel()
	sts := threeServices()
	sts[0].Detail = "🔵 ready · 日本語 · €"
	m := booted(t, &fakeCtrl{statuses: sts})
	m.setLines("18:55:09 🔵 INFO  httpx | GET /collections 日本語", `{"level":"warn","msg":"🚀 deployed"}`)
	for _, size := range [][2]int{{160, 40}, {120, 30}, {90, 30}, {60, 20}, {40, 10}} {
		m = sized(t, m, size[0], size[1])
		for _, mode := range []string{"", "i", "C", "enter", "f6"} {
			mm := m
			if mode != "" {
				mm, _ = send(t, mm, keyFor(mode))
			}
			lines := strings.Split(mm.View(), "\n")
			if len(lines) != size[1] {
				t.Errorf("%dx%d %q: %d rows, want %d", size[0], size[1], mode, len(lines), size[1])
			}
			for i, l := range lines {
				if w := ansi.StringWidth(l); w != size[0] {
					t.Errorf("%dx%d %q: row %d is %d columns, want %d: %q", size[0], size[1], mode, i, w, size[0], l)
				}
			}
		}
	}
}

func TestFunctionKeysAndTheirLetters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		fkey, letter string
		check        func(Model) bool
	}{
		{"f1", "?", func(m Model) bool { return m.help }},
		{"f2", "C", func(m Model) bool { return m.pane == paneConfig }},
		{"f3", "/", func(m Model) bool { return m.filtering }},
		{"f4", "f", func(m Model) bool { return m.nameFiltering }},
		{"f5", "m", func(m Model) bool { return m.all }},
		{"f6", "<", func(m Model) bool { return m.menu != nil && m.menu.title == "Sort by" }},
		{"f7", "i", func(m Model) bool { return m.pane == paneDetail }},
		{"f8", "p", func(m Model) bool { return m.paused }},
		{"f9", "enter", func(m Model) bool { return m.menu != nil && m.menu.title == "Actions" }},
		{"f10", "q", func(m Model) bool { return m.Quitting() }},
	} {
		for _, k := range []string{tc.fkey, tc.letter} {
			m := booted(t, &fakeCtrl{})
			m, _ = send(t, m, keyFor(k))
			if !tc.check(m) {
				t.Errorf("%s did not do what %s does", k, tc.fkey)
			}
		}
	}
}

func TestActionsMenuRunsTheChosenOperation(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{block: make(chan struct{})}
	defer close(f.block)
	m := booted(t, f)

	m, _ = send(t, m, key("enter"))
	if m.menu == nil || m.menu.items[m.menu.cursor].key != "r" {
		t.Fatalf("enter on a running service did not open the menu on Restart: %+v", m.menu)
	}
	if v := m.View(); !strings.Contains(v, "Actions") || !strings.Contains(v, "Restart website") {
		t.Errorf("the menu is not drawn:\n%s", v)
	}
	m, _ = send(t, m, key("down")) // Show detail
	m, _ = send(t, m, key("up"))   // back to Restart
	m, _ = send(t, m, key("up"))   // Stop
	_, _ = send(t, m, key("enter"))
	waitFor(t, "the stop", func() bool { return len(f.opCalls()) == 1 })
	if got := f.opCalls()[0]; got.kind != opStop || strings.Join(got.names, ",") != "website" {
		t.Errorf("the menu ran %s %v, want stop website", got.kind, got.names)
	}

	// A listed key runs its entry straight from the menu; esc only closes.
	m = booted(t, &fakeCtrl{})
	m, _ = send(t, m, keyFor("f9"))
	m, _ = send(t, m, key("esc"))
	if m.menu != nil || m.Quitting() {
		t.Error("esc did not just close the menu")
	}
	m, _ = send(t, m, keyFor("f9"))
	m, _ = send(t, m, key("i"))
	if m.menu != nil || m.pane != paneDetail {
		t.Error("i in the menu did not open the DETAIL pane")
	}
}

func TestSortMenuOrdersTheTable(t *testing.T) {
	t.Parallel()
	sts := threeServices()
	sts[0].Usage = supervisor.Usage{CPU: 1, RSS: 10 << 20}
	sts[1].Usage = supervisor.Usage{CPU: 9, RSS: 50 << 20}
	m := booted(t, &fakeCtrl{statuses: sts})

	names := func(m Model) string {
		var out []string
		for _, i := range m.listIndices() {
			out = append(out, m.statuses[i].Name)
		}
		return strings.Join(out, ",")
	}
	m, _ = send(t, m, keyFor("f6"))
	for m.menu.items[m.menu.cursor].action != "sort:cpu" {
		m, _ = send(t, m, key("down"))
	}
	m, _ = send(t, m, key("enter"))
	if got := names(m); got != "backend,website,worker" {
		t.Errorf("sorted by CPU: %s", got)
	}
	if head, _ := m.theadLine(m.width); !strings.Contains(head, "CPU%▼") {
		t.Errorf("the sort column is not marked: %s", head)
	}
	// The selection follows the table's order, not the declared one.
	m, _ = send(t, m, keyFor("home"))
	if m.SelectedService() != "backend" {
		t.Errorf("home selected %q, want the first row, backend", m.SelectedService())
	}
}

func TestPanesSwitchAndScroll(t *testing.T) {
	t.Parallel()
	m := booted(t, &fakeCtrl{})
	m, _ = send(t, m, keyFor("f7"))
	if m.pane != paneDetail || m.focus != focusLog {
		t.Fatalf("F7: pane %d focus %d; want DETAIL with the keyboard", m.pane, m.focus)
	}
	m, _ = send(t, m, key("j"))
	if m.SelectedService() != "website" {
		t.Error("j moved the selection while the pane had the keyboard")
	}
	m, _ = send(t, m, key("tab"))
	m, _ = send(t, m, key("j"))
	if m.SelectedService() != "backend" || m.pane != paneDetail {
		t.Errorf("after tab, j: selected %q, pane %d; want backend's DETAIL", m.SelectedService(), m.pane)
	}
	if !strings.Contains(m.View(), "backend · DETAIL") {
		t.Errorf("the DETAIL pane does not follow the selection:\n%s", m.View())
	}
	m, _ = send(t, m, keyFor("f7"))
	if m.pane != paneOutput {
		t.Error("F7 again did not return to OUTPUT")
	}
}

func TestMouseRunsTheSameActions(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{block: make(chan struct{})}
	defer close(f.block)
	m := sized(t, booted(t, f), 130, 32)
	fr := m.frame()

	// A row selects.
	m, _ = send(t, m, clickAt(3, fr.table.y+2))
	if m.SelectedService() != "worker" {
		t.Errorf("clicking the third row selected %q", m.SelectedService())
	}
	// A column header sorts.
	_, spans := m.theadLine(fr.thead.w)
	m, _ = send(t, m, clickAt(spanFor(t, spans, "sort:name").x0, fr.thead.y))
	if m.sortBy != "name" {
		t.Errorf("clicking SERVICE sorted by %q", m.sortBy)
	}
	// A pane name switches the pane.
	_, spans = m.pheadLine(fr.phead.w)
	m, _ = send(t, m, clickAt(spanFor(t, spans, "pane:config").x0, fr.phead.y))
	if m.pane != paneConfig {
		t.Error("clicking Config did not switch the pane")
	}
	// A key-bar label presses its key, and a menu entry runs.
	_, spans = m.keyBar(fr.keys.w)
	m, _ = send(t, m, clickAt(spanFor(t, spans, "f9").x0, fr.keys.y))
	if m.menu == nil {
		t.Fatal("clicking F9 did not open the actions menu")
	}
	_, box := m.menuBox()
	stop := 0
	for i, it := range m.menu.items {
		if it.key == "x" {
			stop = i
		}
	}
	_, _ = send(t, m, clickAt(box.x+3, box.y+1+stop))
	waitFor(t, "the stop", func() bool { return len(f.opCalls()) == 1 })
	if got := f.opCalls()[0]; got.kind != opStop {
		t.Errorf("clicking Stop in the menu ran %s", got.kind)
	}
}

func TestTheWheelScrollsWhatIsUnderThePointer(t *testing.T) {
	t.Parallel()
	m := sized(t, booted(t, &fakeCtrl{}), 130, 32)
	for range 200 {
		m.appendLine("line")
	}
	fr := m.frame()
	m, _ = send(t, m, tea.MouseMsg{X: 3, Y: fr.pane.y + 2, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if m.follow {
		t.Error("wheel up over the pane did not leave the tail")
	}
	m, _ = send(t, m, tea.MouseMsg{X: 3, Y: fr.table.y, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if m.SelectedService() != "backend" {
		t.Errorf("wheel down over the table selected %q, want backend", m.SelectedService())
	}
}
