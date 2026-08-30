package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/web"
)

// serveLong is `mabo-ctl serve`'s long help.
//
// Half of it is about the risk, which is proportionate: every other mabo-ctl
// command runs the commands in mabo-ctl.yaml because the user typed something,
// and this one runs them because an HTTP request arrived.
const serveLong = `Serve opens the web console: one page listing every declared service with its
phase, pid, port, health probe, the exact command mabo-ctl runs for it, the
directory it runs in, and a live log stream, plus start, stop and restart
buttons.

The console keeps serving until it is interrupted. Ctrl-C stops the console; it
does not stop the services, which were spawned with setsid and outlive mabo-ctl.

SECURITY: three of this server's routes start, stop and restart the commands in
mabo-ctl.yaml, so anything that can reach it and satisfy its checks runs those
commands as you. It is guarded on four sides:

  * The default bind address is 127.0.0.1, reachable only from this machine.
  * A random session token is generated per run, printed once in the URL below,
    and required as a header on every start, stop and restart. Treat the URL as
    a password: whoever has it can run your dev stack.
  * The Host and Origin headers must name the address mabo-ctl bound, so a page on
    the web cannot reach the console by pointing its own domain at 127.0.0.1.
    --allow-origin adds an origin to that check, which is what a console reached
    through a tunnel or a port forward needs: the browser is then on another
    hostname and every button would be refused while the page itself worked.
    It takes a host (https://dev.tunnel.example) or a subdomain pattern
    (https://*.tunnel.example); plaintext http is refused for anything but a
    loopback host, and the bare "*" needs --i-know-this-is-dangerous. The list
    is also editable while mabo-ctl runs, in the console's Configuration panel.
  * Mutations are POST only and take the token from the HEADER alone. The page
    and the read routes also accept it from the ?token= in the printed URL or
    from a SameSite=Strict cookie, because a browser navigation cannot send a
    header and an EventSource cannot either; a browser without it gets a box to
    paste the token into, never the console and never the token.
  * Only the environment DECLARED in mabo-ctl.yaml is rendered, with
    credential-shaped values redacted. The inherited environment mabo-ctl forwards
    to children is never sent to the browser.

--i-know-this-is-dangerous is the only way to bind a non-loopback address, and
it means what it says: every machine that can route to that address can drive
your dev stack once it has the token. Without it, a non-loopback --addr is a
usage error and mabo-ctl exits 2 without binding anything.

With no --addr, the console binds 127.0.0.1:7999; if that port is already taken
by another server, it falls back to a kernel-chosen free port and prints the
real address. An address you type explicitly is honoured literally — a taken
explicit port is an error, never a silent move.`

// serveCmd builds `mabo-ctl serve`.
func (a *app) serveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "serve",
		Short:         "Serve the web console over a loopback HTTP listener",
		Long:          serveLong,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          a.runServe,
	}
	f := cmd.Flags()
	f.String("addr", web.DefaultAddr, "address to bind as host:port; a non-loopback host requires --i-know-this-is-dangerous")
	f.Bool("open", false, "open the console in the default browser once the socket is bound")
	f.Bool("i-know-this-is-dangerous", false,
		"permit a non-loopback bind, exposing start/stop/restart to every machine that can route to it")
	f.Bool("notify", false,
		"show a desktop notification when a service dies while this console is served")
	f.StringArray("allow-origin", nil,
		"additional browser origin to accept, e.g. https://dev.tunnel.example; repeatable. "+
			"Needed when the console is reached through a tunnel or port forward, where the browser's "+
			"origin is not the address mabo-ctl bound. Editable later in the console itself")
	return cmd
}

// serveOptions is `mabo-ctl serve`'s command line, parsed.
type serveOptions struct {
	// Addr is the host:port to bind.
	Addr string
	// AddrSet records whether the user passed --addr at all. It distinguishes
	// the implicit default from the same value typed out, because serve falls
	// back to a free port only for the former: moving a port a user explicitly
	// asked for would be surprising.
	AddrSet bool
	// Open asks for the platform browser opener once the socket is bound.
	Open bool
	// Force is --i-know-this-is-dangerous: permission to bind a non-loopback
	// address.
	Force bool
	// AllowOrigins is --allow-origin: extra browser origins to accept.
	AllowOrigins []string
	// Notify is --notify: fire a desktop notification when a service dies
	// while this console is being served.
	Notify bool
}

// runServe parses the flags and serves until the first SIGINT.
func (a *app) runServe(cmd *cobra.Command, _ []string) error {
	addr, err := cmd.Flags().GetString("addr")
	if err != nil {
		return usageError(err)
	}
	allow, err := cmd.Flags().GetStringArray("allow-origin")
	if err != nil {
		return usageError(err)
	}

	// Precedence for the console address: an explicit --addr wins; otherwise
	// the config's console_addr decides; otherwise the built-in default.
	// The returned set flag tracks whether ANY source chose a specific
	// address, because serve falls back to a free port only when the default
	// was chosen for the user — never when they asked for a particular port.
	addr, addrSet := a.consoleAddr(addr, cmd.Flags().Changed("addr"))

	opt := serveOptions{
		Addr:         addr,
		AddrSet:      addrSet,
		Open:         boolFlag(cmd, "open"),
		Force:        boolFlag(cmd, "i-know-this-is-dangerous"),
		AllowOrigins: allow,
		Notify:       boolFlag(cmd, "notify"),
	}

	ctx, cancel := interruptible(cmd.Context())
	defer cancel()
	return a.serve(ctx, opt)
}

// consoleAddr resolves the console bind address and whether it was explicitly
// chosen. Precedence: an explicit --addr wins; otherwise the config's
// console_addr decides; otherwise the built-in default.
//
// set is true when a specific address was chosen — by flag or by config — and
// false when the default was selected for the user. serve falls back to a free
// port only when set is false: a port the user asked for, in a file or on the
// command line, is honoured literally and fails loudly instead of silently
// moving. The config is read through [app.config], which has already validated
// console_addr by the time it returns, so an unparseable value cannot reach a
// listener here.
func (a *app) consoleAddr(flagAddr string, flagSet bool) (addr string, set bool) {
	if flagSet {
		return flagAddr, true
	}
	if cfg, err := a.config(); err == nil && cfg.ConsoleAddr != "" {
		return cfg.ConsoleAddr, true
	}
	return flagAddr, false
}

// serve resolves the stack, binds the socket, prints the URL and serves until
// ctx is done.
//
// The order is the point. The listener is bound BEFORE the URL is printed, so
// the printed URL is the address a browser can actually open rather than the
// address that was requested — the two differ whenever the port was 0 or the
// requested port was already taken. Nothing is printed at all when the bind
// fails, which is the only honest thing to print.
func (a *app) serve(ctx context.Context, opt serveOptions) error {
	if _, _, err := net.SplitHostPort(opt.Addr); err != nil {
		return usageErrorf("--addr %q is not an address of the form host:port: %v", opt.Addr, err)
	}

	insts, err := a.resolve()
	if err != nil {
		return err
	}
	sup, err := a.realSupervisor()
	if err != nil {
		return err
	}
	// The console starts services, so the ports it will use are persisted the
	// same way `mabo-ctl start` persists them: a status printed in another
	// terminal has to agree with what the console is supervising.
	if err := service.Persist(a.st, insts); err != nil {
		return err
	}

	// The console's config panel answers the same question `mabo-ctl config`
	// does, from the same ui.ConfigView — so it is handed the same three inputs
	// that command builds one from. None of them is derivable inside
	// internal/web: the precedence chain ran here over the --ports flag and the
	// captured <NAME>_PORT variables, internal/state is the only package that
	// may say where `.dev` lives, and only the flag parser knows whether the
	// config was given with --config or found by walking up.
	makeConsole := func(addr string) (*web.Server, error) {
		return web.New(sup, web.Options{
			Addr:           addr,
			Force:          opt.Force,
			Open:           opt.Open,
			Origins:        a.origins,
			StateDir:       a.stateDir(),
			ExplicitConfig: a.configPath != "",
			AllowedOrigins: opt.AllowOrigins,
		})
	}
	srv, err := makeConsole(opt.Addr)
	if err != nil {
		// A non-loopback address offered without the danger flag is a refusal to
		// expose an RCE surface, not a runtime failure: it is exit 2, and the
		// wrapped message already names the address and says what it would
		// expose. Everything else New reports — a bad port, a host that does not
		// resolve, a token that could not be generated — never bound a socket
		// either, and is a runtime failure.
		if errors.Is(err, web.ErrUnsafeAddr) {
			return usageError(err)
		}
		return err
	}
	if err := srv.Listen(); err != nil {
		// Everywhere else in mabo-ctl a held port names its holder; a bare
		// "bind: address already in use" would make serve the one command that
		// does not. When the holder is another mabo-ctl serve, the useful
		// remedy is the free-port form, not the lsof line.
		if !errors.Is(err, syscall.EADDRINUSE) {
			return err
		}
		// The user asked for no specific address, so 7999 was chosen for them
		// and some other server got there first. Rather than die on a port
		// nobody requested, keep the one soft guarantee bind order exists for —
		// the printed URL is the address actually bound — and retry on a
		// kernel-chosen free port. An explicitly requested address is honoured
		// literally and fails loudly instead, because silently moving a port a
		// user asked for would be surprising.
		if opt.Addr != web.DefaultAddr || opt.AddrSet {
			msg := fmt.Sprintf("mabo-ctl serve: %s: %v", opt.Addr, err)
			if _, portStr, perr := net.SplitHostPort(opt.Addr); perr == nil {
				if port, aerr := strconv.Atoi(portStr); aerr == nil {
					if h := supervisor.PortHolder(port); h.PID > 0 {
						msg += fmt.Sprintf(" — held by pid %d (%s)", h.PID, h.Command)
					}
				}
			}
			msg += "; start it elsewhere with --addr 127.0.0.1:0"
			return errors.New(msg)
		}
		fallback, ferr := makeConsole("127.0.0.1:0")
		if ferr != nil {
			return ferr
		}
		if ferr = fallback.Listen(); ferr != nil {
			// The fallback is port 0, so the kernel picks it: this only fails if
			// no loopback port at all is free, which is a real failure worth
			// surfacing on its own terms.
			return ferr
		}
		fmt.Fprintf(a.env.Stderr,
			"Note: %s is already in use; serving the console on %s instead.\n",
			web.DefaultAddr, fallback.Addr())
		srv = fallback
	}

	if opt.Notify {
		watcher := newNotifier(sup, sendDesktopNotification)
		go watcher.watch(ctx)
		fmt.Fprintln(a.env.Stderr, "Desktop notifications on: a dying service will announce itself.")
	}

	a.announceServe(srv, serveClosing)
	if opt.Open {
		if err := a.env.OpenURL(ctx, srv.URL()); err != nil {
			// A missing xdg-open is not a reason to refuse to serve: the URL is
			// already on stdout and the user can paste it.
			fmt.Fprintf(a.env.Stderr, "could not open a browser (%v); open the URL above yourself\n", err)
		}
	}
	return srv.ListenAndServe(ctx)
}

// serveClosing is [app.announceServe]'s last line for `mabo-ctl serve`, whose
// whole process is the console.
const serveClosing = "Ctrl-C stops the console; the services keep running."

// announceServe prints the console's URL on stdout and everything a human needs
// to read around it on stderr.
//
// The URL goes to stdout ALONE, one line, token included, so `mabo-ctl serve |
// head -1` is a usable address; the warnings go to stderr so they cannot end up
// pasted into a browser bar. It is printed once — the token does not rotate, and
// reprinting it on every request would put a credential in every log.
//
// closing is the last line: how THIS console is stopped. It is a parameter
// because the answer differs — `mabo-ctl serve` ends on Ctrl-C, and the console
// `mabo-ctl start --web-console` binds does not, because there Ctrl-C belongs to
// the prompt that owns it — and a stop instruction that does not work is worse
// than none.
func (a *app) announceServe(srv *web.Server, closing string) {
	fmt.Fprintf(a.env.Stderr,
		"mabo-ctl console listening on %s\n"+
			"The URL below carries a session token that can start, stop and restart every\n"+
			"declared service. Treat it as a password.\n", srv.Addr())
	fmt.Fprintln(a.env.Stdout, srv.URL())
	// Trusted origins are announced because they widen who may drive the
	// console, and an operator who cannot see the list cannot audit it. They
	// are printed after the URL and before the danger warning, in the order of
	// increasing alarm.
	if extra := srv.TrustedOrigins(); len(extra) > 0 {
		fmt.Fprintf(a.env.Stderr,
			"Also accepting browser requests from: %s\n"+
				"Those origins may drive this console when they have the token above.\n",
			strings.Join(extra, ", "))
	}
	if exposedToNetwork(srv.Addr()) {
		fmt.Fprintf(a.env.Stderr,
			"\nWARNING: %s is not a loopback address. Every machine that can route to it can\n"+
				"start, stop and restart the commands in mabo-ctl.yaml as you, once it has the\n"+
				"token above. This is real remote code execution, exposed on purpose by\n"+
				"--i-know-this-is-dangerous.\n\n", srv.Addr())
	}
	fmt.Fprintln(a.env.Stderr, closing)
}

// exposedToNetwork reports whether addr is reachable from another machine.
//
// It decides only how loudly to warn — [web.CheckAddr] has already made the
// security decision and refused the dangerous case unless it was authorised —
// and it is asked AFTER the bind, when the address comes from the listener and
// is therefore always a numeric IP. An address it cannot parse is treated as
// exposed, because a control that cannot tell must not stay quiet.
//
// It is NOT a substitute for [web.CheckAddr] and must never be used as the
// gate. This one does not resolve hostnames and treats an unparseable address
// as exposed, which is the right bias for a warning and the wrong one for a
// refusal; CheckAddr resolves names and fails closed on a lookup error. Two
// predicates exist because they answer two different questions, and collapsing
// them would silently weaken whichever call site kept the wrong one.
func exposedToNetwork(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	return !ip.IsLoopback()
}
