package console

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// wheelStep is how many rows one wheel notch scrolls the pane.
const wheelStep = 3

// handleMouse answers a mouse event. The keyboard is the console's primary
// interface; the mouse is a shortcut to the same actions. A click on a control
// runs exactly what that control's key runs, so the two cannot drift apart:
// a table row selects, a column header sorts, a pane name switches the pane,
// a key-bar label presses its key, a menu entry runs. The wheel scrolls what
// is under the pointer.
func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.confirmReload {
		// The reload question is answered with y or another key, never by a
		// click that happened to land somewhere.
		return m, nil
	}
	x, y := msg.X, msg.Y
	if msg.Action == tea.MouseActionPress && (msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown) {
		return m.wheel(x, y, msg.Button == tea.MouseButtonWheelUp)
	}
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}
	if m.help || m.showEvents {
		m.help, m.showEvents = false, false
		return m, nil
	}
	if m.menu != nil {
		_, r := m.menuBox()
		if i := y - r.y - 1; r.has(x, y) && i >= 0 && i < len(m.menu.items) {
			return m.runMenu(i)
		}
		m.menu = nil
		return m, nil
	}
	// A click elsewhere commits a filter being typed, as enter would: the
	// draft is what the operator meant, and dropping it would lose it.
	var pre tea.Cmd
	if m.filtering || m.nameFiltering {
		mm, cmd := m.editFilter(tea.KeyMsg{Type: tea.KeyEnter})
		m, pre = mm.(Model), cmd
	}
	mm, cmd := m.click(x, y)
	return mm, tea.Batch(pre, cmd)
}

// wheel scrolls what is under the pointer: the table or the pane.
func (m Model) wheel(x, y int, up bool) (tea.Model, tea.Cmd) {
	if m.help || m.showEvents || m.menu != nil {
		return m, nil
	}
	f := m.frame()
	step := wheelStep
	if up {
		step = -wheelStep
	}
	switch {
	case f.table.has(x, y):
		return m.move(step / wheelStep)
	case f.pane.has(x, y):
		m.scrollPane(step)
	}
	return m, nil
}

// click answers a left click at (x, y).
func (m Model) click(x, y int) (tea.Model, tea.Cmd) {
	f := m.frame()
	at := func(spans []span) string {
		for _, s := range spans {
			if x >= s.x0 && x < s.x1 {
				return s.action
			}
		}
		return ""
	}
	switch {
	case f.thead.has(x, y):
		_, spans := m.theadLine(f.thead.w)
		return m.act(at(spans))
	case f.table.has(x, y):
		idx := m.listIndices()
		j := m.listWindow(idx, f.table.h) + y - f.table.y
		m.focus = focusList
		if j < 0 || j >= len(idx) || idx[j] == m.sel {
			return m, nil
		}
		m.sel = idx[j]
		m.filter = ""
		return m.syncTailModel()
	case f.phead.has(x, y):
		_, spans := m.pheadLine(f.phead.w)
		return m.act(at(spans))
	case f.pane.has(x, y):
		m.focus = focusLog
	case f.notice.has(x, y):
		return m.act("e")
	case f.keys.has(x, y):
		_, spans := m.keyBar(f.keys.w)
		return m.act(at(spans))
	}
	return m, nil
}

// act runs an action: a key, or one of the few actions with no single key
// of their own (a pane, a sort order, focusing the pane).
func (m Model) act(action string) (tea.Model, tea.Cmd) {
	switch {
	case action == "":
		return m, nil
	case action == "focus":
		if m.focus == focusLog {
			m.focus = focusList
		} else {
			m.focus = focusLog
		}
		return m, nil
	case action == "pane:output":
		m.pane, m.paneOffset = paneOutput, 0
		return m, nil
	case action == "pane:detail":
		if m.pane != paneDetail {
			m.togglePane(paneDetail)
		}
		return m, nil
	case action == "pane:config":
		if m.pane != paneConfig {
			m.togglePane(paneConfig)
		}
		return m, nil
	case strings.HasPrefix(action, "sort:"):
		m.sortBy = strings.TrimPrefix(action, "sort:")
		return m, nil
	}
	return m.handleKey(keyFor(action))
}

// keyFor is the keypress an action string names.
func keyFor(action string) tea.KeyMsg {
	switch action {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "f1":
		return tea.KeyMsg{Type: tea.KeyF1}
	case "f2":
		return tea.KeyMsg{Type: tea.KeyF2}
	case "f3":
		return tea.KeyMsg{Type: tea.KeyF3}
	case "f4":
		return tea.KeyMsg{Type: tea.KeyF4}
	case "f5":
		return tea.KeyMsg{Type: tea.KeyF5}
	case "f6":
		return tea.KeyMsg{Type: tea.KeyF6}
	case "f7":
		return tea.KeyMsg{Type: tea.KeyF7}
	case "f8":
		return tea.KeyMsg{Type: tea.KeyF8}
	case "f9":
		return tea.KeyMsg{Type: tea.KeyF9}
	case "f10":
		return tea.KeyMsg{Type: tea.KeyF10}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(action)}
}
