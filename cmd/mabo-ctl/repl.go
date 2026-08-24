package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/maborak/mabo-ctl/internal/repl"
	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/web"
)

// replLong is `mabo-ctl repl`'s long help.
const replLong = `Repl opens a prompt that stays resident and runs mabo-ctl commands.

Every line is handed to the SAME command tree this binary uses from the shell,
so "start api", "logs web -f", "exec api pytest", "reset --force" and every
other command and flag behave identically to typing "mabo-ctl" in front of them.
There is no separate list of console commands to learn or to drift.

Two verbs belong to the session rather than to the command tree:

  serve [host:port]   bind the web console for as long as this session lasts and
                      print its URL. Running it again prints the same URL rather
                      than binding a second time; the command-line "mabo-ctl serve"
                      would instead block until interrupted.
  unserve             stop that console and release its port.

Because the prompt is resident it is the one place mabo-ctl can notice a service
dying while you watch: a crash is written into the scrollback as it happens,
rather than waiting for the next time somebody runs "mabo-ctl status".

Ctrl-C abandons the line being typed, or cancels the command that is running. It
does not leave the console and it does not stop anything. Quitting — "quit",
"exit" or Ctrl-D — leaves every service RUNNING: they are spawned detached, so
closing the console is closing a window. Type "stop" first to take them down.`

// replCmd builds `mabo-ctl repl`.
func (a *app) replCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "repl",
		Short:         "Open a resident prompt that runs mabo-ctl commands and reports crashes",
		Long:          replLong,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          a.runREPL,
	}
}

// runREPL opens the interactive prompt with nothing bound.
// runREPL opens the interactive prompt from the `repl` command.
//
// The terminal check lives HERE rather than in prompt, because prompt is also
// the hand-off target for `start --interactive`, which handOff has already
// gated, and because the tests drive prompt directly with a scripted reader.
// This is the command a user types, and it is the one resident entry point that
// had no guard: piped into a script or a CI step it would sit waiting on a
// prompt nobody is typing at, and the pipeline would hang forever.
func (a *app) runREPL(cmd *cobra.Command, _ []string) error {
	if !a.env.IsTTY() {
		fmt.Fprintln(a.env.Stderr,
			"mabo-ctl: the interactive console needs a terminal and this is not one")
		return withCode(exitUsage, errors.New("the interactive console requires a terminal"))
	}
	return a.prompt(cmd, nil)
}

// prompt opens the interactive prompt, optionally adopting a web console the
// caller has already bound and announced — which is what `mabo-ctl start
// --web-console` hands over. The session owns it from here: it is released when
// the prompt is left, by the same path `unserve` uses.
//
// The supervisor is built here rather than lazily so a missing or invalid
// mabo-ctl.yaml fails with exit code 3 at the shell, where a script can see it,
// instead of failing once per line inside a console the user then has to quit.
func (a *app) prompt(cmd *cobra.Command, console repl.Listener) error {
	if a.inREPL {
		// The tree the console dispatches to is the whole tree, `repl` included.
		// Nesting one session inside another would give two loops reading the
		// same stdin and two crash watchers printing the same death.
		return usageErrorf(`already inside the interactive console; type "quit" to leave it`)
	}
	cfg, err := a.config()
	if err != nil {
		return err
	}
	lc, _, err := a.supervisor()
	if err != nil {
		return err
	}

	interrupts, stop := notifyInterrupts()
	defer stop()

	a.inREPL = true
	defer func() { a.inREPL = false }()

	return repl.Run(cmd.Context(), repl.Options{
		Repo:        filepath.Base(cfg.Root),
		In:          a.env.Stdin,
		Out:         a.env.Stdout,
		Commands:    &cobraDispatcher{a: a},
		NewListener: a.newConsoleListener,
		Console:     console,
		Watch:       replMonitor{lc: lc},
		Interrupts:  interrupts,
		Interactive: a.env.IsTTY(),
		FormatError: a.renderer().Error,
	})
}

// cobraDispatcher runs one console line against mabo-ctl's command tree.
//
// It is the whole of the console's command surface: there is no table of verbs
// here and there must never be one. Anything the CLI can do, the console can
// do, because it is the same tree — and nothing can be true of one and not the
// other, which is the drift that produced two diverging copies of the shell
// predecessor.
type cobraDispatcher struct{ a *app }

// Commands lists the tree's subcommands for the console's `help`.
//
// `repl` is left out on purpose: it is refused inside a session, so listing it
// would advertise a command that can only produce an error.
func (d *cobraDispatcher) Commands() []repl.Command {
	children := d.a.rootCmd().Commands()
	out := make([]repl.Command, 0, len(children))
	for _, c := range children {
		if !c.IsAvailableCommand() || c.Name() == "repl" {
			continue
		}
		out = append(out, repl.Command{Name: c.Name(), Short: c.Short})
	}
	return out
}

// Dispatch executes argv against a FRESH command tree and returns whatever the
// command returned.
//
// The tree is rebuilt per line, and that is not tidiness. A pflag value keeps
// whatever the last parse gave it, and cobra reuses the same *Command across
// invocations, so a tree held across lines would carry `--all` out of one
// `start` and into the next one — the user types `start api` and mabo-ctl starts
// everything. Rebuilding gives every flag its declared default back, and
// repl_test.go asserts it.
//
// The error is RETURNED, never printed and never fatal. Nothing on this path
// calls os.Exit: every command in the tree sets SilenceErrors and SilenceUsage,
// so one typo leaves the user at the prompt with a message above it rather than
// killing the session.
func (d *cobraDispatcher) Dispatch(ctx context.Context, argv []string) error {
	// --ports lives on the app rather than on the command, so it has to be
	// cleared by hand; the fresh tree cannot do it.
	//
	// Clearing the flag is not enough on its own: app.resolve memoises on
	// a.resolved, and a.ports is only read inside the branch memoisation skips.
	// The prompt resolves once before the loop starts, so every --ports typed at
	// the prompt was silently inert. Drop the resolution with the flag so each
	// line resolves against what it actually asked for.
	d.a.ports = portsFlag{}
	d.a.invalidateResolution()

	// A session is bound to ONE repository. --config on a single line looked
	// like a one-off but rebound everything: reconcileConfig writes
	// a.configPath, the seeding below then makes every later line inherit it,
	// while the prompt string and the crash watcher stay pinned to the repo the
	// session opened with. Half the session would be talking about a different
	// project than the other half. Refusing is the honest answer; the user can
	// quit and reopen against the other config.
	for i, a := range argv {
		if a == "--config" || strings.HasPrefix(a, "--config=") {
			_ = i
			return usageErrorf(
				"--config cannot be changed inside the interactive console: this session is bound to %s. "+
					`Type "quit" and start mabo-ctl again with the other config.`, d.a.configPath)
		}
	}

	root := d.a.rootCmd()
	// The session's --config must survive a line that does not repeat it.
	// PersistentPreRun reconciles the parsed value against the one bootstrap
	// used, and a fresh tree parses "" for a flag nobody typed — which would
	// silently drop `mabo-ctl --config x repl` back to walking up the tree on the
	// second line. Seeding the default makes "not typed" mean "unchanged".
	if f := root.PersistentFlags().Lookup("config"); f != nil && d.a.configPath != "" {
		f.DefValue = d.a.configPath
		if err := f.Value.Set(d.a.configPath); err != nil {
			return err
		}
	}
	root.SetArgs(argv)
	_, err := root.ExecuteContextC(ctx)
	return err
}

// replMonitor adapts the lifecycle backend to what the console's crash watcher
// reads.
//
// It goes through [lifecycle] rather than the concrete supervisor so the
// watcher is exercised by the same fake the rest of the CLI tests use. The
// translation is where a supervisor phase becomes a yes/no answer to "is this
// service gone without mabo-ctl having stopped it": PhaseExited is a crash the
// console should announce, PhaseFailed is a death during startup that whatever
// ran `start` has already reported in the foreground, and every other phase is
// not a death at all.
type replMonitor struct{ lc lifecycle }

// Status reports each service's death, or absence of one.
func (m replMonitor) Status(ctx context.Context) []repl.Status {
	// The watcher reads only the phase, so it must not fork lsof per stopped
	// service on every two-second tick.
	sts := m.lc.StatusNoPorts(ctx)
	out := make([]repl.Status, 0, len(sts))
	for _, st := range sts {
		dead := st.Phase == supervisor.PhaseExited || st.Phase == supervisor.PhaseFailed
		out = append(out, repl.Status{
			Name:       st.Name,
			Dead:       dead,
			Startup:    st.Phase == supervisor.PhaseFailed,
			ExitCode:   st.ExitCode,
			ExitSignal: st.ExitSignal,
			StartedAt:  st.StartedAt,
			ExitedAt:   st.ExitedAt,
		})
	}
	return out
}

// newConsoleListener builds the web console the prompt's `serve` binds.
//
// --i-know-this-is-dangerous is deliberately unreachable from here, which is
// why force is hard-coded false. A non-loopback bind exposes start/stop/restart
// to every machine that can route to it, and authorising that is a decision to
// make on a command line where it is visible in shell history, not a word typed
// at a prompt.
func (a *app) newConsoleListener(addr string) (repl.Listener, error) {
	srv, err := a.newWebConsole(addr, false)
	if err != nil {
		// A typed *web.Server would be a non-nil repl.Listener holding nil.
		return nil, err
	}
	return srv, nil
}

// newWebConsole builds a web console over the resolved stack.
//
// It constructs and does not listen: the caller binds, so that the URL it
// prints is the address that was actually bound rather than the one that was
// asked for — they differ whenever the port was 0 or the port was taken.
//
// force is --i-know-this-is-dangerous, and it is a parameter rather than a
// field read from somewhere so that every caller has to state its answer. It is
// never derived from anything else: no flag that asks for a console implies
// permission to expose one to the network.
func (a *app) newWebConsole(addr string, force bool) (*web.Server, error) {
	if addr == "" {
		addr = web.DefaultAddr
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, usageErrorf("%q is not an address of the form host:port: %v", addr, err)
	}

	insts, err := a.resolve()
	if err != nil {
		return nil, err
	}
	sup, err := a.realSupervisor()
	if err != nil {
		return nil, err
	}
	// The console starts services, so its ports are persisted exactly as
	// `mabo-ctl start` persists them: a status printed in another terminal has to
	// agree with what the console is supervising.
	if err := service.Persist(a.st, insts); err != nil {
		return nil, err
	}

	srv, err := web.New(sup, web.Options{
		Addr:           addr,
		Force:          force,
		Origins:        a.origins,
		StateDir:       a.stateDir(),
		ExplicitConfig: a.configPath != "",
		AllowedOrigins: a.allowOrigins,
	})
	if err != nil {
		if errors.Is(err, web.ErrUnsafeAddr) {
			return nil, usageError(err)
		}
		return nil, err
	}
	return srv, nil
}

// notifyInterrupts turns SIGINT into a channel the console reads, and returns
// the function that stops the conversion.
//
// It exists because the console must NOT die on Ctrl-C: at the prompt a Ctrl-C
// abandons the line, and during a command it cancels that command. Registering
// a handler is what stops the default action — killing the process — from
// applying, and internal/repl does not import os/signal, so the registration
// belongs here with the rest of cmd/mabo-ctl's dealings with the operating
// system.
//
// The send is non-blocking over a buffer of one. A burst of Ctrl-C is one
// cancellation rather than a queue of them, and a full buffer means the console
// has not acted on the first yet.
func notifyInterrupts() (<-chan struct{}, func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)

	out := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-sig:
				select {
				case out <- struct{}{}:
				default:
				}
			case <-done:
				return
			}
		}
	}()

	var once sync.Once
	return out, func() {
		once.Do(func() {
			signal.Stop(sig)
			close(done)
		})
	}
}
