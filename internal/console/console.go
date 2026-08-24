// Package console implements mabo-ctl's full-screen interactive console: the
// mode a user gets by running the bare binary on a terminal.
//
// The layout is fixed and deliberately boring — a title bar naming the config
// root, a service list with a selection cursor, a log pane following the
// selected service, and a key-hint line — because the console is a thing to
// glance at, not a thing to learn.
//
// Three properties are load-bearing and every change to this package has to
// preserve them.
//
// # The UI never blocks
//
// Starting a service can take the full readiness timeout (30s by default).
// Nothing in [Model.Update] may wait for that. Every supervisor call happens
// inside a [tea.Cmd] on its own goroutine and reports back as a message, so a
// 30s start is 30s of a live, scrollable, quittable console rather than 30s of
// a frozen terminal.
//
// # Nothing is left running behind the console
//
// The log pane follows a service by holding a cancellable
// supervisor.Supervisor.Tail in a goroutine. Selecting another service
// cancels the previous tail, drains its channel so the producer cannot block
// on a send nobody will receive, and only then starts the next one. A console
// left open for an afternoon must hold exactly one tail, not one per keypress.
// [Model.Shutdown] does the same for the last one, and Run defers it so a
// panic unwinding through the event loop still releases it.
//
// # Quitting is not stopping
//
// Services are spawned detached (setsid), so mabo-ctl is not their parent and
// closing the console is closing a window, not shutting anything down. The
// help overlay says so in as many words, because the opposite assumption —
// "I quit the TUI, so my dev servers are down" — leads to a confusing second
// session where every port is mysteriously held.
//
// Layering: this package drives the supervisor and borrows internal/ui for the
// phase glyphs. It imports neither os/exec nor syscall; every process
// operation goes through the supervisor.
package console

import (
	"context"
	"errors"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// Controller is the slice of *supervisor.Supervisor that the console drives.
//
// It exists so the console's model can be exercised without spawning a single
// process: the tests substitute a fake and drive [Model.Update] directly.
// *supervisor.Supervisor satisfies it by construction, and the signatures are
// copied verbatim from the supervisor API — this interface deliberately adds
// nothing of its own.
type Controller interface {
	// Start starts names (empty means every service) and reports progress on
	// ev. It blocks until every service has settled or ctx is done, which can
	// be as long as the configured readiness timeout.
	Start(ctx context.Context, names []string, ev chan<- supervisor.Event) error
	// Stop stops names (empty means every service), blocking for up to the
	// configured stop grace per service.
	Stop(ctx context.Context, names []string, ev chan<- supervisor.Event) error
	// Restart stops then starts names (empty means every service).
	Restart(ctx context.Context, names []string, ev chan<- supervisor.Event) error
	// Status reports the current phase of every service, in declaration order.
	// It may issue health probes and therefore may block.
	Status(ctx context.Context) []supervisor.Status
	// Tail sends svc's log lines to out until ctx is done. With follow set it
	// does not return on its own; cancelling ctx is the only way to stop it.
	Tail(ctx context.Context, svc string, n int, follow bool, out chan<- string) error
}

// describer is implemented by a controller that can describe the services it
// supervises. *supervisor.Supervisor does, which is how the plain [Run] picks
// up the colours declared in mabo-ctl.yaml without the caller passing them. It
// is an optional interface: a controller that does not implement it simply
// gets the fallback palette.
type describer interface {
	Instances() []service.Instance
}

// serviceColors reads the declared colour of every service ctrl supervises,
// and returns nil when ctrl cannot describe them.
func serviceColors(ctrl Controller) map[string]string {
	d, ok := ctrl.(describer)
	if !ok {
		return nil
	}
	insts := d.Instances()
	colors := make(map[string]string, len(insts))
	for _, in := range insts {
		colors[in.Name] = in.Color
	}
	return colors
}

// Options carries what the console needs beyond the supervisor itself. The
// zero value is usable: the console then derives the root from a status log
// path and gives every service a stable colour derived from its name.
type Options struct {
	// Root is the absolute directory holding mabo-ctl.yaml, shown in the title
	// bar so a console started in the wrong repo is obvious at a glance. When
	// empty it is derived from the first status's log path.
	Root string

	// Colors maps service name to the colour declared in mabo-ctl.yaml. Names
	// absent from the map, and colours the terminal cannot express, fall back
	// to a stable colour derived from the service name.
	Colors map[string]string
}

// ErrNoSupervisor reports that the console was asked to run without anything
// to supervise. It is returned rather than panicking because a nil supervisor
// is a programming error in the caller, not a reason to lose the terminal.
var ErrNoSupervisor = errors.New("console: no supervisor")

// Run starts the full-screen console on the current terminal and blocks until
// the user quits. It returns nil on a normal quit.
//
// Run takes over the terminal (alternate screen, raw mode) and restores it
// before returning, including when the event loop panics: bubbletea's own
// panic handler restores the terminal, and Run additionally kills the program
// and shuts the model down from a defer, so no tail goroutine outlives the
// call.
//
// Quitting the console does NOT stop the supervised services.
//
// Run returns [ErrNoSupervisor] when sup is nil.
func Run(sup *supervisor.Supervisor) error {
	if sup == nil {
		return ErrNoSupervisor
	}
	return RunWith(sup, Options{})
}

// RunWith is [Run] with explicit [Options]. cmd/mabo-ctl uses it to name the
// config root and to pass the per-service colours from mabo-ctl.yaml; Run is the
// same call with an empty Options.
//
// It returns [ErrNoSupervisor] when ctrl is nil.
func RunWith(ctrl Controller, opt Options) error {
	if ctrl == nil {
		return ErrNoSupervisor
	}

	m := New(ctrl, opt)
	p := tea.NewProgram(m, tea.WithAltScreen())

	// Both defers run during a panic unwind as well as on a normal return.
	// Kill cancels the program's context so the terminal is restored even if
	// the panic came from outside bubbletea's own recover, and Shutdown
	// releases the tail regardless of which copy of the model owned it — the
	// live state is shared by every copy for exactly this reason.
	defer m.Shutdown()
	defer p.Kill()

	// The supervisor is deliberately NOT waited on here. Its reaper goroutines
	// block in wait(2) on the children it spawned, and a service the user left
	// running never exits — waiting for them would hang the console on quit,
	// which is the opposite of "quitting is closing a window".
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("console: %w", err)
	}
	return nil
}
