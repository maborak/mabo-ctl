package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/web"
)

// rootLong is the root command's long help. The exit codes are documented here
// because they are mabo-ctl's interface to any script that calls it.
const rootLong = `mabo-ctl supervises the long-running local development processes declared in
mabo-ctl.yaml: it starts, stops, restarts, health-checks, tails and inspects them,
and keeps enough state in .dev/ that a second terminal knows what the first one
started.

mabo-ctl.yaml is found by walking UP from the current directory, the way git finds
.git, so mabo-ctl works from any subdirectory. --config skips that search.

  mabo-ctl                 no arguments, on a terminal: open the interactive console
  mabo-ctl                 no arguments, piped or redirected: print the status block
  mabo-ctl <service>       shorthand for "mabo-ctl start <service>"
  mabo-ctl repl            open a resident prompt that runs these same commands
  mabo-ctl serve           serve the web console on http://127.0.0.1:7999

Three flags make "mabo-ctl start" stay instead of exiting. All three need a
terminal, and without one they are reported on stderr and ignored, so mabo-ctl in
a script, a Makefile or CI behaves exactly as it does with no flag at all:

  mabo-ctl start -a           start, then hand off to the full-screen console
  mabo-ctl start -i           start, then drop into the resident prompt
  mabo-ctl start --web-console
                            start, then serve the web console on a free loopback
                            port, print its URL with its token, and hold it open
                            at the prompt. It never implies
                            --i-know-this-is-dangerous

Port precedence, highest first:

  1. --ports=A,B,C,D                positional; an empty slot keeps the default
  2. <NAME>_PORT in the environment captured AND unset before anything spawns
  3. .dev/run.env                   persisted from the previous run
  4. the port declared in mabo-ctl.yaml

A persisted port that outranks a changed default is announced on stderr; stale
state never wins silently.

Exit codes:

  0  success
  1  a runtime failure
  2  a usage error, such as an unknown service or an unknown flag
  3  mabo-ctl.yaml is missing, unreadable or invalid
  4  a service failed to become ready inside ready_timeout

  mabo-ctl exec is the one exception: it forwards the child's exit code verbatim.

A resident mode does not swallow the start that failed inside it: quit the
console or the prompt however cleanly you like, "mabo-ctl start -a", "-i" and
"--web-console" still exit 4 when a selected service never became ready.

SECURITY: mabo-ctl.yaml declares commands and mabo-ctl runs them. Running mabo-ctl in
a repository you do not trust runs that repository's code as you.

mabo-ctl serve puts that behind an HTTP listener: its start, stop and restart
routes run those same commands. It binds 127.0.0.1 only, requires the session
token printed with its URL on every mutation, and refuses a non-loopback address
unless --i-know-this-is-dangerous says otherwise — which exposes those routes,
and therefore your dev stack, to every machine that can route to the address.`

// rootCmd builds the whole command tree.
//
// The root itself accepts service names, which is the `mabo-ctl <svc>` shorthand
// for `mabo-ctl start <svc>`, and therefore carries the same flags as start. With
// no arguments and no start flags it runs the default action: the console on a
// terminal, the status block anywhere else.
func (a *app) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "mabo-ctl [service...]",
		Short: "Supervise the local development processes declared in mabo-ctl.yaml",
		Long:  rootLong,
		// Setting Version gives --version for free. The stamps come from
		// -ldflags at link time; an unstamped build reports "dev".
		Version: fmt.Sprintf("%s (commit %s)", version, commit),
		// Arbitrary args, because a bare service name is the start shorthand and
		// cobra's default would reject it as an unknown command before mabo-ctl
		// could say which services actually exist.
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			a.reconcileConfig(cmd.Root().PersistentFlags().Lookup("config").Value.String())
			// Global, like --config: port drift is a repository condition, not
			// a property of one command, and every command that resolves ports
			// can meet it. Deliberately NOT in startFlagNames — it must not
			// turn a bare `mabo-ctl --refresh-ports` into a start.
			a.refreshPorts = boolFlag(cmd, "refresh-ports")
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && !startFlagsChanged(cmd) {
				return a.runDefault(cmd)
			}
			return a.runStart(cmd, args)
		},
	}

	root.SetIn(a.env.Stdin)
	root.SetOut(a.env.Stdout)
	root.SetErr(a.env.Stderr)
	root.SetArgs(a.env.Args)
	// A flag error must exit 2, not 1, and must be followed by the usage text.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError(err) })
	// mabo-ctl ships its own completion command with a narrower surface than
	// cobra's default, which also offers shells mabo-ctl does not support.
	root.CompletionOptions.DisableDefaultCmd = true

	root.PersistentFlags().String("config", "", "path to mabo-ctl.yaml; skips the walk up the directory tree")
	root.PersistentFlags().Bool("refresh-ports", false,
		"re-resolve every port from the declared defaults, ignoring persisted .dev/run.env, and rewrite the file")
	addStartFlags(root)

	root.AddCommand(
		a.startCmd(),
		a.stopCmd(),
		a.restartCmd(),
		a.statusCmd(),
		a.healthCmd(),
		a.configCmd(),
		a.logsCmd(),
		a.resetCmd(),
		a.preflightCmd(),
		a.execCmd(),
		a.shellCmd(),
		a.openCmd(),
		a.serveCmd(),
		a.replCmd(),
		a.completionCmd(),
		a.upgradeCmd(),
		a.schemaCmd(),
		a.doctorCmd(),
	)
	return root
}

// addStartFlags registers the flags `start` accepts. They live on the root
// command too, so `mabo-ctl backend -f` works as the start shorthand.
func addStartFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.BoolP("follow", "f", false, "after starting, follow the logs of the started services until interrupted")
	f.Bool("all", false, "start EVERY declared service, including any with autostart: false; naming nothing starts only the autostart ones")
	f.Var(&portsFlag{}, "ports", "positional port overrides, e.g. --ports=,,7999; an empty slot keeps the declared default")
	f.Var(&namedPortsFlag{}, "port", "named port override SERVICE=PORT, e.g. --port backend=7999; repeatable. Cannot be combined with --ports")
	f.BoolP("attach", "a", false,
		"after starting, hand off to the full-screen console instead of exiting; needs a terminal, and is ignored without one")
	f.BoolP("interactive", "i", false,
		"after starting, drop into the interactive prompt instead of exiting; needs a terminal, and is ignored without one")
	f.StringArray("allow-origin", nil,
		"additional browser origin the web console accepts, e.g. https://dev.tunnel.example; repeatable. "+
			"Needed when the console is reached through a tunnel or port forward. Editable later in the console itself")
	f.Bool("web-console", false,
		"after starting, serve the web console on a free loopback port and print its URL; it lasts only as long as mabo-ctl does, "+
			"so it implies --interactive and, like it, needs a terminal")
	f.String("web-addr", webConsoleAddr,
		"address for --web-console as host:port; port 0 lets the kernel pick a free one. A non-loopback host requires --i-know-this-is-dangerous")
	f.Bool("i-know-this-is-dangerous", false,
		"permit --web-addr to bind a non-loopback address, exposing start/stop/restart to every machine that can route to it")
}

// startFlagNames are the flags that make a bare `mabo-ctl` a start rather than
// the default action. Every flag [addStartFlags] registers belongs here: one
// that did not would be accepted and then silently ignored.
var startFlagNames = []string{
	"follow", "all", "ports", "port",
	"attach", "interactive", "web-console", "web-addr", "i-know-this-is-dangerous",
	"allow-origin",
}

// startFlagsChanged reports whether the user set any of the start flags, which
// makes a bare `mabo-ctl --all` a start rather than the default action.
func startFlagsChanged(cmd *cobra.Command) bool {
	for _, name := range startFlagNames {
		if flagChanged(cmd, name) {
			return true
		}
	}
	return false
}

// adoptPorts copies the executing command's --ports and --port values onto the
// app, so resolution sees them whether they arrived on `start` or on the root
// shorthand. The two spellings are mutually exclusive: positional slots and
// named overrides both winning would be resolved by accident, and a silent
// last-wins between two explicit requests is exactly the untruth this tool
// refuses to print.
func (a *app) adoptPorts(cmd *cobra.Command) error {
	positional := cmd.Flags().Lookup("ports")
	named := cmd.Flags().Lookup("port")
	if positional != nil && named != nil && positional.Changed && named.Changed {
		return usageErrorf("--ports and --port cannot be combined: name the services (--port svc=PORT) or the positions (--ports=A,B,C), not both")
	}
	if f := positional; f != nil {
		pf, ok := f.Value.(*portsFlag)
		if !ok {
			return fmt.Errorf("mabo-ctl: --ports has unexpected type %T", f.Value)
		}
		a.ports = *pf
	}
	if f := named; f != nil {
		nf, ok := f.Value.(*namedPortsFlag)
		if !ok {
			return fmt.Errorf("mabo-ctl: --port has unexpected type %T", f.Value)
		}
		a.portOverrides = nf.values
	}
	return nil
}

// boolFlag reads a registered boolean flag, defaulting to false when the
// command does not define it.
func boolFlag(cmd *cobra.Command, name string) bool {
	v, err := cmd.Flags().GetBool(name)
	if err != nil {
		return false
	}
	return v
}

// stringFlag reads a registered string flag, falling back to def when the
// command does not define it.
func stringFlag(cmd *cobra.Command, name, def string) string {
	v, err := cmd.Flags().GetString(name)
	if err != nil {
		return def
	}
	return v
}

// flagChanged reports whether the user set name on the executing command, as
// opposed to it holding its declared default.
func flagChanged(cmd *cobra.Command, name string) bool {
	f := cmd.Flags().Lookup(name)
	return f != nil && f.Changed
}

// runDefault implements a bare `mabo-ctl`.
//
// On a terminal it opens the interactive console. Anywhere else — a pipe, a
// redirect, a CI log — it prints the status block and exits, because a
// full-screen TUI rendered into a pipe is unreadable garbage and `mabo-ctl | head`
// is a thing people type.
func (a *app) runDefault(cmd *cobra.Command) error {
	if !a.env.IsTTY() {
		return a.runStatus(cmd, false)
	}
	sup, err := a.realSupervisor()
	if err != nil {
		return err
	}
	return a.env.RunConsole(sup)
}

// startCmd builds `mabo-ctl start`.
func (a *app) startCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start [service...]",
		Short: "Start services and wait for them to become ready",
		Long: `Start starts the named services, or every declared service when none is named,
and waits for each one's health URL to answer.

Dependencies start first. A service that is already running is left alone. A
service whose port is held by a process mabo-ctl did not start is refused, with the
lsof command that names the holder.

Exit code 4 means at least one service failed to become ready: it either died
while starting, or was still not answering after ready_timeout.

Three flags make mabo-ctl STAY instead of exiting, and all three need a terminal:

  -a, --attach       hand off to the full-screen console
  -i, --interactive  drop into the interactive prompt
      --web-console  serve the web console on a loopback port the kernel picks,
                     print its URL with its token on a line of its own, and hold
                     it open at the interactive prompt

Only one of them, and only one of them and --follow, can be given: each hands
this one terminal to something different. Combining two is a usage error and
starts nothing.

WITHOUT A TERMINAL they are announced on stderr and ignored, and mabo-ctl starts
the services and exits exactly as it would with no flag at all. That is what
keeps mabo-ctl usable in a script, a Makefile and CI: a prompt nobody is typing at
would hang the job, and --web-console would bind a socket only to drop it a
moment later.

A start that failed is not forgiven by a clean quit. Leaving the console or the
prompt still exits 4 when a selected service never became ready.

--web-console binds 127.0.0.1 on a free port; --web-addr overrides that. It does
NOT imply --i-know-this-is-dangerous, which stays separate and explicit: a
non-loopback --web-addr is refused with exit 2 and binds nothing unless that
flag is given as well.`,
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          a.runStart,
	}
	addStartFlags(cmd)
	return cmd
}

// runStart resolves the selection, starts it, prints the resulting status
// block, and then either exits, follows the logs, or hands the terminal to one
// of the front ends.
//
// The resolved ports are persisted to .dev/run.env first, so the next
// invocation from any terminal resolves the same ports.
//
// The mode is parsed BEFORE anything else, because a flag combination mabo-ctl
// will not carry out must be refused with nothing started. And a resident mode
// runs even when the start failed — that is when a human most wants the logs
// and a restart key — so [app.handOff] carries the start's exit status through
// the session rather than the session's clean quit erasing it.
func (a *app) runStart(cmd *cobra.Command, args []string) error {
	mode, err := startModeFor(cmd)
	if err != nil {
		return err
	}
	if err := a.adoptPorts(cmd); err != nil {
		return err
	}
	names, err := a.selection(cmd, args)
	if err != nil {
		return err
	}
	sup, insts, err := a.supervisor()
	if err != nil {
		return err
	}
	if err := service.Persist(a.st, insts); err != nil {
		return err
	}

	// Whatever the hand-off can refuse, it refuses now, while refusing is free:
	// a start that happened and then reported a usage error would leave the user
	// to work out which half of the command line took effect.
	var console *web.Server
	if mode.resident() {
		if console, err = a.prepareHandOff(mode); err != nil {
			return err
		}
	}

	outcome := a.startServices(cmd, sup, names)

	if mode.resident() {
		return a.handOff(cmd, mode, console, outcome)
	}
	if outcome != nil {
		return outcome
	}
	if mode.follow {
		return a.runTail(cmd, names, defaultTailLines, true)
	}
	return nil
}

// startServices starts names, renders the events as they arrive and the status
// block once they have settled, and returns the exit status of the start
// itself: nil, or an error already tagged with the code it exits with.
//
// The SIGINT registration lives and dies inside this call, so whatever runs
// afterwards — a log tail, a console, a prompt — owns Ctrl-C outright.
func (a *app) startServices(cmd *cobra.Command, sup lifecycle, names []string) error {
	ctx, cancel := interruptible(cmd.Context())
	defer cancel()

	ev, wait := a.pumpEvents()
	startErr := sup.Start(ctx, names, ev)
	wait()

	// The final status is taken with the uncancelled parent context, so a
	// Ctrl-C during startup still produces a truthful report of what is running.
	sts := filterStatus(sup.Status(cmd.Context()), names)
	a.printStatus(sts)

	if bad := notReady(sts); len(bad) > 0 {
		err := fmt.Errorf("%s did not become ready; see the DETAIL column above and .dev/logs/", joinAnd(bad))
		if startErr != nil {
			err = errors.Join(startErr, err)
		}
		return withCode(exitNotReady, err)
	}
	if startErr != nil {
		// A service that was refused (its port is held) or skipped (its
		// dependency died) never spawns, so it reads as "stopped" rather than
		// "failed" and notReady above does not see it. It still did not become
		// ready, which is exactly what exit code 4 means.
		if errors.Is(startErr, supervisor.ErrNotStarted) {
			return withCode(exitNotReady, startErr)
		}
		return startErr
	}
	return nil
}

// stopCmd builds `mabo-ctl stop`.
func (a *app) stopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop [service...]",
		Short: "Stop services, killing the whole process group",
		Long: `Stop signals the named services, or every declared service when none is named.

Naming services stops exactly those: depends_on orders STARTS, not stops, so a
service's dependencies are left running.

SIGTERM goes to the process GROUP, not the bare pid, and is followed by SIGKILL
after stop_grace. Signalling the pid alone leaves the child that "npm run dev"
spawned holding the port, which is how the predecessor accumulated orphans.

The pid file is removed only once the process is confirmed dead.`,
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			names, err := a.selection(cmd, args)
			if err != nil {
				return err
			}
			sup, _, err := a.supervisor()
			if err != nil {
				return err
			}
			ctx, cancel := interruptible(cmd.Context())
			defer cancel()

			ev, wait := a.pumpEvents()
			stopErr := sup.Stop(ctx, names, ev)
			wait()

			a.printStatus(filterStatus(sup.Status(cmd.Context()), names))
			return stopErr
		},
	}
	// --all means what it means on `start`. Naming no service already stops
	// everything, so this flag adds no capability — it removes a papercut:
	// `mabo-ctl start --all` works, so `mabo-ctl stop --all` gets typed, and it
	// used to exit 2 with a usage error. A script that mirrored the start line
	// failed for a reason that had nothing to do with the services.
	cmd.Flags().Bool("all", false, "stop every declared service (the same as naming none)")
	return cmd
}

// restartCmd builds `mabo-ctl restart`.
func (a *app) restartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restart [service...]",
		Short: "Stop then start services",
		Long: `Restart stops the named services, or every declared service when none is named,
and starts them again, waiting for readiness exactly as start does.

Exit code 4 means at least one service failed to become ready.`,
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			names, err := a.selection(cmd, args)
			if err != nil {
				return err
			}
			sup, insts, err := a.supervisor()
			if err != nil {
				return err
			}
			if err := service.Persist(a.st, insts); err != nil {
				return err
			}
			ctx, cancel := interruptible(cmd.Context())
			defer cancel()

			ev, wait := a.pumpEvents()
			restartErr := sup.Restart(ctx, names, ev)
			wait()

			sts := filterStatus(sup.Status(cmd.Context()), names)
			a.printStatus(sts)

			if bad := notReady(sts); len(bad) > 0 {
				err := fmt.Errorf("%s did not become ready; see the DETAIL column above and .dev/logs/", joinAnd(bad))
				if restartErr != nil {
					err = errors.Join(restartErr, err)
				}
				return withCode(exitNotReady, err)
			}
			if restartErr != nil {
				// Same reasoning as runStart: a refused or skipped service is
				// "stopped", not "failed", so only the error names it.
				if errors.Is(restartErr, supervisor.ErrNotStarted) {
					return withCode(exitNotReady, restartErr)
				}
				return restartErr
			}
			if boolFlag(cmd, "follow") {
				return a.runTail(cmd, names, defaultTailLines, true)
			}
			return nil
		},
	}
	cmd.Flags().BoolP("follow", "f", false, "after restarting, follow the logs of the restarted services until interrupted")
	cmd.Flags().Bool("all", false, "restart every declared service (the same as naming none)")
	return cmd
}

// resetCmd builds `mabo-ctl reset`.
func (a *app) resetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Stop everything, reap orphans by port, and delete .dev/",
		Long: `Reset stops every service and removes the .dev/ state directory.

A declared port that is STILL held once everything has been stopped belongs to a
process mabo-ctl did not start: an orphan from a previous run whose pid file went
stale, or something unrelated. Reset names each one, with the lsof command that
identifies it, and leaves it alone. --force kills it instead.

That gate is the point. Reaping by PORT rather than by pid file is the only way
to catch an orphan — the port is ground truth where a pid file is a guess — and
it is also a destructive act against a process the user may care about.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sup, _, err := a.supervisor()
			if err != nil {
				return err
			}
			ctx, cancel := interruptible(cmd.Context())
			defer cancel()

			ev, wait := a.pumpEvents()
			resetErr := sup.Reset(ctx, boolFlag(cmd, "force"), ev)
			wait()
			return resetErr
		},
	}
	cmd.Flags().Bool("force", false, "kill whatever still holds a declared port, even though mabo-ctl did not start it")
	return cmd
}
