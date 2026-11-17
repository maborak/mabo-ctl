package console

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// The tests in this file cover the web console's structure in the terminal:
// the sidebar, the top and tab bars, the service card, the output toolbar,
// and the mouse driving every drawn control.

// wide resizes m to a terminal that gets the sidebar.
func wide(t *testing.T, m Model) Model {
	t.Helper()
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 160, Height: 40})
	if !m.frame().wide {
		t.Fatal("160x40 did not get the wide layout")
	}
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

func TestWideLayoutMirrorsTheWebConsole(t *testing.T) {
	t.Parallel()
	m := wide(t, booted(t, &fakeCtrl{}))
	view := m.View()
	lines := strings.Split(view, "\n")
	f := m.frame()
	for i, want := range map[int][]string{
		0:            {"mabo-ctl", "CONSOLE", "CONFIG", "[a Start all]", "[R Restart all]", "[S Stop all]", "2/3 up"},
		1:            {"SERVICES", "[f filter by name]", "OUTPUT", "CONFIG", "1 ● ready", "1 ◐ slow", "1 ○ stopped", "3 services"},
		2:            {"SERVICE", "PORT", "PID", "UPTIME", "PROBE", "┌ website ● ready", "[s Start]", "[x Stop]", "[r Restart]"},
		3:            {"website", "7100", "PID", "READY IN", "CPU · MEM"},
		f.tool.y:     {"OUTPUT · website", "[m All services]", "[p Pause]", "[G Follow tail]", "[c Clear]", "[/ filter lines…]"},
		f.tool.y + 1: {"[✓ All]", "[Err]", "[Warn]", "[Info]", "[Dbg]", "[ ] Separators", "[ ] Pretty JSON", "lines"},
		f.sys.y:      {"SYSTEM"},
	} {
		for _, w := range want {
			if !strings.Contains(lines[i], w) {
				t.Errorf("row %d lacks %q:\n%s", i, w, lines[i])
			}
		}
	}
	// The sidebar and the main column are divided by one rule on every row.
	for i, l := range lines {
		if !strings.Contains(l, "│") {
			t.Errorf("row %d has no column rule: %q", i, l)
		}
	}
}

// TestEveryRowIsExactlyTheTerminalWide: a row one column too wide wraps and
// pushes the bottom of the layout off the screen. Wide characters count two
// columns, so an emoji in a log line or a service's detail must not shift the
// rule or the card's border.
func TestEveryRowIsExactlyTheTerminalWide(t *testing.T) {
	t.Parallel()
	sts := threeServices()
	sts[0].Detail = "🔵 ready · 日本語 · €"
	m := booted(t, &fakeCtrl{statuses: sts})
	m.setLines("18:55:09 🔵 INFO  httpx | GET /collections 日本語", `{"level":"warn","msg":"🚀 deployed"}`)
	for _, size := range []tea.WindowSizeMsg{{Width: 160, Height: 40}, {Width: 120, Height: 30}, {Width: 90, Height: 30}, {Width: 60, Height: 20}} {
		m, _ = send(t, m, size)
		for _, mode := range []string{"", "i", "C"} {
			mm := m
			if mode != "" {
				mm, _ = send(t, mm, key(mode))
			}
			lines := strings.Split(mm.View(), "\n")
			if len(lines) != size.Height {
				t.Errorf("%dx%d %q: %d rows, want %d", size.Width, size.Height, mode, len(lines), size.Height)
			}
			for i, l := range lines {
				if w := ansi.StringWidth(l); w != size.Width {
					t.Errorf("%dx%d %q: row %d is %d columns, want %d: %q", size.Width, size.Height, mode, i, w, size.Width, l)
				}
			}
		}
	}
}

func TestNarrowLayoutStacksTheSameParts(t *testing.T) {
	t.Parallel()
	m := booted(t, &fakeCtrl{})
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 90, Height: 30})
	if m.frame().wide {
		t.Fatal("90 columns got the sidebar")
	}
	view := m.View()
	for _, want := range []string{"CONFIG", "[a Start all]", "OUTPUT", "SERVICE", "┌ website", "[x Stop]", "[m All services]", "[✓ All]", "SYSTEM", "q quit"} {
		if !strings.Contains(view, want) {
			t.Errorf("the narrow layout lacks %q:\n%s", want, view)
		}
	}
	// The phase chips go compact rather than losing the last ones.
	tabs, _ := m.tabBar(70)
	for _, want := range []string{"◆0", "●1", "○1"} {
		if !strings.Contains(tabs, want) {
			t.Errorf("compact chips lack %q: %s", want, tabs)
		}
	}
}

func TestClickingAServiceRowSelectsIt(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{}
	m := wide(t, booted(t, f))
	fr := m.frame()
	m, cmd := send(t, m, clickAt(5, fr.side.y+fr.sideRowsTop()+2))
	if m.SelectedService() != "worker" || cmd == nil {
		t.Fatalf("clicking the third row selected %q (cmd %v)", m.SelectedService(), cmd != nil)
	}
	f.tailFor(t, "worker")

	// Narrow: the same click lands on the stacked table.
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 90, Height: 30})
	fr = m.frame()
	m, _ = send(t, m, clickAt(5, fr.side.y+fr.sideRowsTop()))
	if m.SelectedService() != "website" {
		t.Errorf("narrow click selected %q, want website", m.SelectedService())
	}
}

func TestClickingTheCardButtonsRunsTheirOperation(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{block: make(chan struct{})}
	defer close(f.block)
	m := wide(t, booted(t, f))
	fr := m.frame()
	_, spans := m.cardHead(fr.card.w)
	stop := spanFor(t, spans, "x")
	_, _ = send(t, m, clickAt(fr.card.x+stop.x0, fr.card.y))
	waitFor(t, "the stop to reach the supervisor", func() bool { return len(f.opCalls()) == 1 })
	if got := f.opCalls()[0]; got.kind != opStop || strings.Join(got.names, ",") != "website" {
		t.Errorf("clicking Stop ran %s %v, want stop website", got.kind, got.names)
	}
}

func TestClickingTheToolbarTogglesTheLogControls(t *testing.T) {
	t.Parallel()
	m := wide(t, booted(t, &fakeCtrl{}))
	m.setLines(`{"level":"error","msg":"x"}`, `{"level":"info","msg":"y"}`)
	fr := m.frame()

	click := func(row func(int) (string, []span), y int, action string) {
		t.Helper()
		_, spans := row(fr.tool.w)
		s := spanFor(t, spans, action)
		m, _ = send(t, m, clickAt(fr.tool.x+s.x0, y))
	}
	click(m.levelBar, fr.tool.y+1, "J")
	click(m.levelBar, fr.tool.y+1, "|")
	click(m.levelBar, fr.tool.y+1, "level:error")
	if !m.pretty || !m.separators || m.levelFilter != "error" || len(m.rows()) != 4 {
		t.Errorf("after the clicks: pretty %v, separators %v, level %q, %d rows", m.pretty, m.separators, m.levelFilter, len(m.rows()))
	}
	click(m.toolBar, fr.tool.y, "p")
	if !m.paused {
		t.Error("clicking Pause did not pause")
	}
	click(m.toolBar, fr.tool.y, "/")
	if !m.filtering {
		t.Error("clicking the filter box did not open the filter")
	}
	// A click elsewhere commits what was typed.
	m, _ = send(t, m, key("x"))
	m, _ = send(t, m, clickAt(fr.log.x+2, fr.log.y+1))
	if m.filtering || m.filter != "x" {
		t.Errorf("a click did not commit the filter: filtering %v, filter %q", m.filtering, m.filter)
	}
}

func TestClickingTabsAndPhaseChips(t *testing.T) {
	t.Parallel()
	m := wide(t, booted(t, &fakeCtrl{}))
	fr := m.frame()
	_, spans := m.tabBar(fr.tabs.w)
	cfg := spanFor(t, spans, "tab:config")
	m, _ = send(t, m, clickAt(fr.tabs.x+cfg.x0, fr.tabs.y))
	if !m.showConfig {
		t.Fatal("clicking CONFIG did not open the tab")
	}
	m, _ = send(t, m, clickAt(fr.tabs.x+spanFor(t, spans, "tab:output").x0, fr.tabs.y))
	if m.showConfig {
		t.Fatal("clicking OUTPUT did not return")
	}

	// The slow chip is the fourth phase.
	slow := spanFor(t, spans, "key:4")
	m, _ = send(t, m, clickAt(fr.tabs.x+slow.x0, fr.tabs.y))
	if len(m.listIndices()) != 1 || m.SelectedService() != "backend" {
		t.Errorf("the slow chip shows %d services, selected %q", len(m.listIndices()), m.SelectedService())
	}
}

func TestTheWheelScrollsWhatIsUnderThePointer(t *testing.T) {
	t.Parallel()
	m := wide(t, booted(t, &fakeCtrl{}))
	for range 200 {
		m.appendLine("line")
	}
	fr := m.frame()
	m, _ = send(t, m, tea.MouseMsg{X: fr.log.x + 3, Y: fr.log.y + 2, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if m.follow {
		t.Error("wheel up over the log did not leave the tail")
	}
	m, _ = send(t, m, tea.MouseMsg{X: 3, Y: fr.side.y + fr.sideRowsTop(), Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if m.SelectedService() != "backend" {
		t.Errorf("wheel down over the list selected %q, want backend", m.SelectedService())
	}
}

func TestConfigTabKeepsTheSidebarKeysWorking(t *testing.T) {
	t.Parallel()
	f := &fakeCtrl{block: make(chan struct{})}
	defer close(f.block)
	m := wide(t, booted(t, f))
	m, _ = send(t, m, key("C"))
	m, _ = send(t, m, key("j"))
	if !m.showConfig || m.cfgOffset != 1 || m.SelectedService() != "website" {
		t.Errorf("j on CONFIG: tab %v, offset %d, selected %q; want it to scroll the config", m.showConfig, m.cfgOffset, m.SelectedService())
	}
	m, _ = send(t, m, key("x"))
	waitFor(t, "the stop", func() bool { return len(f.opCalls()) == 1 })
	if !m.showConfig {
		t.Error("a service key closed the CONFIG tab")
	}
	m, _ = send(t, m, key("esc"))
	if m.showConfig {
		t.Error("esc did not return to OUTPUT")
	}
}
