package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maborak/mabo-ctl/internal/repl"
	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// newREPLApp returns a bootstrapped app over the standard fixture, with the
// fake supervisor wired in, plus the dispatcher the console would use.
func newREPLApp(t *testing.T) (*app, *fakeSup, *cobraDispatcher) {
	t.Helper()
	h := newHarness(t)
	a := newApp(h.env)
	a.bootstrap()
	return a, h.sup, &cobraDispatcher{a: a}
}

// TestREPLDispatchReachesTheRealCommandTree is the requirement that a console
// line is executed by the SAME tree the CLI uses, with no per-verb code.
func TestREPLDispatchReachesTheRealCommandTree(t *testing.T) {
	t.Parallel()
	a, sup, d := newREPLApp(t)

	for _, argv := range [][]string{{"start", "alpha"}, {"stop", "beta"}, {"restart", "delta"}} {
		if err := d.Dispatch(context.Background(), argv); err != nil {
			t.Fatalf("Dispatch(%v): %v", argv, err)
		}
	}
	if got, want := sup.startedNames(), [][]string{{"alpha"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("started %#v, want %#v", got, want)
	}
	sup.mu.Lock()
	stopped, restarted := sup.stopped, sup.restart
	sup.mu.Unlock()
	if want := [][]string{{"beta"}}; !reflect.DeepEqual(stopped, want) {
		t.Fatalf("stopped %#v, want %#v", stopped, want)
	}
	if want := [][]string{{"delta"}}; !reflect.DeepEqual(restarted, want) {
		t.Fatalf("restarted %#v, want %#v", restarted, want)
	}
	if a.inREPL {
		t.Fatal("dispatching must not set the nesting guard")
	}
}

// TestREPLFlagStateDoesNotLeakBetweenLines is the first of the two
// re-entrancy hazards: pflag values persist on a *cobra.Command, so a tree
// reused across lines carries `--all` out of one start and into the next. The
// user types `start alpha` and mabo-ctl starts everything.
func TestREPLFlagStateDoesNotLeakBetweenLines(t *testing.T) {
	t.Parallel()
	a, sup, d := newREPLApp(t)

	if err := d.Dispatch(context.Background(), []string{"start", "--all"}); err != nil {
		t.Fatalf("Dispatch(start --all): %v", err)
	}
	// A leaked --all makes this either "start everything" (nil selection) or a
	// usage error, because --all and a service name cannot be combined.
	if err := d.Dispatch(context.Background(), []string{"start", "alpha"}); err != nil {
		t.Fatalf("Dispatch(start alpha) after start --all: %v", err)
	}

	// --all selects every service by name; the point of this test is that the
	// SECOND line is unaffected by the first, not how "all" is spelled.
	want := [][]string{{"alpha", "beta", "delta", "gamma"}, {"alpha"}}
	if got := sup.startedNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("started %#v, want %#v — --all leaked into the next line", got, want)
	}

	// The same property, asserted on the tree rather than through its effect.
	root := a.rootCmd()
	for _, name := range []string{"all", "follow", "ports"} {
		f := root.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("root has no --%s", name)
		}
		if f.Changed {
			t.Fatalf("--%s is still marked Changed on a freshly built tree", name)
		}
	}
	if a.ports.raw != "" {
		t.Fatalf("a.ports survived the line that set it: %q", a.ports.raw)
	}
}

// TestREPLPortsFlagDoesNotLeak covers --ports specifically, which lives on the
// app rather than on the command and therefore cannot be reset by rebuilding
// the tree.
func TestREPLPortsFlagDoesNotLeak(t *testing.T) {
	t.Parallel()
	a, _, d := newREPLApp(t)

	if err := d.Dispatch(context.Background(), []string{"start", "--ports=7999", "alpha"}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if err := d.Dispatch(context.Background(), []string{"status"}); err != nil {
		t.Fatalf("Dispatch(status): %v", err)
	}
	if len(a.ports.values) != 0 || a.ports.raw != "" {
		t.Fatalf("--ports survived into the next line: %+v", a.ports)
	}
}

// TestREPLDispatchReturnsErrorsRatherThanExiting is the second re-entrancy
// hazard. A typo must come back as a value; nothing on this path may end the
// process.
func TestREPLDispatchReturnsErrorsRatherThanExiting(t *testing.T) {
	t.Parallel()
	_, sup, d := newREPLApp(t)

	err := d.Dispatch(context.Background(), []string{"nosuchthing"})
	if err == nil {
		t.Fatal("an unknown word must return an error")
	}
	if !strings.Contains(err.Error(), "declared services are: alpha, beta, delta, gamma") {
		t.Fatalf("the error must name the valid services; got %q", err)
	}
	if got := exitCodeFor(err); got != exitUsage {
		t.Fatalf("exit code %d, want %d", got, exitUsage)
	}

	if err := d.Dispatch(context.Background(), []string{"start", "--nosuchflag"}); err == nil {
		t.Fatal("an unknown flag must return an error")
	}
	// The tree is still usable afterwards, which is the whole point.
	if err := d.Dispatch(context.Background(), []string{"start", "alpha"}); err != nil {
		t.Fatalf("Dispatch after two failures: %v", err)
	}
	if got, want := sup.startedNames(), [][]string{{"alpha"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("started %#v, want %#v", got, want)
	}
}

// TestREPLCommandsExcludeRepl keeps the console from advertising the one
// command it refuses to run.
func TestREPLCommandsExcludeRepl(t *testing.T) {
	t.Parallel()
	_, _, d := newREPLApp(t)

	var names []string
	for _, c := range d.Commands() {
		names = append(names, c.Name)
		if c.Short == "" {
			t.Fatalf("command %q has no description for help", c.Name)
		}
	}
	for _, want := range []string{"start", "stop", "status", "logs", "exec", "shell", "health", "preflight", "open", "reset", "config"} {
		if !slicesContains(names, want) {
			t.Fatalf("the console must dispatch %q; got %v", want, names)
		}
	}
	if slicesContains(names, "repl") {
		t.Fatalf("repl must not be advertised inside a repl; got %v", names)
	}
}

// slicesContains reports whether s holds v.
func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestREPLSessionRunsAndQuits drives `mabo-ctl repl` end to end through the same
// entry point the shell uses.
func TestREPLSessionRunsAndQuits(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "repl")
	h.env.IsTTY = func() bool { return true }
	h.env.Stdin = strings.NewReader("status\nstart alpha\nquit\n")

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code %d, want %d; stderr:\n%s", code, exitOK, h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, "mabo-ctl(") {
		t.Fatalf("want a prompt naming the repo; got:\n%s", out)
	}
	if got, want := h.sup.startedNames(), [][]string{{"alpha"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("started %#v, want %#v", got, want)
	}
}

// TestREPLQuitDoesNotStopServices pins the promise the exit line makes.
func TestREPLQuitDoesNotStopServices(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "repl")
	h.env.IsTTY = func() bool { return true }
	h.env.Stdin = strings.NewReader("start alpha\nquit\n")

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code %d, want %d", code, exitOK)
	}
	h.sup.mu.Lock()
	stopped, resets := h.sup.stopped, h.sup.resets
	h.sup.mu.Unlock()
	if len(stopped) != 0 || resets != 0 {
		t.Fatalf("quitting stopped things: stopped=%v resets=%d", stopped, resets)
	}
	if !strings.Contains(h.stdout.String(), "STILL RUNNING") {
		t.Fatalf("the exit line must say the services keep running; got:\n%s", h.stdout.String())
	}
}

// TestREPLUnknownCommandDoesNotEndTheSession is the end-to-end form of the
// requirement: exit code 0, and the line after the typo still ran.
func TestREPLUnknownCommandDoesNotEndTheSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "repl")
	h.env.IsTTY = func() bool { return true }
	h.env.Stdin = strings.NewReader("nosuchthing\nstart --nosuchflag\nstart alpha\nquit\n")

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code %d, want %d; a session that ends cleanly is exit 0", code, exitOK)
	}
	out := h.stdout.String()
	if !strings.Contains(out, "declared services are") {
		t.Fatalf("want the unknown-service error; got:\n%s", out)
	}
	if !strings.Contains(out, "commands: ") {
		t.Fatalf("want the valid command set; got:\n%s", out)
	}
	if got, want := h.sup.startedNames(), [][]string{{"alpha"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("started %#v, want %#v — the session did not survive the bad lines", got, want)
	}
}

// TestREPLRefusesToNest keeps one stdin from being read by two loops.
func TestREPLRefusesToNest(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "repl")
	h.env.IsTTY = func() bool { return true }
	h.env.Stdin = strings.NewReader("repl\nstart alpha\nquit\n")

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code %d, want %d", code, exitOK)
	}
	out := h.stdout.String()
	if !strings.Contains(out, "already inside the interactive console") {
		t.Fatalf("want the nesting refusal; got:\n%s", out)
	}
	if got, want := h.sup.startedNames(), [][]string{{"alpha"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("started %#v, want %#v", got, want)
	}
}

// TestREPLWithoutConfigExitsThree keeps the missing-config failure at the shell,
// where a script sees it, instead of once per line inside a console.
func TestREPLWithoutConfigExitsThree(t *testing.T) {
	t.Parallel()
	h := newHarnessAt(t, t.TempDir(), "repl")
	// A terminal, so the run gets far enough to reach the config load: this
	// test is about WHERE the config failure surfaces (before the loop, at the
	// shell) and not about the no-terminal refusal, which has its own test.
	h.env.IsTTY = func() bool { return true }
	h.env.Stdin = strings.NewReader("quit\n")

	if code := h.run(); code != exitConfig {
		t.Fatalf("exit code %d, want %d", code, exitConfig)
	}
}

// TestREPLMonitorTranslatesPhases pins which supervisor phases the console
// treats as a death worth announcing.
func TestREPLMonitorTranslatesPhases(t *testing.T) {
	t.Parallel()
	ended := time.Now()
	started := ended.Add(-time.Hour)
	sup := &fakeSup{statuses: []supervisor.Status{
		{Name: "ready", Phase: supervisor.PhaseReady},
		{Name: "stopped", Phase: supervisor.PhaseStopped},
		{Name: "degraded", Phase: supervisor.PhaseDegraded},
		{Name: "crashed", Phase: supervisor.PhaseExited, ExitCode: 1, StartedAt: started, ExitedAt: ended},
		{Name: "neverup", Phase: supervisor.PhaseFailed, ExitSignal: "SIGKILL", ExitedAt: ended},
	}}

	got := replMonitor{lc: sup}.Status(context.Background())
	want := []repl.Status{
		{Name: "ready"},
		{Name: "stopped"},
		{Name: "degraded"},
		{Name: "crashed", Dead: true, ExitCode: 1, StartedAt: started, ExitedAt: ended},
		{Name: "neverup", Dead: true, Startup: true, ExitSignal: "SIGKILL", ExitedAt: ended},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Status() = %#v, want %#v", got, want)
	}
}

// TestNotifyInterruptsStops checks the signal registration is undone, so a
// console that has quit does not keep swallowing Ctrl-C for the rest of the
// process.
func TestNotifyInterruptsStops(t *testing.T) {
	t.Parallel()
	ch, stop := notifyInterrupts()
	if ch == nil {
		t.Fatal("notifyInterrupts returned a nil channel")
	}
	stop()
	stop() // idempotent: the caller defers it and the command may also return early
}
