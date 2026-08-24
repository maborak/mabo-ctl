// Command mabo-ctl supervises the long-running local development processes a
// repository declares in its mabo-ctl.yaml.
//
// mabo-ctl is the outermost layer of the program: it owns argument parsing, the
// exit code, and every decision about what the user actually sees. It renders
// nothing itself — internal/ui does that — and it supervises nothing itself —
// internal/supervisor does that. What lives here is wiring, and three pieces of
// that wiring are load-bearing enough to be called out.
//
// First, [service.CaptureEnv] is called exactly once, from [run], before any
// command body executes and therefore before anything can spawn a child. The
// function reads AND UNSETS the caller's <NAME>_PORT variables; calling it late
// or twice would leave a stale BACKEND_PORT in the environment a child
// inherits, so the service would bind a port the supervisor is not probing.
//
// Second, a bare `mabo-ctl` launches the full-screen console only when stdin and
// stdout are both a terminal. Into a pipe it prints the status block and exits,
// because a TUI written to `mabo-ctl | head` or to a CI log is unreadable noise.
//
// Third, every exit is one of five documented codes, listed in the root
// command's help so that a script does not have to guess:
//
//	0  success
//	1  a runtime failure
//	2  a usage error
//	3  mabo-ctl.yaml is missing or invalid
//	4  a service failed to become ready
//
// `mabo-ctl exec` is the single deliberate exception: it forwards the child's
// exit code verbatim, which is the whole reason to run a command through it.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/console"
	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/ui"
)

// Build stamps, set with -ldflags at link time by the Makefile:
//
//	go build -ldflags '-X main.version=v1.2.3 -X main.commit=abc1234'
//
// They are plain vars rather than consts because the linker's -X can only
// rewrite a variable. The defaults are what an unstamped `go build ./...`
// produces, and saying "dev" is more honest than pretending to a version.
var (
	// version is the release the binary was built from.
	version = "dev"
	// commit is the git revision the binary was built from.
	commit = "unknown"
)

// Exit codes. They are part of mabo-ctl's interface to scripts, are documented in
// the root command's long help, and must stay stable.
const (
	// exitOK reports success.
	exitOK = 0
	// exitFailure reports a runtime failure: a process that would not stop, a
	// state directory that could not be written, a check that errored.
	exitFailure = 1
	// exitUsage reports a usage error: an unknown service, an unknown flag, a
	// wrong number of arguments.
	exitUsage = 2
	// exitConfig reports that mabo-ctl.yaml is missing, unreadable or invalid.
	exitConfig = 3
	// exitNotReady reports that a service failed to become ready inside
	// ready_timeout, or died while starting.
	exitNotReady = 4
)

// Env is every piece of the outside world cmd/mabo-ctl touches. It exists so the
// decisions that matter — is stdout a terminal, does the console run, what does
// the platform opener do — can be substituted in a test instead of being
// discovered by shelling out.
//
// The zero value is not usable; build one with [defaultEnv] and override the
// fields a test needs.
type Env struct {
	// Args are the command-line arguments after the program name.
	Args []string
	// Stdin is the standard input handed to `exec` and `shell`. When it is an
	// *os.File it is passed to the child directly, so an interactive shell keeps
	// its terminal; anything else is copied through a pipe.
	Stdin io.Reader
	// Stdout receives every normal result: status blocks, log lines, JSON.
	Stdout io.Writer
	// Stderr receives diagnostics: errors, usage, and the port-override notice.
	Stderr io.Writer
	// Wd is the directory config discovery walks up from. Empty means the
	// process working directory.
	Wd string
	// IsTTY reports whether mabo-ctl is attached to a terminal on both stdin and
	// stdout, which is the condition for launching the console.
	IsTTY func() bool
	// Renderer overrides the renderer. Nil means one built from Stdout.
	Renderer *ui.Renderer
	// RunConsole runs the interactive console. Nil means console.Run.
	RunConsole func(sup *supervisor.Supervisor) error
	// OpenURL hands one URL to the platform browser opener. Nil means openURL.
	OpenURL func(ctx context.Context, rawURL string) error
	// NewSupervisor wraps the real supervisor in the interface the one-shot
	// commands drive. Nil means the identity. It is a test seam: the console
	// always receives the real *supervisor.Supervisor regardless.
	NewSupervisor func(sup *supervisor.Supervisor) lifecycle
}

// defaultEnv returns the Env of the real process.
func defaultEnv() *Env {
	return &Env{
		Args:       os.Args[1:],
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		IsTTY:      func() bool { return isCharDevice(os.Stdin) && isCharDevice(os.Stdout) },
		RunConsole: console.Run,
		OpenURL:    openURL,
	}
}

// isCharDevice reports whether f is a character device, i.e. a terminal rather
// than a pipe, a regular file or a closed handle.
func isCharDevice(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func main() {
	os.Exit(run(defaultEnv()))
}

// run executes mabo-ctl against e and returns the process exit code. It never
// panics on a nil field of e that [defaultEnv] would have filled; missing
// callbacks fall back to their production implementations.
//
// The caller's <NAME>_PORT variables are captured here, before cobra dispatches
// to any command body, because capture removes them from the environment every
// child would otherwise inherit.
func run(e *Env) int {
	normalize(e)
	a := newApp(e)
	a.bootstrap()

	root := a.rootCmd()
	cmd, err := root.ExecuteC()
	if err == nil {
		return exitOK
	}

	code := exitCodeFor(err)
	fmt.Fprintln(e.Stderr, a.renderer().Error(err))
	if code == exitUsage && cmd != nil {
		fmt.Fprint(e.Stderr, cmd.UsageString())
	}
	return code
}

// normalize fills in the fields of e a caller left nil, so a partially
// constructed Env — which is what a test writes — behaves like the real one
// everywhere it does not care.
func normalize(e *Env) {
	if e.Args == nil {
		// cobra falls back to os.Args[1:] for a nil argument slice, which would
		// make a caller that meant "no arguments" inherit the real command line.
		e.Args = []string{}
	}
	if e.Stdout == nil {
		e.Stdout = io.Discard
	}
	if e.Stderr == nil {
		e.Stderr = io.Discard
	}
	if e.Stdin == nil {
		e.Stdin = strings.NewReader("")
	}
	if e.IsTTY == nil {
		e.IsTTY = func() bool { return false }
	}
	if e.RunConsole == nil {
		e.RunConsole = console.Run
	}
	if e.OpenURL == nil {
		e.OpenURL = openURL
	}
	if e.NewSupervisor == nil {
		e.NewSupervisor = func(sup *supervisor.Supervisor) lifecycle { return sup }
	}
}

// exitError carries the exit code mabo-ctl should end with alongside the error
// that caused it. Wrapping rather than replacing keeps errors.Is and errors.As
// working on whatever the underlying package returned.
type exitError struct {
	code int
	err  error
}

// Error returns the wrapped error's message; the code is not printed, it is
// returned to the shell.
func (e *exitError) Error() string { return e.err.Error() }

// Unwrap returns the underlying error so errors.Is and errors.As see through
// the exit code.
func (e *exitError) Unwrap() error { return e.err }

// ExitCode returns the process exit code this error maps to.
func (e *exitError) ExitCode() int { return e.code }

// withCode tags err with an explicit exit code. A nil err returns nil, so it is
// safe to apply to a result that may have succeeded.
func withCode(code int, err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: code, err: err}
}

// usageError tags err as a usage error, exit code 2. The caller's usage text is
// printed after it.
func usageError(err error) error { return withCode(exitUsage, err) }

// usageErrorf builds a usage error, exit code 2, from a format string.
func usageErrorf(format string, a ...any) error {
	return usageError(fmt.Errorf(format, a...))
}

// exitCodeFor maps an error to a process exit code.
//
// An explicit [exitError] anywhere in the chain wins. Otherwise a missing or
// invalid mabo-ctl.yaml is exit 3, and everything else is a runtime failure,
// exit 1. A nil error is exit 0.
func exitCodeFor(err error) int {
	if err == nil {
		return exitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	var ve *config.ValidationError
	if errors.As(err, &ve) {
		return exitConfig
	}
	if errors.Is(err, config.ErrNotFound) {
		return exitConfig
	}
	return exitFailure
}

// interruptible derives a context that is cancelled by the first SIGINT, and a
// stop function the caller must defer. A second SIGINT is left to the default
// handler, so an operator can always kill a wedged mabo-ctl with a second Ctrl-C.
//
// Cancelling does not stop the supervised services: they were spawned with
// Setsid and mabo-ctl is not their parent.
func interruptible(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt)
}

// peekConfig extracts the value of the global --config flag straight from the
// raw arguments.
//
// It runs before cobra parses anything, because the caller's <NAME>_PORT
// variables must be captured before any command body runs, and knowing WHICH
// services to capture means loading the config first. Scanning stops at a bare
// "--", after which everything belongs to a child command line. When the peek
// is wrong — a stray "--config" in an `exec` argument list, say — the root
// command's PersistentPreRunE notices the disagreement and reloads; see
// [app.reconcileConfig].
//
// It returns "" when no --config appears.
func peekConfig(args []string) string {
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "--":
			return ""
		case "--config":
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		default:
			if v, ok := strings.CutPrefix(arg, "--config="); ok {
				return v
			}
		}
	}
	return ""
}

// joinAnd renders a list as "a", "a and b", or "a, b and c".
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}
