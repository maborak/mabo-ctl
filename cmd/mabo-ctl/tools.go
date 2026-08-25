package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/ui"
)

// checkTimeout bounds one preflight check, TCP dial or command alike, so a
// firewalled host cannot hang the whole command.
const checkTimeout = 5 * time.Second

// preflightCmd builds `mabo-ctl preflight`.
func (a *app) preflightCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "preflight",
		Short: "Run the checks declared in mabo-ctl.yaml",
		Long: `Preflight runs the checks: block of mabo-ctl.yaml, in parallel.

Each check sets exactly one of:

  tcp:      host:port — the check passes when the address accepts a connection
  command:  argv      — the check passes when the command exits 0

mabo-ctl knows nothing about what is being checked. A database, a cache, a message
broker: they belong to the supervised application, are declared in its
mabo-ctl.yaml, and mabo-ctl only dials or runs what that file says.

Exit code 1 means at least one check failed.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          a.runPreflight,
	}
}

// checkResult is one finished preflight check.
type checkResult struct {
	name    string
	ok      bool
	detail  string
	elapsed time.Duration
}

// runPreflight runs every declared check concurrently and reports them in
// declaration order.
//
// It starts one goroutine per check, each bounded by checkTimeout and by ctx,
// and returns only after all of them have finished.
func (a *app) runPreflight(cmd *cobra.Command, _ []string) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}
	if len(cfg.Checks) == 0 {
		fmt.Fprintf(a.env.Stderr, "%s declares no checks:\n", cfg.Path)
		return nil
	}

	ctx, cancel := interruptible(cmd.Context())
	defer cancel()

	results := make([]checkResult, len(cfg.Checks))
	var wg sync.WaitGroup
	for i, ck := range cfg.Checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = runCheck(ctx, cfg, ck)
		}()
	}
	wg.Wait()

	fmt.Fprintln(a.env.Stdout, renderChecks(results))

	var failed []string
	for _, r := range results {
		if !r.ok {
			failed = append(failed, r.name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("preflight: %s failed", joinAnd(failed))
	}
	return nil
}

// runCheck performs one check. A TCP check dials host:port; a command check
// runs argv in the project root with the caller's environment and treats exit 0
// as a pass. Its combined output is reported on failure, trimmed to one line so
// the block stays scannable.
func runCheck(ctx context.Context, cfg *config.Config, ck config.Check) checkResult {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	start := time.Now()

	switch {
	case ck.TCP != "":
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", ck.TCP)
		if err != nil {
			return checkResult{name: ck.Name, detail: err.Error(), elapsed: time.Since(start)}
		}
		res := checkResult{name: ck.Name, ok: true, detail: ck.TCP + " accepted a connection", elapsed: time.Since(start)}
		if err := conn.Close(); err != nil {
			res.detail += fmt.Sprintf(" (close failed: %v)", err)
		}
		return res

	case len(ck.Command) > 0:
		// #nosec G204 -- running declared commands is mabo-ctl's whole purpose; the
		// trust boundary is "whoever can write mabo-ctl.yaml can run code as you",
		// and argv is passed as a list so there is no shell to inject into.
		c := exec.CommandContext(ctx, ck.Command[0], ck.Command[1:]...)
		c.Dir = cfg.Root
		out, err := c.CombinedOutput()
		if err != nil {
			return checkResult{
				name:    ck.Name,
				detail:  strings.TrimSpace(err.Error() + ": " + firstLine(string(out))),
				elapsed: time.Since(start),
			}
		}
		detail := firstLine(string(out))
		if detail == "" {
			// A silent success still deserves a cell; an empty DETAIL reads as
			// "nothing happened" rather than "the command exited 0".
			detail = strings.Join(ck.Command, " ") + " exited 0"
		}
		return checkResult{name: ck.Name, ok: true, detail: detail, elapsed: time.Since(start)}

	default:
		return checkResult{
			name:    ck.Name,
			detail:  "check declares neither tcp: nor command:",
			elapsed: time.Since(start),
		}
	}
}

// renderChecks lays the results out as an aligned block. It uses the same
// glyph-and-word pair as the status block, so "passed" and "failed" are
// distinguishable with colour off, which is what NO_COLOR and a pipe give you.
func renderChecks(results []checkResult) string {
	okGlyph, _ := ui.PhaseLabel(supervisor.PhaseReady)
	badGlyph, _ := ui.PhaseLabel(supervisor.PhaseFailed)

	wName := len("CHECK")
	for _, r := range results {
		if n := len(r.name); n > wName {
			wName = n
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%-*s  %-8s  %s", wName, "CHECK", "RESULT", "DETAIL")
	for _, r := range results {
		glyph, word := badGlyph, "failed"
		if r.ok {
			glyph, word = okGlyph, "passed"
		}
		fmt.Fprintf(&b, "\n%-*s  %s %-6s  %s", wName, r.name, glyph, word, r.detail)
	}
	return b.String()
}

// firstLine returns s up to its first newline, with surrounding space removed.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// execCmd builds `mabo-ctl exec`.
func (a *app) execCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exec <service> <command> [args...]",
		Short: "Run a command in a service's resolved environment and directory",
		Long: `Exec runs a command with EXACTLY the environment and working directory the
named service would run with: the resolved <NAME>_PORT of every service, the
service's declared env, and the interpreter its runtime: line selects. That is
what makes "mabo-ctl exec backend pytest" correct rather than approximately
correct — it does not depend on whether your shell was a login shell.

The command is looked up on the SERVICE's PATH, not mabo-ctl's, and is run
directly: there is no shell, so no quoting rules and no word splitting. Flags
after the service name belong to the command, not to mabo-ctl.

Exec forwards the child's exit code verbatim. It is the one mabo-ctl command whose
exit code is not from mabo-ctl's own table.`,
		Args:               argsAtLeast(2, "exec needs a service and a command, e.g. mabo-ctl exec backend pytest -q"),
		SilenceUsage:       true,
		SilenceErrors:      true,
		DisableFlagParsing: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			in, err := a.instance(cmd, args[0])
			if err != nil {
				return err
			}
			return a.spawn(in, stripLeadingDashDash(args[1:]), "")
		},
	}
	// Everything after the service name belongs to the child command, so mabo-ctl
	// must stop looking for its own flags at the first positional argument.
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// stripLeadingDashDash drops a leading "--" from a child command line.
//
// Cobra removes the first "--" itself, but only while it is still parsing
// flags. `exec` turns interspersed parsing off at the service name, so a "--"
// written after it — which the help text invites, and which is the habit every
// other tool teaches — arrives as a literal argument and would otherwise become
// the name of the program to run.
func stripLeadingDashDash(argv []string) []string {
	if len(argv) > 0 && argv[0] == "--" {
		return argv[1:]
	}
	return argv
}

// shellCmd builds `mabo-ctl shell`.
func (a *app) shellCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell <name>",
		Short: "Open a declared shell, or an interactive shell in a service's environment",
		Long: `Shell resolves its argument in two steps.

If it names an entry in the shells: block of mabo-ctl.yaml, that entry's command
runs. When the entry sets service:, it runs in that service's directory with that
service's resolved environment.

Otherwise, if it names a service, an interactive $SHELL opens in that service's
directory with that service's resolved environment.

A declared shell wins over a service of the same name, because a declared shell
is an explicit statement of intent. mabo-ctl has no built-in knowledge of psql,
redis-cli or any other tool: everything it can open is declared in mabo-ctl.yaml.

Shell forwards the child's exit code verbatim.`,
		Args:          argsExactly(1, "shell needs one name, e.g. mabo-ctl shell backend"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runShell(cmd, args[0])
		},
	}
}

// runShell opens the declared shell or the service shell named n.
func (a *app) runShell(cmd *cobra.Command, n string) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}

	if sh, ok := cfg.LookupShell(n); ok {
		if len(sh.Command) == 0 {
			return fmt.Errorf("shell %q declares an empty command", n)
		}
		if sh.Service == "" {
			return a.spawn(service.Instance{
				Name: n,
				Dir:  cfg.Root,
				Env:  os.Environ(),
			}, sh.Command, fmt.Sprintf("mabo-ctl: shell %q in %s", n, cfg.Root))
		}
		in, err := a.instance(cmd, sh.Service)
		if err != nil {
			return err
		}
		return a.spawn(in, sh.Command,
			fmt.Sprintf("mabo-ctl: shell %q in the environment of service %q (%s)", n, in.Name, in.Dir))
	}

	if _, ok := cfg.Service(n); ok {
		in, err := a.instance(cmd, n)
		if err != nil {
			return err
		}
		sh := lookupEnv(in.Env, "SHELL")
		if sh == "" {
			sh = "/bin/sh"
		}
		banner := fmt.Sprintf("mabo-ctl: %s in the environment of service %q (%s)", sh, in.Name, in.Dir)
		if in.Port > 0 {
			banner += fmt.Sprintf("; port %d", in.Port)
		}
		return a.spawn(in, []string{sh}, banner)
	}

	shells := make([]string, 0, len(cfg.Shells))
	for _, sh := range cfg.Shells {
		shells = append(shells, sh.Name)
	}
	return usageErrorf("unknown shell %q; declared shells are: %s; declared services are: %s",
		n, checkNames(shells), checkNames(cfg.Names()))
}

// spawn runs argv in the foreground with in's directory and environment,
// wired to mabo-ctl's own stdin, stdout and stderr, and forwards the child's exit
// code as the process exit code.
//
// argv[0] is resolved against the CHILD's PATH — the one in in.Env, which the
// service's runtime: line may have prepended to — never against mabo-ctl's own.
// Resolving ambiently is the predecessor bug where a non-login shell had no nvm
// on PATH and the wrong interpreter ran.
//
// SIGINT is ignored by mabo-ctl for the duration, so a Ctrl-C typed at the
// terminal reaches the child, which is what makes an interactive shell usable.
// banner, when non-empty, is printed to stderr before the child starts.
func (a *app) spawn(in service.Instance, argv []string, banner string) error {
	if len(argv) == 0 {
		return usageErrorf("no command given")
	}
	path, err := lookPath(argv[0], in.Dir, lookupEnv(in.Env, "PATH"))
	if err != nil {
		return fmt.Errorf("service %q: %w", in.Name, err)
	}

	// #nosec G204 -- running a command the user typed, in a service environment
	// declared in mabo-ctl.yaml, is exactly this command's purpose. argv is a list,
	// so there is no shell and nothing to quote.
	c := exec.Command(path, argv[1:]...)
	c.Dir = in.Dir
	c.Env = in.Env
	c.Stdin = a.stdinForChild()
	c.Stdout = a.env.Stdout
	c.Stderr = a.env.Stderr

	if banner != "" {
		fmt.Fprintln(a.env.Stderr, banner)
	}

	// The child shares mabo-ctl's process group, so the terminal delivers Ctrl-C
	// to both. mabo-ctl must not act on it while the child is in the foreground —
	// the child decides what Ctrl-C means.
	//
	// This deliberately does NOT use signal.Ignore/signal.Reset. Both are
	// process-global: Reset removes EVERY channel registered for the signal, so
	// running `exec` once inside the interactive console tore down the session's
	// own long-lived SIGINT registration and the next Ctrl-C killed the console
	// instead of cancelling a line. Draining into a throwaway channel gets the
	// same "mabo-ctl ignores it" behaviour with a lifetime scoped to this call,
	// and leaves any other registration intact.
	swallow := make(chan os.Signal, 1)
	signal.Notify(swallow, os.Interrupt)
	go func() {
		for range swallow { //nolint:revive // drained on purpose; the child owns Ctrl-C
		}
	}()
	// Stop BEFORE close, in one defer. Two defers would run LIFO and close the
	// channel while the signal package could still send on it, which panics.
	defer func() {
		signal.Stop(swallow)
		close(swallow)
	}()

	runErr := c.Run()
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		if code := ee.ExitCode(); code > 0 {
			return withCode(code, fmt.Errorf("%s exited with status %d", argv[0], code))
		}
		// A negative ExitCode means the child was signalled; report it as a
		// runtime failure rather than as success.
		return fmt.Errorf("%s was terminated by a signal: %w", argv[0], runErr)
	}
	if runErr != nil {
		return fmt.Errorf("run %s in %s: %w", path, in.Dir, runErr)
	}
	return nil
}

// stdinForChild returns the standard input to hand a child. A real *os.File is
// passed straight through so an interactive shell keeps its terminal; anything
// else is handed over as a plain reader, which Go copies through a pipe.
func (a *app) stdinForChild() io.Reader {
	if f, ok := a.env.Stdin.(*os.File); ok {
		return f
	}
	return a.env.Stdin
}

// lookupEnv returns the value of key in a "KEY=VALUE" slice, or "".
func lookupEnv(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, prefix); ok {
			return v
		}
	}
	return ""
}

// lookPath resolves name to an executable using pathList, the PATH the CHILD
// will run with, and dir, the directory it will run in.
//
// A name containing a path separator is taken as a path, relative to dir when it
// is not absolute. Anything else is searched along pathList in order. The error
// names the PATH that was searched, so a missing interpreter says where mabo-ctl
// looked rather than just that it failed.
func lookPath(name, dir, pathList string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("empty command")
	}
	if strings.ContainsRune(name, os.PathSeparator) {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if err := executableFile(p); err != nil {
			return "", fmt.Errorf("%q: %w", name, err)
		}
		return p, nil
	}
	for _, d := range filepath.SplitList(pathList) {
		if d == "" {
			d = "."
		}
		p := filepath.Join(d, name)
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if executableFile(p) == nil {
			return p, nil
		}
	}
	where := pathList
	if where == "" {
		where = "(the service's PATH is empty)"
	}
	return "", fmt.Errorf("%q was not found on the service's PATH: %s", name, where)
}

// executableFile reports whether path is an existing, non-directory file with
// at least one execute bit.
func executableFile(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	if fi.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", path)
	}
	return nil
}

// openURL hands rawURL to the platform's browser opener as a separate argument.
//
// macOS uses open(1) and Linux uses xdg-open(1); every other platform is
// unsupported and says so. The URL is never interpolated into a shell command
// line, and its scheme is checked before the opener is invoked, because the
// value comes from mabo-ctl.yaml and the desktop will launch a handler for any
// scheme it recognises.
//
// It returns once the opener has been launched; the browser itself keeps
// running independently.
func openURL(ctx context.Context, rawURL string) error {
	if _, err := parseHTTPURL(rawURL); err != nil {
		return err
	}
	var opener string
	switch runtime.GOOS {
	case "darwin":
		opener = "open"
	case "linux":
		opener = "xdg-open"
	default:
		return fmt.Errorf("mabo-ctl open: unsupported platform %q; open %s yourself", runtime.GOOS, rawURL)
	}
	path, err := exec.LookPath(opener)
	if err != nil {
		return fmt.Errorf("mabo-ctl open: %s is not on PATH: %w", opener, err)
	}
	// The URL is a separate argv element, never a shell word.
	c := exec.CommandContext(ctx, path, rawURL)
	if err := c.Run(); err != nil {
		return fmt.Errorf("mabo-ctl open: %s %s: %w", opener, rawURL, err)
	}
	return nil
}

// parseHTTPURL parses rawURL and requires an absolute http or https URL with a
// host. Anything else is refused rather than handed to a URL handler.
func parseHTTPURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%q is not a URL: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%q has scheme %q; only http and https may be opened", rawURL, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%q has no host", rawURL)
	}
	return u, nil
}

// completionCmd builds `mabo-ctl completion`.
func (a *app) completionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "completion <bash|zsh|fish|powershell>",
		Short: "Print a shell completion script",
		Long: `Completion writes a completion script to stdout.

  bash:        source <(mabo-ctl completion bash)
  zsh:         mabo-ctl completion zsh > "${fpath[1]}/_mabo-ctl"
  fish:        mabo-ctl completion fish > ~/.config/fish/completions/mabo-ctl.fish
  powershell:  mabo-ctl completion powershell >> $PROFILE

The completion scripts cover every shell cobra generates for; the mabo-ctl
BINARY itself still runs on macOS and Linux only.`,
		Args:          argsOneOf("bash", "zsh", "fish", "powershell"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletionV2(a.env.Stdout, true)
			case "zsh":
				return cmd.Root().GenZshCompletion(a.env.Stdout)
			case "fish":
				return cmd.Root().GenFishCompletion(a.env.Stdout, true)
			case "powershell":
				return cmd.Root().GenPowerShellCompletion(a.env.Stdout)
			default:
				return usageErrorf("unsupported shell %q; use bash, zsh, fish or powershell", args[0])
			}
		},
	}
}

// argsExactly requires exactly n positional arguments, reporting a usage error
// with hint when the count is wrong.
func argsExactly(n int, hint string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != n {
			return usageErrorf("expected %d argument(s), got %d; %s", n, len(args), hint)
		}
		return nil
	}
}

// argsAtLeast requires at least n positional arguments, reporting a usage error
// with hint when there are too few.
func argsAtLeast(n int, hint string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) < n {
			return usageErrorf("expected at least %d argument(s), got %d; %s", n, len(args), hint)
		}
		return nil
	}
}

// argsOneOf requires exactly one positional argument drawn from valid.
func argsOneOf(valid ...string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 1 {
			return usageErrorf("expected one of %s", strings.Join(valid, ", "))
		}
		for _, v := range valid {
			if args[0] == v {
				return nil
			}
		}
		return usageErrorf("%q is not supported; expected one of %s", args[0], strings.Join(valid, ", "))
	}
}
