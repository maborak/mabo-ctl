package console

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// wheelStep is how many rows one wheel notch scrolls the log or the config.
const wheelStep = 3

// handleMouse answers a mouse event. A click runs exactly what the clicked
// control's key runs — the buttons are drawn with their keys, and a click is
// that key — so the mouse and the keyboard cannot drift apart. The wheel
// scrolls whatever is under the pointer.
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

// wheel scrolls what is under the pointer: the service list, the config or
// the log.
func (m Model) wheel(x, y int, up bool) (tea.Model, tea.Cmd) {
	if m.help || m.showEvents {
		return m, nil
	}
	f := m.frame()
	step := wheelStep
	if up {
		step = -wheelStep
	}
	switch {
	case f.side.has(x, y):
		return m.move(step / wheelStep)
	case m.showConfig && f.body.has(x, y):
		m.cfgOffset = max(0, m.cfgOffset+step)
	case f.log.has(x, y):
		m.scroll(step)
	}
	return m, nil
}

// click answers a left click at (x, y).
func (m Model) click(x, y int) (tea.Model, tea.Cmd) {
	f := m.frame()
	at := func(r rect, spans []span) string {
		for _, s := range spans {
			if x-r.x >= s.x0 && x-r.x < s.x1 {
				return s.action
			}
		}
		return ""
	}
	switch {
	case f.top.has(x, y):
		_, spans := m.topBar(f.top.w)
		return m.act(at(f.top, spans))
	case f.tabs.has(x, y):
		_, spans := m.tabBar(f.tabs.w)
		return m.act(at(f.tabs, spans))
	case f.side.has(x, y):
		return m.clickSide(f, x, y)
	case f.card.has(x, y):
		switch y - f.card.y {
		case 0:
			_, spans := m.cardHead(f.card.w)
			return m.act(at(f.card, spans))
		case 3:
			if f.card.h >= 5 {
				return m.act("i")
			}
		}
	case f.tool.has(x, y):
		var spans []span
		if y == f.tool.y {
			_, spans = m.toolBar(f.tool.w)
		} else {
			_, spans = m.levelBar(f.tool.w)
		}
		return m.act(at(f.tool, spans))
	case f.sys.has(x, y):
		return m.act("e")
	case f.log.has(x, y):
		m.focus = focusLog
	}
	return m, nil
}

// clickSide answers a click in the sidebar: the filter box opens the name
// filter, a service row selects that service, and the footer opens the help.
func (m Model) clickSide(f frame, x, y int) (tea.Model, tea.Cmd) {
	row := y - f.side.y
	if f.wide && row == 1 {
		_, spans := m.sideFilterBar(f.side.w)
		for _, s := range spans {
			if x-f.side.x >= s.x0 && x-f.side.x < s.x1 {
				return m.act(s.action)
			}
		}
		return m, nil
	}
	if f.wide && row == f.side.h-1 {
		return m.act("?")
	}
	top := f.sideRowsTop()
	if row < top {
		if !f.wide {
			return m.act("f")
		}
		return m, nil
	}
	idx := m.listIndices()
	room := f.side.h - top
	if f.wide {
		room -= sideFoot
	}
	j := m.listWindow(idx, room) + row - top
	if j < 0 || j >= len(idx) || idx[j] == m.sel {
		m.focus = focusList
		return m, nil
	}
	m.focus = focusList
	m.sel = idx[j]
	m.filter = ""
	return m.syncTailModel()
}

// act runs a control's action: a key, or one of the few actions with no
// single key of their own.
func (m Model) act(action string) (tea.Model, tea.Cmd) {
	switch {
	case action == "":
		return m, nil
	case action == "tab:output":
		m.showConfig = false
		return m, nil
	case action == "tab:config":
		if !m.showConfig {
			m.showConfig, m.cfgOffset = true, 0
		}
		return m, nil
	case action == "follow":
		if m.follow && !m.paused {
			m.follow = false
			m.offset = m.bottomOffset()
			return m, nil
		}
		if m.paused {
			m.togglePause()
		}
		m.follow = true
		return m, nil
	case action == "focus":
		m.focus = focusLog
		return m, nil
	case strings.HasPrefix(action, "level:"):
		m.levelFilter = strings.TrimPrefix(action, "level:")
		m.follow = true
		return m, nil
	case strings.HasPrefix(action, "key:"):
		action = strings.TrimPrefix(action, "key:")
	}
	return m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(action)})
}
