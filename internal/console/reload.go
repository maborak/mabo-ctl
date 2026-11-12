package console

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// Messages of the configuration watch and reload.
type (
	// configCheckMsg is one comparison of the files on disk with cfg, the
	// configuration in force when it was asked. A result for a configuration
	// a reload has since replaced is dropped.
	configCheckMsg struct {
		cfg     *config.Config
		changed bool
	}

	// reloadEventMsg is one supervisor.Event from an in-flight reload.
	reloadEventMsg struct {
		sess *reloadSession
		ev   supervisor.Event
	}

	// reloadDoneMsg reports a finished reload: res is non-nil when the new
	// configuration was applied, err names what failed either way.
	reloadDoneMsg struct {
		sess *reloadSession
		res  *Reloaded
		err  error
	}
)

// checkConfig compares mabo-ctl.yaml, and the env_files it names, with cfg on a
// background goroutine. A file that cannot be read counts as changed: the
// configuration in force is certainly not what is on disk then, and the reload
// it offers is what reports why.
func checkConfig(cfg *config.Config) tea.Cmd {
	return func() tea.Msg {
		changed, err := cfg.Changed()
		return configCheckMsg{cfg: cfg, changed: changed || err != nil}
	}
}

// reloadSession is one in-flight configuration reload.
//
// Like an operation it owns a goroutine and an event channel, so the console
// shows services stopping and starting as it happens. Unlike one it is never
// cancelled: a reload abandoned half way leaves services stopped that nothing
// will start again (see supervisor.ApplyReload). Quitting waits for it
// instead; see [Model.requestQuit].
type reloadSession struct {
	ev   chan supervisor.Event
	done chan reloadDoneMsg
}

// newReload runs reload on its own goroutine and returns the session. The
// goroutine closes the event channel after publishing the outcome, so a reader
// that sees the close never waits for the result.
func newReload(reload func(context.Context, chan<- supervisor.Event) (*Reloaded, error)) *reloadSession {
	s := &reloadSession{
		ev:   make(chan supervisor.Event, eventBuffer),
		done: make(chan reloadDoneMsg, 1),
	}
	go func() {
		defer close(s.ev)
		defer func() {
			if r := recover(); r != nil {
				s.done <- reloadDoneMsg{sess: s, err: fmt.Errorf("console: reload panicked: %v", r)}
			}
		}()
		res, err := reload(context.Background(), s.ev)
		s.done <- reloadDoneMsg{sess: s, res: res, err: err}
	}()
	return s
}

// next waits for the reload's next event, or for it to finish.
func (s *reloadSession) next() tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-s.ev
		if !ok {
			return <-s.done
		}
		return reloadEventMsg{sess: s, ev: ev}
	}
}

// reloadBlocked says why a reload cannot start now, or "" when it can.
func (m Model) reloadBlocked() string {
	switch {
	case m.opt.Reload == nil:
		return "this console cannot reload its configuration"
	case m.reload != nil:
		return "a configuration reload is already running"
	case m.activeOps > 0:
		return "wait for the running start/stop to finish, then press L to reload"
	default:
		return ""
	}
}

// askReload answers L by opening the y/n question, or by saying why a reload
// cannot run now: a key that silently does nothing reads as a broken key. The
// question is asked because the answer restarts services, which the web
// console confirms too, and because L is one Shift away from l.
func (m Model) askReload() (tea.Model, tea.Cmd) {
	if why := m.reloadBlocked(); why != "" {
		m.notice = why
		return m, nil
	}
	m.confirmReload = true
	return m, nil
}

// answerReload closes the y/n question: y starts the reload, anything else
// leaves the configuration as it is.
func (m Model) answerReload(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.confirmReload = false
	if k := msg.String(); k != "y" && k != "Y" {
		m.notice = "reload cancelled"
		return m, nil
	}
	if why := m.reloadBlocked(); why != "" {
		m.notice = why
		return m, nil
	}
	m.reload = newReload(m.opt.Reload)
	m.err, m.reloadErr = nil, nil
	m.notice = "reloading mabo-ctl.yaml and restarting the services it changes…"
	return m, m.reload.next()
}

// finishReload adopts what a reload applied and reports the outcome. A refused
// reload leaves the configuration in force — and the offer to reload — as
// they were.
func (m Model) finishReload(msg reloadDoneMsg) (tea.Model, tea.Cmd) {
	if msg.sess != m.reload {
		return m, nil
	}
	m.reload = nil
	m.err = nil
	if msg.res == nil {
		m.notice = ""
		m.reloadErr = fmt.Errorf("configuration not reloaded; the one in force is unchanged: %w", msg.err)
	} else {
		m.ctrl = msg.res.Controller
		m.ctrlGen++
		m.opt.Colors = serviceColors(m.ctrl)
		m.opt.Origins = msg.res.Origins
		m.cfgView = m.buildConfigView()
		m.configChanged = false
		m.live.clearTail()
		m.tail, m.tailSvc = nil, ""
		m.notice = reloadSummary(msg.res)
		m.reloadErr = nil
		if msg.err != nil {
			m.reloadErr = fmt.Errorf("configuration reloaded, but %w", msg.err)
		}
	}
	if m.quitAfterReload {
		return m.quitNow()
	}
	m.refreshing = true
	return m, refresh(m.ctrl, m.ctrlGen)
}

// reloadSummary is the notice a reload that applied leaves behind.
func reloadSummary(res *Reloaded) string {
	var parts []string
	if len(res.Restarted) > 0 {
		parts = append(parts, "restarted "+strings.Join(res.Restarted, ", "))
	}
	if len(res.Stopped) > 0 {
		parts = append(parts, "stopped "+strings.Join(res.Stopped, ", "))
	}
	if len(parts) == 0 {
		return "configuration reloaded; no running service needed a restart"
	}
	return "configuration reloaded; " + strings.Join(parts, "; ")
}

// requestQuit quits — unless a configuration reload is still applying. Then it
// waits for the reload, because quitting ends this process and with it a
// reload that may have stopped services it has not started again. Asking a
// second time quits at once.
func (m Model) requestQuit() (tea.Model, tea.Cmd) {
	if m.reload == nil || m.quitAfterReload {
		return m.quitNow()
	}
	m.quitAfterReload = true
	m.notice = "quitting once the configuration reload finishes — press q or ctrl-c again to quit now"
	return m, nil
}
