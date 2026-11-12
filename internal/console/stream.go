package console

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/ui"
)

// Timing and buffering constants for the background work the console drives.
const (
	// refreshInterval is how often the service list re-reads the supervisor's
	// view of the world. One second is fast enough that a service that just
	// went ready looks instantaneous, and slow enough that the health probes
	// behind it are not a load source.
	refreshInterval = time.Second

	// statusTimeout bounds one Status call. Status probes health, and a probe
	// against a hung server takes its own timeout; without a bound here a
	// wedged service would pin a goroutine per tick forever.
	statusTimeout = 10 * time.Second

	// tailBacklog is how many existing log lines a newly selected service
	// shows before following. It is a screenful several times over, so
	// scrolling back after selecting is useful rather than empty.
	tailBacklog = 500

	// mergedBacklog is the per-service backlog of the merged all-services
	// pane, the same as the web console's merged stream asks for.
	mergedBacklog = 200

	// tailBuffer is the channel depth between the supervisor's tail and the
	// event loop. A line may be 256 KiB, so depth is memory: 16 lines is at
	// most 4 MiB. Blocking the tail loses nothing — it reads a file.
	tailBuffer = 16

	// eventBuffer is the depth of a lifecycle operation's event channel.
	// Events are small; a start emits a burst of them, absorbed here.
	eventBuffer = 256

	// maxLogLines caps the retained log buffer. A dev server can emit
	// megabytes an hour; the console keeps the tail of that, not all of it.
	maxLogLines = 5000

	// maxLogBytes caps the same buffer by size. A line can be 256 KiB, so a
	// line count alone allowed over a gigabyte; this keeps the newest 4 MiB.
	maxLogBytes = 4 << 20
)

// Messages the console's own commands deliver to [Model.Update]. Every one of
// them is the *result* of work done on another goroutine — Update itself never
// waits for anything.
type (
	// tickMsg is the periodic status-refresh trigger.
	tickMsg time.Time

	// statusMsg carries one completed Status call, tagged with the controller
	// generation it was read from; see Model.ctrlGen.
	statusMsg struct {
		statuses []supervisor.Status
		gen      int
	}

	// tailLineMsg is one log line, tagged with the session that produced it so
	// a line from a cancelled tail can be recognised and dropped, and with the
	// service it came from for the merged pane's labels.
	tailLineMsg struct {
		sess *tailSession
		svc  string
		line string
	}

	// tailClosedMsg reports that a tail ended: cancelled, or failed with err.
	tailClosedMsg struct {
		sess *tailSession
		err  error
	}

	// opEventMsg is one supervisor.Event from an in-flight operation.
	opEventMsg struct {
		sess *opSession
		ev   supervisor.Event
	}

	// opDoneMsg reports that an operation finished, successfully or not.
	opDoneMsg struct {
		sess *opSession
		err  error
	}
)

// tick schedules the next status refresh.
func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// refresh reads the supervisor's status on a background goroutine and delivers
// it as a [statusMsg]. It never blocks the event loop, and it bounds itself
// with [statusTimeout] so a hung health probe cannot accumulate goroutines.
//
// A nil controller yields an empty snapshot rather than a panic: the console
// renders "no services" and stays usable, which is a better failure than
// losing the terminal to a stack trace.
func refresh(ctrl Controller, gen int) tea.Cmd {
	return func() tea.Msg {
		if ctrl == nil {
			return statusMsg{gen: gen}
		}
		ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
		defer cancel()
		return statusMsg{statuses: ctrl.Status(ctx), gen: gen}
	}
}

// tailLine is one line of a tail session, labelled with its service.
type tailLine struct {
	svc, text string
}

// tailSession is one following tail of one service's log, or of several
// services' logs merged into one pane.
//
// The session owns one goroutine per service running supervisor.Supervisor.Tail,
// which does not return until its context is cancelled. Exactly one session is
// live at a time; [tailSession.stop] is what makes that true, and skipping it
// is the leak this type exists to prevent.
type tailSession struct {
	key    string // the pane it feeds: a service name, or allKey
	lines  chan tailLine
	done   chan error
	cancel context.CancelFunc
	once   sync.Once
}

// newTail starts following svcs and returns the session. key names what the
// pane shows; backlog is how many existing lines each service replays.
//
// Per service it runs Tail on its own unbuffered channel plus a forwarder that
// labels lines into the session's channel. Tail closes its channel as it
// returns, including during a panic (its close is deferred), so every
// forwarder ends. When the last one does, the session publishes the joined
// error on done and closes lines, in that order, which is what lets
// [tailSession.next] report a failure rather than a silent, empty pane. A
// panic inside the supervisor's tail is recovered and reported the same way,
// not allowed to take the process down with the terminal in raw mode.
func newTail(ctrl Controller, key string, svcs []string, backlog int) *tailSession {
	ctx, cancel := context.WithCancel(context.Background())
	s := &tailSession{
		key:    key,
		lines:  make(chan tailLine, tailBuffer),
		done:   make(chan error, 1),
		cancel: cancel,
	}
	errs := make([]error, len(svcs))
	var wg sync.WaitGroup
	for i, svc := range svcs {
		// Unbuffered: the tailer blocked on a send already holds one line,
		// and the forwarder one more. A line may be 256 KiB.
		in := make(chan string)
		wg.Add(2)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs[i] = fmt.Errorf("console: tailing %s panicked: %v", svc, r)
				}
			}()
			errs[i] = ctrl.Tail(ctx, svc, backlog, true, in)
		}()
		go func() {
			defer wg.Done()
			for line := range in {
				select {
				case s.lines <- tailLine{svc: svc, text: ui.Sanitize(line)}:
				case <-ctx.Done():
					for range in { //nolint:revive // draining so Tail can return
					}
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		s.done <- errors.Join(errs...)
		close(s.lines)
	}()
	return s
}

// next returns a command that waits for the session's next log line.
//
// The command blocks on a channel receive, which is exactly what a tea.Cmd is
// for: it runs on its own goroutine and the event loop keeps drawing. Update
// re-arms it for as long as the session stays current.
func (s *tailSession) next() tea.Cmd {
	return func() tea.Msg {
		l, ok := <-s.lines
		if !ok {
			// done is published before lines is closed, so this receive never
			// waits — and a tail that failed ("no such log file") is reported
			// rather than shown as a silent, empty pane.
			return tailClosedMsg{sess: s, err: <-s.done}
		}
		return tailLineMsg{sess: s, svc: l.svc, line: l.text}
	}
}

// stop cancels the tail and drains what it has already produced.
//
// The drain matters as much as the cancel. Once a session stops being current
// the event loop stops re-arming its reader, so a forwarder mid-send on a full
// channel would block forever. The forwarders also select on the context, and
// the drain goroutine ends when the session closes the channel.
//
// stop is idempotent and safe to call from outside the event loop, which is
// what lets [Model.Shutdown] run from a deferred call during a panic.
func (s *tailSession) stop() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.cancel()
		go func() {
			for range s.lines { //nolint:revive // draining, the values are dead
			}
		}()
	})
}

// opKind names a supervisor operation the console can launch.
type opKind string

// The operations bound to keys.
const (
	opStart   opKind = "start"
	opStop    opKind = "stop"
	opRestart opKind = "restart"
)

// opSession is one in-flight start, stop or restart.
//
// Like a tail it owns a goroutine and a channel, and for the same reason: a
// start blocks for the readiness timeout, which must not be the event loop's
// problem. Events arrive as messages while it runs, so the console shows a
// service going running → slow → ready as it happens.
type opSession struct {
	kind   opKind
	names  []string
	ev     chan supervisor.Event
	done   chan error
	cancel context.CancelFunc
	once   sync.Once
}

// newOp launches kind over names (empty means every service) and returns the
// session.
//
// It spawns one goroutine. Cancelling the session abandons the operation's
// remaining work — a start stops waiting for readiness, a stop stops waiting
// out its grace period — but it never signals an already-spawned process:
// killing services is what the stop key is for, not what quitting does.
//
// Unlike a tail, the goroutine closes the event channel itself: the supervisor
// only writes to it, and the console needs the close to know the operation
// ended. The buffer is generous because the supervisor drops events it cannot
// deliver immediately rather than blocking on them.
func newOp(ctrl Controller, kind opKind, names []string) *opSession {
	ctx, cancel := context.WithCancel(context.Background())
	s := &opSession{
		kind:   kind,
		names:  names,
		ev:     make(chan supervisor.Event, eventBuffer),
		done:   make(chan error, 1),
		cancel: cancel,
	}
	go func() {
		defer close(s.ev)
		defer func() {
			if r := recover(); r != nil {
				s.done <- fmt.Errorf("console: %s panicked: %v", kind, r)
			}
		}()
		var err error
		switch kind {
		case opStart:
			err = ctrl.Start(ctx, names, s.ev)
		case opStop:
			err = ctrl.Stop(ctx, names, s.ev)
		case opRestart:
			err = ctrl.Restart(ctx, names, s.ev)
		default:
			err = fmt.Errorf("console: unknown operation %q", kind)
		}
		s.done <- err
	}()
	return s
}

// next waits for the session's next event, or for the session to finish.
func (s *opSession) next() tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-s.ev
		if !ok {
			// The outcome is published before the channel is closed, so this
			// receive never actually waits — and an operation that failed is
			// never reported as one that merely finished.
			return opDoneMsg{sess: s, err: <-s.done}
		}
		return opEventMsg{sess: s, ev: ev}
	}
}

// stop cancels the operation and drains its event channel, on the same
// reasoning as [tailSession.stop]. It is idempotent.
func (s *opSession) stop() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.cancel()
		go func() {
			for range s.ev { //nolint:revive // draining, the values are dead
			}
		}()
	})
}

// label describes the operation for the status line, e.g. "starting backend"
// or "stopping all services".
func (s *opSession) label() string {
	target := "all services"
	if len(s.names) == 1 {
		target = s.names[0]
	} else if len(s.names) > 1 {
		target = fmt.Sprintf("%d services", len(s.names))
	}
	switch s.kind {
	case opStart:
		return "starting " + target
	case opStop:
		return "stopping " + target
	case opRestart:
		return "restarting " + target
	default:
		return string(s.kind) + " " + target
	}
}

// live is the part of the model that must be reachable from outside the event
// loop: the goroutine-owning sessions.
//
// [Model] is a value, copied on every Update, so a session stored in a plain
// field is only reachable from the copy that created it. Run has to be able to
// shut the console down from a deferred call while a panic unwinds, and that
// deferred call only holds the original Model — so the sessions live behind
// this shared pointer instead, guarded by a mutex because Shutdown may run on
// a different goroutine than Update.
type live struct {
	mu   sync.Mutex
	tail *tailSession
	ops  map[*opSession]struct{}
	down bool
}

// newLive returns an empty live set.
func newLive() *live { return &live{ops: make(map[*opSession]struct{})} }

// setTail stops the previous tail and adopts s. It returns false, having
// already stopped s, when the console is shutting down — so a tail started by
// an in-flight command after Shutdown cannot outlive the console.
func (l *live) setTail(s *tailSession) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tail != nil {
		l.tail.stop()
		l.tail = nil
	}
	if l.down {
		s.stop()
		return false
	}
	l.tail = s
	return true
}

// clearTail stops the current tail, if any.
func (l *live) clearTail() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tail != nil {
		l.tail.stop()
		l.tail = nil
	}
}

// addOp registers an in-flight operation. It returns false, having stopped s,
// when the console is shutting down.
func (l *live) addOp(s *opSession) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.down {
		s.stop()
		return false
	}
	l.ops[s] = struct{}{}
	return true
}

// removeOp forgets a finished operation.
func (l *live) removeOp(s *opSession) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.ops, s)
}

// shutdown stops every session and marks the set closed so nothing new is
// adopted afterwards. It is idempotent and safe to call from any goroutine.
func (l *live) shutdown() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.down = true
	if l.tail != nil {
		l.tail.stop()
		l.tail = nil
	}
	for op := range l.ops {
		op.stop()
		delete(l.ops, op)
	}
}
