package main

import (
	"context"

	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/maborak/mabo-ctl/internal/web"
)

// webConsoleAddr is where `mabo-ctl start --web-console` binds when --web-addr
// does not say otherwise.
//
// It is loopback with port 0 rather than web.DefaultAddr's fixed 7999 because
// the console is incidental to the start: a fixed port turns "a console is
// already open in another terminal" into a failed `mabo-ctl start`, and the
// kernel picking a free port is the only way to promise it cannot. Nothing is
// printed until the socket exists, so the URL always names the port that was
// actually bound.
const webConsoleAddr = "127.0.0.1:0"

// webConsoleClosing is [app.announceServe]'s last line for the console `mabo-ctl
// start --web-console` binds.
//
// It does not say Ctrl-C, because Ctrl-C does not stop this one: the prompt
// that owns the listener treats it as "abandon the line I am typing". Printing
// the `mabo-ctl serve` instruction here would leave a user hammering a key that
// does nothing.
const webConsoleClosing = `Leaving the prompt — "quit", "exit" or Ctrl-D — stops the console, and so does
typing "unserve". The services keep running either way.`

// startMode is what `mabo-ctl start` does once every selected service has
// settled: exit, follow the logs, or hand the terminal to one of mabo-ctl's three
// front ends.
//
// Exiting is the default and it is the ONLY thing that happens without a
// terminal. A resident mode reached from a script, a Makefile or a CI job would
// sit at a prompt nobody is typing at until the job timed out, so every field
// below is a request that [app.handOff] may refuse.
type startMode struct {
	// follow is -f: tail the started services' logs until interrupted.
	follow bool
	// attach is -a: hand off to the full-screen console.
	attach bool
	// interactive is -i: drop into the line-oriented prompt. --web-console
	// turns it on too, because the console it binds needs a resident host.
	interactive bool
	// web is --web-console: bind the web console and print its URL.
	web bool
	// webAddr is --web-addr, the address --web-console binds.
	webAddr string
	// allowOrigins is --allow-origin: extra browser origins the console accepts.
	allowOrigins []string
	// webForce is --i-know-this-is-dangerous: permission for webAddr to be a
	// non-loopback address. Nothing else in mabo-ctl may set it, and no other
	// flag implies it.
	webForce bool
}

// resident reports whether this mode keeps mabo-ctl running once the start is
// done, rather than exiting.
func (m startMode) resident() bool { return m.attach || m.interactive }

// flag names the flag that asked for residency, for an error or a notice about
// the mode the user requested.
func (m startMode) flag() string {
	switch {
	case m.attach:
		return "--attach"
	case m.web:
		return "--web-console"
	default:
		return "--interactive"
	}
}

// startModeFor reads the resident flags off the executing command and rejects
// the combinations that have no meaning.
//
// Every rejection is a usage error, exit code 2, and every one of them is
// decided BEFORE anything is started or bound: a command line mabo-ctl will not
// carry out must not half-happen.
func startModeFor(cmd *cobra.Command) (startMode, error) {
	m := startMode{
		follow:      boolFlag(cmd, "follow"),
		attach:      boolFlag(cmd, "attach"),
		interactive: boolFlag(cmd, "interactive"),
		web:         boolFlag(cmd, "web-console"),
		webAddr:     stringFlag(cmd, "web-addr", webConsoleAddr),
		webForce:    boolFlag(cmd, "i-know-this-is-dangerous"),
	}
	// A mistyped origin must fail here, with the rest of the mode validation,
	// rather than after the stack is already running.
	if allow, err := cmd.Flags().GetStringArray("allow-origin"); err == nil {
		m.allowOrigins = allow
	}
	if m.webAddr == "" {
		m.webAddr = webConsoleAddr
	}

	// Each of these claims the terminal for something different once the start
	// returns, and there is only one terminal. --interactive and --web-console
	// are the single exception, and not a courtesy: --web-console has to stay
	// resident for its socket to be worth binding, and the prompt is where it
	// stays, so asking for both asks for one thing.
	var claims []string
	if m.follow {
		claims = append(claims, "--follow")
	}
	if m.attach {
		claims = append(claims, "--attach")
	}
	switch {
	case m.interactive && m.web:
		claims = append(claims, "--interactive with --web-console")
	case m.interactive:
		claims = append(claims, "--interactive")
	case m.web:
		claims = append(claims, "--web-console")
	}
	if len(claims) > 1 {
		return startMode{}, usageErrorf(
			"%s cannot be combined: each of them hands this terminal to something different once the services are up, so pick one",
			joinAnd(claims))
	}

	if !m.web {
		if flagChanged(cmd, "web-addr") {
			return startMode{}, usageErrorf(
				"--web-addr does nothing without --web-console; add that flag, or use `mabo-ctl serve --addr %s` to bind a console on its own",
				m.webAddr)
		}
		if m.webForce {
			return startMode{}, usageErrorf(
				"--i-know-this-is-dangerous does nothing without --web-console; it authorises a non-loopback --web-addr and it authorises nothing else")
		}
	}
	// The web console outlives the start only for as long as mabo-ctl does, so it
	// needs a resident host, and the prompt is it.
	if m.web {
		// Validate the address HERE, where the decision is pure and does not
		// depend on there being a terminal.
		//
		// This check used to live only inside newWebConsole, which
		// prepareHandOff skips when stdin is not a terminal — so
		// `mabo-ctl start --web-console --web-addr 0.0.0.0:0` was refused with
		// exit 2 at a prompt and accepted with exit 0 through a pipe, having
		// started every service on the way. Nothing was ever bound either way,
		// so nothing was exposed; but a command line mabo-ctl will not carry out
		// must not half-happen, and a security control whose answer depends on
		// whether a terminal is attached is not one anybody can reason about.
		if err := web.CheckAddr(m.webAddr, m.webForce); err != nil {
			return startMode{}, withCode(exitUsage, err)
		}
		// Same reasoning for the origins: a mistyped --allow-origin is a
		// command line mabo-ctl will not carry out, so it is refused before any
		// service is started rather than after.
		for _, o := range m.allowOrigins {
			// The same rule the server will apply: the bare wildcard needs the
			// danger flag, and refusing it here means refusing before anything
			// starts rather than after.
			norm := web.NormalizeOrigin
			if m.webForce {
				norm = web.NormalizeOriginAllowingAny
			}
			if _, err := norm(o); err != nil {
				return startMode{}, withCode(exitUsage, err)
			}
		}
		m.interactive = true
	}
	return m, nil
}

// prepareHandOff decides, BEFORE anything is started, whether the resident mode
// the user asked for can run at all, and builds the web console when it can.
//
// Everything that can be refused is refused here, with nothing started and
// nothing bound: a nested session, an address mabo-ctl will not expose, a token
// it could not generate. The SOCKET is not bound here, though — [app.webConsole]
// does that after the start, because until it is bound there is no port to name
// and the printed URL has to be one that works.
//
// A resident mode cannot nest. Inside the prompt mabo-ctl is already the resident
// session, and a second one would give two loops one stdin.
func (a *app) prepareHandOff(m startMode) (*web.Server, error) {
	if a.inREPL {
		return nil, usageErrorf(
			"%s cannot be used from inside the interactive console, which is already mabo-ctl's resident session; "+
				`type "serve" here for the web console, or quit and run "mabo-ctl start %s" from the shell`,
			m.flag(), m.flag())
	}
	if !m.web || !a.env.IsTTY() {
		return nil, nil
	}
	a.allowOrigins = m.allowOrigins
	return a.newWebConsole(m.webAddr, m.webForce)
}

// handOff runs the resident front end m asked for and returns the exit status
// of the whole command. console is what [app.prepareHandOff] built, or nil.
//
// outcome is what the start itself produced — nil, or an error already carrying
// its exit code. Two rules are load-bearing here.
//
// WITHOUT A TERMINAL NOTHING IS RESIDENT. `mabo-ctl start` in a script, a
// Makefile or CI must behave exactly as it does with no flag at all: the
// request is reported on stderr and refused, stdout is untouched, and the exit
// code is the start's own. A console handed a pipe would wait forever for input
// nobody is typing, and --web-console would bind a socket and immediately drop
// it as mabo-ctl exited. The terminal question is [Env.IsTTY], which is injected,
// so both branches are testable without a terminal.
//
// A FAILED START IS NOT FORGIVEN BY A CLEAN QUIT. The console is opened even
// when the start failed — that is exactly when a human wants the logs and a
// restart key — and quitting it returns outcome, so `mabo-ctl start -a` still
// exits 4 for a service that never became ready. A session that fails on its
// own account reports both, with the start's code winning, because "it never
// came up" is the more actionable half.
func (a *app) handOff(cmd *cobra.Command, m startMode, console *web.Server, outcome error) error {
	if !a.env.IsTTY() {
		fmt.Fprintf(a.env.Stderr,
			"mabo-ctl: %s needs a terminal and this is not one, so it was ignored: "+
				"a resident session reading input nobody is typing never ends. "+
				"The services were started and mabo-ctl is exiting as it would without the flag.\n",
			m.flag())
		return outcome
	}

	// The crash watcher runs alongside WHATEVER resident front end was asked
	// for: it reads the supervisor itself and needs nothing from the console
	// or the prompt. Its lifetime is exactly the residency — cancelled when
	// this function returns, which happens when the front end quits.
	var stopWatch func()
	if boolFlag(cmd, "notify") {
		stopWatch = a.watchDeaths(context.Background())
		defer stopWatch()
	}

	var err error
	switch {
	case m.attach:
		err = a.attachConsole()
	case m.web:
		err = a.webConsole(cmd, console)
	default:
		err = a.prompt(cmd, nil)
	}
	if err != nil {
		if outcome != nil {
			return errors.Join(outcome, err)
		}
		return err
	}
	return outcome
}

// watchDeaths starts a [notifier] against the real supervisor and returns its
// stop function. It reports nothing at startup: a watcher that announced
// itself would train the operator to dismiss mabo-ctl notifications.
func (a *app) watchDeaths(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	sup, err := a.realSupervisor()
	if err != nil {
		cancel()
		return func() {}
	}
	n := newNotifier(sup, sendDesktopNotification)
	go n.watch(ctx)
	return cancel
}

// attachConsole hands the terminal to the full-screen console, over the same
// supervisor that has just done the starting.
//
// Quitting it does not stop anything: the services were spawned detached, and
// closing a window is not a shutdown request.
func (a *app) attachConsole() error {
	sup, err := a.realSupervisor()
	if err != nil {
		return err
	}
	return a.env.RunConsole(sup)
}

// webConsole binds srv, prints its URL, and hands the terminal to the prompt —
// which ADOPTS the listener, so there is exactly one owner and exactly one
// shutdown. Ctrl-C belongs to the prompt and does not touch the socket; a
// cancelled context ends the session, and it is the session closing that
// releases it. Both routes run the same shutdown once. A console this function
// kept for itself would need a second one, and a socket with two owners is a
// socket with none.
//
// The bind happens before the URL is printed, because the default port is 0 and
// a URL printed from the requested address would say 0. It happens after the
// start because the console is what a human reads once the stack is up.
func (a *app) webConsole(cmd *cobra.Command, srv *web.Server) error {
	if srv == nil {
		return errors.New("mabo-ctl: --web-console reached the hand-off with no console built")
	}
	if err := srv.Listen(); err != nil {
		return err
	}
	a.announceServe(srv, webConsoleClosing)
	return a.prompt(cmd, srv)
}
