package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/ui"
)

// fixture is a mabo-ctl.yaml with three ported services and one portless one.
// Every command resolves to echo, which exec.LookPath finds on any developer
// machine, so resolution succeeds without the test spawning anything.
const fixture = `
services:
  - name: alpha
    port: 7100
    cmd: [echo, alpha]
  - name: beta
    port: 7101
    cmd: [echo, beta]
  - name: delta
    port: 7102
    cmd: [echo, delta]
  - name: gamma
    cmd: [echo, gamma]
`

// fakeSup records what the CLI asked the supervisor to do and returns canned
// statuses, so a command's routing and exit code can be tested without
// spawning a process.
type fakeSup struct {
	statusCalls       int
	statusNoPortCalls int
	mu                sync.Mutex
	started           [][]string
	stopped           [][]string
	restart           [][]string
	resets            int
	resetForce        bool
	statuses          []supervisor.Status
	startErr          error
	lines             map[string][]string
}

// Start records names and replays startErr.
func (f *fakeSup) Start(_ context.Context, names []string, ev chan<- supervisor.Event) error {
	f.mu.Lock()
	f.started = append(f.started, append([]string(nil), names...))
	f.mu.Unlock()
	ev <- supervisor.Event{Service: "alpha", Phase: supervisor.PhaseReady, Msg: "started"}
	return f.startErr
}

// Stop records names.
func (f *fakeSup) Stop(_ context.Context, names []string, _ chan<- supervisor.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, append([]string(nil), names...))
	return nil
}

// Restart records names.
func (f *fakeSup) Restart(_ context.Context, names []string, _ chan<- supervisor.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restart = append(f.restart, append([]string(nil), names...))
	return nil
}

// Status returns the canned statuses.
func (f *fakeSup) Status(context.Context) []supervisor.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	return append([]supervisor.Status(nil), f.statuses...)
}

// StatusNoPorts returns the same statuses and counts separately, so a test can
// assert that a poller took the cheap path rather than forking lsof per tick.
func (f *fakeSup) StatusNoPorts(context.Context) []supervisor.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusNoPortCalls++
	return append([]supervisor.Status(nil), f.statuses...)
}

// Reset counts the call and records whether --force was passed through.
func (f *fakeSup) Reset(_ context.Context, force bool, _ chan<- supervisor.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resets++
	f.resetForce = force
	return nil
}

// Tail replays the canned lines for svc and returns.
//
// It closes out, because the real supervisor does: Tail owns the channel it is
// handed. A fake that did not close it would let a caller that wrongly closes
// the channel itself pass the tests and panic in production.
func (f *fakeSup) Tail(_ context.Context, svc string, _ int, _ bool, out chan<- string) error {
	defer close(out)
	f.mu.Lock()
	lines := append([]string(nil), f.lines[svc]...)
	f.mu.Unlock()
	for _, l := range lines {
		out <- l
	}
	return nil
}

// startedNames returns a copy of the recorded start selections.
func (f *fakeSup) startedNames() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.started...)
}

// harness is one prepared mabo-ctl invocation.
type harness struct {
	root    string
	env     *Env
	sup     *fakeSup
	stdout  *bytes.Buffer
	stderr  *bytes.Buffer
	console int
	// ctx/cancel drive blocking commands (logs -f) deterministically; cancel is
	// registered via t.Cleanup so no test can leak a resident goroutine.
	ctx    context.Context
	cancel context.CancelFunc
}

// newHarness writes fixture into a fresh temp directory and wires an Env whose
// terminal decision, console and supervisor are all injected.
func newHarness(t *testing.T, args ...string) *harness {
	t.Helper()
	return newHarnessWithConfig(t, fixture, args...)
}

// newHarnessWithConfig is newHarness with an explicit mabo-ctl.yaml body.
func newHarnessWithConfig(t *testing.T, body string, args ...string) *harness {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "mabo-ctl.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write mabo-ctl.yaml: %v", err)
	}
	return newHarnessAt(t, root, args...)
}

// newHarnessAt wires a harness rooted at an existing directory, which may or
// may not hold a mabo-ctl.yaml.
func newHarnessAt(t *testing.T, root string, args ...string) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &harness{
		root:   root,
		sup:    &fakeSup{statuses: readyStatuses()},
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
		ctx:    ctx,
		cancel: cancel,
	}
	h.env = &Env{
		Args:     args,
		Ctx:      ctx,
		Stdout:   h.stdout,
		Stderr:   h.stderr,
		Wd:       root,
		IsTTY:    func() bool { return false },
		Renderer: &ui.Renderer{}, // plain: no colour, no width limit
		RunConsole: func(sup *supervisor.Supervisor) error {
			if sup == nil {
				return errors.New("console got a nil supervisor")
			}
			h.console++
			return nil
		},
		OpenURL:       func(context.Context, string) error { return nil },
		NewSupervisor: func(*supervisor.Supervisor) lifecycle { return h.sup },
	}
	return h
}

// run executes mabo-ctl and returns the exit code.
func (h *harness) run() int { return run(h.env) }

// readyStatuses is the canned "everything is up" report.
func readyStatuses() []supervisor.Status {
	return []supervisor.Status{
		{Name: "alpha", Phase: supervisor.PhaseReady, PID: 11, Port: 7100},
		{Name: "beta", Phase: supervisor.PhaseReady, PID: 12, Port: 7101},
		{Name: "delta", Phase: supervisor.PhaseReady, PID: 13, Port: 7102},
		{Name: "gamma", Phase: supervisor.PhaseRunning, PID: 14},
	}
}

func TestParsePorts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    []int
		wantErr string
	}{
		{name: "empty", in: "", want: nil},
		{name: "blank", in: "   ", want: nil},
		{name: "all values", in: "7100,7101,7102", want: []int{7100, 7101, 7102}},
		{name: "leading empty slots keep defaults", in: ",,7999", want: []int{0, 0, 7999}},
		{name: "interior empty slot", in: "7100,,7102", want: []int{7100, 0, 7102}},
		{name: "trailing empty slots are dropped", in: ",,7999,", want: []int{0, 0, 7999}},
		{name: "only empty slots is a no-op", in: ",,,", want: nil},
		{name: "zero means keep the default", in: "0,7101", want: []int{0, 7101}},
		{name: "dash means keep the default", in: "-,7101", want: []int{0, 7101}},
		{name: "spaces are trimmed", in: " 7100 , , 7102 ", want: []int{7100, 0, 7102}},
		{name: "not a number", in: "7100,abc", wantErr: "slot 2"},
		{name: "out of range high", in: "70000", wantErr: "out of range"},
		{name: "out of range negative", in: "-1", wantErr: "out of range"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parsePorts(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parsePorts(%q) = %v, want an error containing %q", tc.in, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parsePorts(%q) error = %q, want it to contain %q", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePorts(%q): unexpected error %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parsePorts(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestExitCodeFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "nil is success", err: nil, want: exitOK},
		{name: "plain error is a runtime failure", err: errors.New("boom"), want: exitFailure},
		{name: "usage error", err: usageErrorf("bad flag"), want: exitUsage},
		{name: "wrapped usage error", err: fmt.Errorf("outer: %w", usageErrorf("bad flag")), want: exitUsage},
		{name: "missing config", err: fmt.Errorf("look: %w", config.ErrNotFound), want: exitConfig},
		{name: "invalid config", err: &config.ValidationError{Path: "mabo-ctl.yaml", Problems: []string{"x"}}, want: exitConfig},
		{name: "wrapped invalid config", err: fmt.Errorf("load: %w", &config.ValidationError{Problems: []string{"x"}}), want: exitConfig},
		{name: "not ready", err: withCode(exitNotReady, errors.New("slow")), want: exitNotReady},
		{name: "forwarded child code", err: withCode(7, errors.New("exit 7")), want: 7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := exitCodeFor(tc.err); got != tc.want {
				t.Fatalf("exitCodeFor(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestPeekConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "absent", args: []string{"status"}, want: ""},
		{name: "separate value", args: []string{"--config", "/tmp/a.yaml", "status"}, want: "/tmp/a.yaml"},
		{name: "joined value", args: []string{"--config=/tmp/b.yaml"}, want: "/tmp/b.yaml"},
		{name: "after a subcommand", args: []string{"status", "--config=/tmp/c.yaml"}, want: "/tmp/c.yaml"},
		{name: "stops at a bare dash dash", args: []string{"exec", "a", "--", "--config=/x"}, want: ""},
		{name: "dangling flag", args: []string{"--config"}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := peekConfig(tc.args); got != tc.want {
				t.Fatalf("peekConfig(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// TestNoArgsNonTTYPrintsStatus locks in the rule that mabo-ctl never renders a
// full-screen TUI into a pipe: with no arguments and no terminal, it prints the
// status block and exits.
func TestNoArgsNonTTYPrintsStatus(t *testing.T) {
	h := newHarness(t)
	h.env.IsTTY = func() bool { return false }

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	if h.console != 0 {
		t.Fatalf("console ran %d time(s) without a terminal; it must never be launched into a pipe", h.console)
	}
	out := h.stdout.String()
	for _, want := range []string{"SERVICE", "alpha", "beta", "delta", "gamma", "ready"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status block is missing %q:\n%s", want, out)
		}
	}
}

// TestNoArgsTTYRunsConsole is the other half of the same decision.
func TestNoArgsTTYRunsConsole(t *testing.T) {
	h := newHarness(t)
	h.env.IsTTY = func() bool { return true }

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	if h.console != 1 {
		t.Fatalf("console ran %d time(s), want exactly 1", h.console)
	}
	if strings.Contains(h.stdout.String(), "SERVICE") {
		t.Fatalf("the console branch must not also print a status block:\n%s", h.stdout)
	}
}

// TestNoArgsWithStartFlagIsStart checks that a bare `mabo-ctl --all` is a start
// rather than the default action, even though it names no service.
func TestNoArgsWithStartFlagIsStart(t *testing.T) {
	h := newHarness(t, "--all")
	h.env.IsTTY = func() bool { return true }

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	if h.console != 0 {
		t.Fatalf("console ran %d time(s); --all is a start, not the default action", h.console)
	}
	// --all now selects every service BY NAME rather than by passing the empty
	// "default selection": autostart: false narrows the default, and --all must
	// not be narrowed by it.
	// --all now selects every service BY NAME rather than by passing the empty
	// "default selection": autostart: false narrows the default, and --all must
	// not be narrowed by it. The fixture declares four services.
	if got := h.sup.startedNames(); len(got) != 1 || len(got[0]) != 4 {
		t.Fatalf("start selections = %v, want one call naming all four fixture services", got)
	}
}

// TestShorthandStartsService covers `mabo-ctl <svc>` resolving to start.
func TestShorthandStartsService(t *testing.T) {
	h := newHarness(t, "beta")

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	got := h.sup.startedNames()
	want := [][]string{{"beta"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("start selections = %v, want %v", got, want)
	}
	if h.console != 0 {
		t.Fatalf("console ran %d time(s); a named service is a start", h.console)
	}
}

// TestShorthandMatchesExplicitStart pins the shorthand to the same code path as
// `mabo-ctl start <svc>`.
func TestShorthandMatchesExplicitStart(t *testing.T) {
	explicit := newHarness(t, "start", "beta", "gamma")
	if code := explicit.run(); code != exitOK {
		t.Fatalf("explicit start exit code = %d (stderr: %s)", code, explicit.stderr)
	}
	short := newHarness(t, "beta", "gamma")
	if code := short.run(); code != exitOK {
		t.Fatalf("shorthand exit code = %d (stderr: %s)", code, short.stderr)
	}
	if !reflect.DeepEqual(explicit.sup.startedNames(), short.sup.startedNames()) {
		t.Fatalf("shorthand selected %v, explicit start selected %v",
			short.sup.startedNames(), explicit.sup.startedNames())
	}
}

// TestUnknownServiceIsUsageError is the anti-regression for dev.sh bug #2: an
// unknown service must be a loud usage error that lists the real names, never a
// silent no-op.
func TestUnknownServiceIsUsageError(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "shorthand", args: []string{"nope"}},
		{name: "start", args: []string{"start", "nope"}},
		{name: "stop", args: []string{"stop", "nope"}},
		{name: "restart", args: []string{"restart", "nope"}},
		{name: "logs", args: []string{"logs", "nope"}},
		{name: "exec", args: []string{"exec", "nope", "echo"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.args...)
			code := h.run()
			if code != exitUsage {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, h.stderr)
			}
			msg := h.stderr.String()
			if !strings.Contains(msg, `unknown service "nope"`) {
				t.Fatalf("stderr does not name the unknown service:\n%s", msg)
			}
			for _, name := range []string{"alpha", "beta", "delta", "gamma"} {
				if !strings.Contains(msg, name) {
					t.Fatalf("stderr does not list the valid service %q:\n%s", name, msg)
				}
			}
			if len(h.sup.startedNames()) != 0 {
				t.Fatalf("a rejected selection must not reach the supervisor, got %v", h.sup.startedNames())
			}
		})
	}
}

// TestMistypedCommandSuggests checks that a mistyped subcommand is reported as
// a usage error that also points at the command the user meant.
func TestMistypedCommandSuggests(t *testing.T) {
	h := newHarness(t, "staus")
	if code := h.run(); code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
	if msg := h.stderr.String(); !strings.Contains(msg, "status") {
		t.Fatalf("stderr does not suggest the status command:\n%s", msg)
	}
}

// TestPortsFlagOverridesDeclaredPort walks --ports with empty slots all the way
// through resolution and out into the persisted port cache.
func TestPortsFlagOverridesDeclaredPort(t *testing.T) {
	h := newHarness(t, "start", "--ports=,,7999")

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	runEnv := readFile(t, filepath.Join(h.root, ".dev", "run.env"))
	for _, want := range []string{"PORT_ALPHA=7100", "PORT_BETA=7101", "PORT_DELTA=7999"} {
		if !strings.Contains(runEnv, want) {
			t.Fatalf("run.env is missing %q; empty slots must keep the declared default:\n%s", want, runEnv)
		}
	}
}

// TestPortsFlagRejectsTooManySlots checks that a --ports list longer than the
// number of ported services is a usage error rather than a silent truncation.
func TestPortsFlagRejectsTooManySlots(t *testing.T) {
	h := newHarness(t, "start", "--ports=1,2,3,4,5")
	if code := h.run(); code == exitOK {
		t.Fatalf("exit code = %d, want a failure (stderr: %s)", code, h.stderr)
	}
}

// TestPortsFlagBadValueIsUsageError checks that a malformed --ports value is
// reported by the flag parser, which means exit 2.
func TestPortsFlagBadValueIsUsageError(t *testing.T) {
	h := newHarness(t, "start", "--ports=7100,abc")
	if code := h.run(); code != exitUsage {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, h.stderr)
	}
}

// TestPersistedPortOverrideIsAnnounced is the mitigation for the documented
// trap: a persisted .dev/run.env value outranking a changed default must be
// visible, and on stderr so `status --json` stays clean.
func TestPersistedPortOverrideIsAnnounced(t *testing.T) {
	h := newHarness(t, "status")
	mkdir(t, filepath.Join(h.root, ".dev"))
	writeFile(t, filepath.Join(h.root, ".dev", "run.env"), "PORT_ALPHA=7999\n")

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	if msg := h.stderr.String(); !strings.Contains(msg, "port override") {
		t.Fatalf("a persisted port outranking the declared default was not announced:\n%s", msg)
	}
	if out := h.stdout.String(); strings.Contains(out, "port override") {
		t.Fatalf("the override notice must not land on stdout:\n%s", out)
	}
}

// driftedHarness is a harness whose .dev/run.env holds a port the fixture no
// longer declares: the stale-state trap, ready for a refresh conversation.
func driftedHarness(t *testing.T, args ...string) *harness {
	t.Helper()
	h := newHarness(t, args...)
	mkdir(t, filepath.Join(h.root, ".dev"))
	writeFile(t, filepath.Join(h.root, ".dev", "run.env"), "PORT_ALPHA=7999\n")
	return h
}

// runEnvIs reads .dev/run.env and fails the test unless it contains want.
func runEnvIs(t *testing.T, h *harness, want string) {
	t.Helper()
	got := readFile(t, filepath.Join(h.root, ".dev", "run.env"))
	if !strings.Contains(got, want) {
		t.Fatalf(".dev/run.env is missing %q:\n%s", want, got)
	}
}

// TestRefreshPortsFlagAdoptsDeclaredPorts is the scripted form of the drift
// prompt: the flag skips the run.env level, adopts the declared defaults and
// rewrites the file, so the NEXT plain invocation agrees too.
func TestRefreshPortsFlagAdoptsDeclaredPorts(t *testing.T) {
	h := driftedHarness(t, "status", "--refresh-ports")

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	runEnvIs(t, h, "PORT_ALPHA=7100")
	if got := readFile(t, filepath.Join(h.root, ".dev", "run.env")); strings.Contains(got, "7999") {
		t.Fatalf("the stale port survived --refresh-ports:\n%s", got)
	}
	if msg := h.stderr.String(); strings.Contains(msg, "port override") {
		t.Fatalf("an adoption run still announced an override:\n%s", msg)
	}
}

// TestPortDriftPromptAdoptsOnYes: an interactive run with drift asks, and yes
// adopts — the file is rewritten so the answer outlives the invocation.
func TestPortDriftPromptAdoptsOnYes(t *testing.T) {
	h := driftedHarness(t, "status")
	h.env.IsTTY = func() bool { return true }
	h.env.Stdin = strings.NewReader("y\n")

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	runEnvIs(t, h, "PORT_ALPHA=7100")
	if msg := h.stderr.String(); !strings.Contains(msg, "adopted declared ports") {
		t.Fatalf("the adoption was not announced:\n%s", msg)
	}
}

// TestPortDriftPromptKeepsOldOnEnter pins the [y/N] default: pressing Enter
// alone keeps the persisted ports, because a run.env value may have been set
// deliberately and must not move on a careless keystroke.
func TestPortDriftPromptKeepsOldOnEnter(t *testing.T) {
	h := driftedHarness(t, "status")
	h.env.IsTTY = func() bool { return true }
	h.env.Stdin = strings.NewReader("\n")

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	runEnvIs(t, h, "PORT_ALPHA=7999")
	if msg := h.stderr.String(); strings.Contains(msg, "adopted declared ports") {
		t.Fatalf("Enter must not adopt:\n%s", msg)
	}
}

// TestPortDriftPromptNeverBlocksAPipe: without a terminal there is nobody to
// ask, so the run proceeds on the persisted ports with the notice only.
func TestPortDriftPromptNeverBlocksAPipe(t *testing.T) {
	h := driftedHarness(t, "status") // IsTTY false, stdin empty

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	runEnvIs(t, h, "PORT_ALPHA=7999")
	if msg := h.stderr.String(); strings.Contains(msg, "adopt the declared ports?") {
		t.Fatalf("a non-interactive run was asked a question:\n%s", msg)
	}
}

// TestStatusJSONNeverPrompts guards the machine contract: --json on a terminal
// must stay byte-clean even when drift exists and stdin is full of yes.
func TestStatusJSONNeverPrompts(t *testing.T) {
	h := driftedHarness(t, "status", "--json")
	h.env.IsTTY = func() bool { return true }
	h.env.Stdin = strings.NewReader("y\n")

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	var v any
	if err := json.Unmarshal(h.stdout.Bytes(), &v); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, h.stdout)
	}
	runEnvIs(t, h, "PORT_ALPHA=7999")
	if msg := h.stderr.String(); strings.Contains(msg, "adopt the declared ports?") {
		t.Fatalf("--json was asked a question on stderr:\n%s", msg)
	}
}

// TestCaptureEnvUnsetsCallerPort proves the capture happens before anything can
// spawn: after a mabo-ctl run, the caller's variable is gone from the environment
// a child would inherit.
func TestCaptureEnvUnsetsCallerPort(t *testing.T) {
	t.Setenv("BETA_PORT", "7999")
	h := newHarness(t, "start", "beta")

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	if v, ok := os.LookupEnv("BETA_PORT"); ok {
		t.Fatalf("BETA_PORT is still set to %q; a child would inherit a port the supervisor overrode", v)
	}
	runEnv := readFile(t, filepath.Join(h.root, ".dev", "run.env"))
	if !strings.Contains(runEnv, "PORT_BETA=7999") {
		t.Fatalf("the captured caller port did not win resolution:\n%s", runEnv)
	}
}

// TestStartNotReadyExitsFour maps a service that never answered onto exit 4.
func TestStartNotReadyExitsFour(t *testing.T) {
	tests := []struct {
		name  string
		phase supervisor.Phase
	}{
		{name: "died while starting", phase: supervisor.PhaseFailed},
		{name: "alive but not answering", phase: supervisor.PhaseSlow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "start", "beta")
			h.sup.statuses = []supervisor.Status{
				{Name: "alpha", Phase: supervisor.PhaseReady, Port: 7100},
				{Name: "beta", Phase: tc.phase, Port: 7101, Detail: "log is empty"},
			}
			if code := h.run(); code != exitNotReady {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitNotReady, h.stderr)
			}
			if msg := h.stderr.String(); !strings.Contains(msg, "beta") {
				t.Fatalf("stderr does not name the service that failed to become ready:\n%s", msg)
			}
		})
	}
}

// TestStartIgnoresUnselectedFailure checks that exit 4 is decided over the
// SELECTED services only: another service being down is not this command's
// business.
func TestStartIgnoresUnselectedFailure(t *testing.T) {
	h := newHarness(t, "start", "alpha")
	h.sup.statuses = []supervisor.Status{
		{Name: "alpha", Phase: supervisor.PhaseReady, Port: 7100},
		{Name: "beta", Phase: supervisor.PhaseFailed, Port: 7101},
	}
	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
}

// TestMissingConfigExitsThree covers the config exit code.
func TestMissingConfigExitsThree(t *testing.T) {
	h := newHarnessAt(t, t.TempDir(), "status")
	if code := h.run(); code != exitConfig {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitConfig, h.stderr)
	}
	if msg := h.stderr.String(); !strings.Contains(msg, "mabo-ctl.yaml") {
		t.Fatalf("stderr does not mention the missing file:\n%s", msg)
	}
}

// TestInvalidConfigExitsThree covers an invalid file, and checks that every
// problem is listed rather than only the first.
func TestInvalidConfigExitsThree(t *testing.T) {
	h := newHarnessWithConfig(t, "services:\n  - name: bad/name\n    cmd: []\n", "status")
	if code := h.run(); code != exitConfig {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitConfig, h.stderr)
	}
	msg := h.stderr.String()
	if !strings.Contains(msg, "bad/name") {
		t.Fatalf("stderr does not name the invalid service:\n%s", msg)
	}
	if strings.Count(msg, "•") < 2 {
		t.Fatalf("only one problem was reported; validation must list them all:\n%s", msg)
	}
}

// TestConfigFlagOverridesDiscovery checks that --config wins over the walk up
// the tree, and that the capture happened against THAT file's services.
func TestConfigFlagOverridesDiscovery(t *testing.T) {
	other := t.TempDir()
	path := filepath.Join(other, "elsewhere.yaml")
	writeFile(t, path, "services:\n  - name: solo\n    port: 7200\n    cmd: [echo, solo]\n")

	h := newHarness(t, "--config", path, "start", "solo")
	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	want := [][]string{{"solo"}}
	if got := h.sup.startedNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("start selections = %v, want %v", got, want)
	}
}

// TestStatusJSONIsCleanOnStdout checks the machine contract: valid JSON on
// stdout, with the diagnostics kept on stderr.
func TestStatusJSONIsCleanOnStdout(t *testing.T) {
	h := newHarness(t, "status", "--json")
	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	var records []map[string]any
	if err := json.Unmarshal(h.stdout.Bytes(), &records); err != nil {
		t.Fatalf("stdout is not valid JSON (%v):\n%s", err, h.stdout)
	}
	if len(records) != 4 {
		t.Fatalf("got %d records, want 4", len(records))
	}
	if records[0]["service"] != "alpha" {
		t.Fatalf("first record = %v, want service alpha", records[0])
	}
}

// TestStopAndRestartRouting checks the two remaining lifecycle verbs reach the
// supervisor with the selection the user named.
func TestStopAndRestartRouting(t *testing.T) {
	stop := newHarness(t, "stop", "alpha", "beta")
	if code := stop.run(); code != exitOK {
		t.Fatalf("stop exit code = %d (stderr: %s)", code, stop.stderr)
	}
	if want := [][]string{{"alpha", "beta"}}; !reflect.DeepEqual(stop.sup.stopped, want) {
		t.Fatalf("stop selections = %v, want %v", stop.sup.stopped, want)
	}

	restart := newHarness(t, "restart", "delta")
	if code := restart.run(); code != exitOK {
		t.Fatalf("restart exit code = %d (stderr: %s)", code, restart.stderr)
	}
	if want := [][]string{{"delta"}}; !reflect.DeepEqual(restart.sup.restart, want) {
		t.Fatalf("restart selections = %v, want %v", restart.sup.restart, want)
	}
}

// TestResetForwardsForce checks that the destructive reap-by-port is opt-in and
// that the flag reaches the supervisor rather than being decided in the CLI.
func TestResetForwardsForce(t *testing.T) {
	plain := newHarness(t, "reset")
	if code := plain.run(); code != exitOK {
		t.Fatalf("reset exit code = %d (stderr: %s)", code, plain.stderr)
	}
	if plain.sup.resets != 1 || plain.sup.resetForce {
		t.Fatalf("reset calls = %d, force = %v; want one call with force off",
			plain.sup.resets, plain.sup.resetForce)
	}

	forced := newHarness(t, "reset", "--force")
	if code := forced.run(); code != exitOK {
		t.Fatalf("reset --force exit code = %d (stderr: %s)", code, forced.stderr)
	}
	if !forced.sup.resetForce {
		t.Fatal("--force did not reach the supervisor")
	}
}

// TestAllRejectsServiceNames checks that combining --all with names is a usage
// error rather than a silently ignored flag.
func TestAllRejectsServiceNames(t *testing.T) {
	h := newHarness(t, "start", "--all", "alpha")
	if code := h.run(); code != exitUsage {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, h.stderr)
	}
}

// TestLogsPrefixesWhenTailingSeveralServices checks that interleaved logs stay
// attributable, and that a single service's log is printed verbatim.
func TestLogsPrefixesWhenTailingSeveralServices(t *testing.T) {
	single := newHarness(t, "logs", "alpha")
	single.sup.lines = map[string][]string{"alpha": {"only-line"}}
	if code := single.run(); code != exitOK {
		t.Fatalf("logs exit code = %d (stderr: %s)", code, single.stderr)
	}
	if got := strings.TrimSpace(single.stdout.String()); got != "only-line" {
		t.Fatalf("single-service tail = %q, want the raw line", got)
	}

	all := newHarness(t, "logs", "all")
	all.sup.lines = map[string][]string{"alpha": {"a-line"}, "beta": {"b-line"}}
	if code := all.run(); code != exitOK {
		t.Fatalf("logs all exit code = %d (stderr: %s)", code, all.stderr)
	}
	out := all.stdout.String()
	for _, want := range []string{"alpha", "a-line", "beta", "b-line"} {
		if !strings.Contains(out, want) {
			t.Fatalf("merged tail is missing %q:\n%s", want, out)
		}
	}
}

// TestLogsRejectsNegativeTail keeps a nonsensical --tail out of the supervisor.
func TestLogsRejectsNegativeTail(t *testing.T) {
	h := newHarness(t, "logs", "--tail=-3")
	if code := h.run(); code != exitUsage {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, h.stderr)
	}
}

// TestExecForwardsChildExitCode is the reason exec exists as its own command.
func TestExecForwardsChildExitCode(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want int
	}{
		{name: "success", argv: []string{"sh", "-c", "exit 0"}, want: exitOK},
		{name: "seven", argv: []string{"sh", "-c", "exit 7"}, want: 7},
		{name: "one", argv: []string{"sh", "-c", "exit 1"}, want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"exec", "alpha"}, tc.argv...)
			h := newHarness(t, args...)
			if code := h.run(); code != tc.want {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, tc.want, h.stderr)
			}
		})
	}
}

// TestExecRunsInTheServiceEnvironment checks that the child sees the resolved
// ports and the service's working directory, which is what makes exec exact
// rather than approximate.
func TestExecRunsInTheServiceEnvironment(t *testing.T) {
	h := newHarness(t, "exec", "beta", "sh", "-c", "printf '%s %s' \"$BETA_PORT\" \"$PWD\"")
	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	out := h.stdout.String()
	if !strings.HasPrefix(out, "7101 ") {
		t.Fatalf("child did not see the resolved BETA_PORT: %q", out)
	}
	// t.TempDir may hand back a symlinked path, so compare the resolved forms.
	wantDir, err := filepath.EvalSymlinks(h.root)
	if err != nil {
		t.Fatalf("resolve %s: %v", h.root, err)
	}
	gotDir, err := filepath.EvalSymlinks(strings.TrimPrefix(out, "7101 "))
	if err != nil {
		t.Fatalf("resolve child PWD: %v", err)
	}
	if gotDir != wantDir {
		t.Fatalf("child ran in %s, want the service directory %s", gotDir, wantDir)
	}
}

// TestExecPassesFlagsToTheChild checks that mabo-ctl stops claiming flags at the
// service name, with or without the "--" separator people habitually type.
func TestExecPassesFlagsToTheChild(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "bare", args: []string{"exec", "alpha", "sh", "-c", "printf %s \"$*\"", "--", "-q", "--all"}},
		{name: "after a separator", args: []string{"exec", "alpha", "--", "sh", "-c", "printf %s \"$*\"", "--", "-q", "--all"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.args...)
			if code := h.run(); code != exitOK {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
			}
			if got := h.stdout.String(); got != "-q --all" {
				t.Fatalf("child saw %q, want %q; mabo-ctl must not eat the child's flags", got, "-q --all")
			}
		})
	}
}

func TestStripLeadingDashDash(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want []string
	}{
		{in: nil, want: nil},
		{in: []string{"pytest"}, want: []string{"pytest"}},
		{in: []string{"--", "pytest", "-q"}, want: []string{"pytest", "-q"}},
		{in: []string{"pytest", "--", "-q"}, want: []string{"pytest", "--", "-q"}},
		{in: []string{"--"}, want: []string{}},
	}
	for _, tc := range tests {
		if got := stripLeadingDashDash(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("stripLeadingDashDash(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestExecNeedsACommand checks the argument count is a usage error.
func TestExecNeedsACommand(t *testing.T) {
	h := newHarness(t, "exec", "alpha")
	if code := h.run(); code != exitUsage {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, h.stderr)
	}
}

// TestShellUnknownNameListsBoth checks the shell resolution error names the
// declared shells and the declared services.
func TestShellUnknownNameListsBoth(t *testing.T) {
	body := fixture + `
shells:
  - name: db
    service: alpha
    command: [echo, connected]
`
	h := newHarnessWithConfig(t, body, "shell", "nope")
	if code := h.run(); code != exitUsage {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, h.stderr)
	}
	msg := h.stderr.String()
	for _, want := range []string{"db", "alpha", "gamma"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("stderr is missing %q:\n%s", want, msg)
		}
	}
}

// TestShellRunsADeclaredEntry checks that a declared shell runs in the named
// service's environment.
func TestShellRunsADeclaredEntry(t *testing.T) {
	body := fixture + `
shells:
  - name: db
    service: beta
    command: [sh, -c, "printf %s \"$BETA_PORT\""]
`
	h := newHarnessWithConfig(t, body, "shell", "db")
	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	if got := h.stdout.String(); got != "7101" {
		t.Fatalf("declared shell printed %q, want the service's resolved port", got)
	}
}

// TestPreflightRunsDeclaredChecks covers both check kinds and the exit code.
func TestPreflightRunsDeclaredChecks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	pass := newHarnessWithConfig(t, fixture+fmt.Sprintf(`
checks:
  - name: listener
    tcp: %s
  - name: truth
    command: [sh, -c, "exit 0"]
`, addr), "preflight")
	if code := pass.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, pass.stderr)
	}
	if out := pass.stdout.String(); !strings.Contains(out, "listener") || !strings.Contains(out, "passed") {
		t.Fatalf("preflight output does not report the passing checks:\n%s", out)
	}

	fail := newHarnessWithConfig(t, fixture+`
checks:
  - name: lies
    command: [sh, -c, "echo nope >&2; exit 1"]
`, "preflight")
	if code := fail.run(); code != exitFailure {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitFailure, fail.stderr)
	}
	if out := fail.stdout.String(); !strings.Contains(out, "failed") {
		t.Fatalf("preflight output does not report the failing check:\n%s", out)
	}
}

// TestHealthExitCodes checks the mapping health applies to the supervisor's
// phases: ready is exit 0 for every service that declares a health URL, and
// anything else is exit 4.
//
// The phases themselves are the supervisor's — health no longer derives its own
// — so what is tested here is the mapping, and the derivation is tested against
// real processes in internal/supervisor.
func TestHealthExitCodes(t *testing.T) {
	healthy := "services:\n  - name: alpha\n    cmd: [echo, alpha]\n    health: http://127.0.0.1:7100/\n"

	tests := []struct {
		name  string
		phase supervisor.Phase
		want  int
	}{
		{name: "ready", phase: supervisor.PhaseReady, want: exitOK},
		{name: "alive but past its startup window", phase: supervisor.PhaseDegraded, want: exitNotReady},
		{name: "alive and still starting", phase: supervisor.PhaseSlow, want: exitNotReady},
		{name: "crashed after coming up", phase: supervisor.PhaseExited, want: exitNotReady},
		{name: "never started", phase: supervisor.PhaseStopped, want: exitNotReady},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessWithConfig(t, healthy, "health")
			h.sup.statuses = []supervisor.Status{{
				Name:   "alpha",
				Phase:  tc.phase,
				Health: "http://127.0.0.1:7100/",
			}}
			if code := h.run(); code != tc.want {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, tc.want, h.stderr)
			}
		})
	}
}

// TestHealthIgnoresServicesWithNothingToProbe checks that a service declaring
// no health URL cannot fail `mabo-ctl health`. The command's question is "did
// every declared health URL answer", and a service that declares none has not
// answered nothing — there was nothing to ask.
func TestHealthIgnoresServicesWithNothingToProbe(t *testing.T) {
	h := newHarnessWithConfig(t,
		"services:\n  - name: gamma\n    cmd: [echo, gamma]\n", "health")
	h.sup.statuses = []supervisor.Status{{Name: "gamma", Phase: supervisor.PhaseStopped}}
	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	if out := h.stdout.String(); !strings.Contains(out, "no health check declared") {
		t.Fatalf("health did not say there was nothing to probe:\n%s", out)
	}
}

// TestStatusAndHealthAgreeOnThePhase is the regression test for the drift this
// change removed: `mabo-ctl status` and `mabo-ctl health` used to report DIFFERENT
// phases for one service in one state — slow and exit 0 from one, failed and
// exit 4 from the other — because health maintained its own probe loop and its
// own phase table beside the supervisor's.
//
// Both commands now render the same supervisor statuses, so every phase must
// read identically in both blocks. Running the real phase vocabulary through
// both is what makes a future second derivation fail here.
func TestStatusAndHealthAgreeOnThePhase(t *testing.T) {
	cfg := "services:\n  - name: alpha\n    cmd: [echo, alpha]\n    health: http://127.0.0.1:7100/\n"

	for _, phase := range supervisor.Phases() {
		t.Run(string(phase), func(t *testing.T) {
			sts := []supervisor.Status{{
				Name:   "alpha",
				Phase:  phase,
				Health: "http://127.0.0.1:7100/",
			}}

			status := newHarnessWithConfig(t, cfg, "status")
			status.sup.statuses = sts
			status.run()

			health := newHarnessWithConfig(t, cfg, "health")
			health.sup.statuses = sts
			health.run()

			_, word := ui.PhaseLabel(phase)
			if !strings.Contains(status.stdout.String(), word) {
				t.Fatalf("status did not report %q:\n%s", word, status.stdout)
			}
			if !strings.Contains(health.stdout.String(), word) {
				t.Fatalf("health reported a different phase from status, which said %q:\n%s",
					word, health.stdout)
			}
		})
	}
}

// TestOpenPassesURLsToTheOpener checks that open hands each running service's
// base URL to the platform opener as a value, never as a shell string.
func TestOpenPassesURLsToTheOpener(t *testing.T) {
	h := newHarness(t, "open")
	var mu sync.Mutex
	var opened []string
	h.env.OpenURL = func(_ context.Context, u string) error {
		mu.Lock()
		defer mu.Unlock()
		opened = append(opened, u)
		return nil
	}
	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	want := []string{"http://localhost:7100/", "http://localhost:7101/", "http://localhost:7102/"}
	if !reflect.DeepEqual(opened, want) {
		t.Fatalf("opened %v, want %v (the portless service has no URL)", opened, want)
	}
}

func TestBrowseURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      service.Instance
		want    string
		wantErr string
	}{
		{
			name: "health URL is reduced to its origin",
			in:   service.Instance{Name: "a", Health: "http://localhost:7100/robots.txt?x=1", Port: 7100},
			want: "http://localhost:7100/",
		},
		{
			name: "portless and probeless has nothing to open",
			in:   service.Instance{Name: "worker"},
			want: "",
		},
		{
			name: "port alone gives a base URL",
			in:   service.Instance{Name: "a", Port: 7100},
			want: "http://localhost:7100/",
		},
		{
			name:    "a non-http scheme is refused",
			in:      service.Instance{Name: "a", Health: "file:///etc/passwd"},
			wantErr: "only http and https",
		},
		{
			name: "an absolute open: URL wins over the derived origin",
			in:   service.Instance{Name: "api", Health: "http://localhost:7100/health", Port: 7100, Open: "http://localhost:7100/docs"},
			want: "http://localhost:7100/docs",
		},
		{
			name: "a relative open: path joins the port origin",
			in:   service.Instance{Name: "api", Port: 7100, Open: "/docs"},
			want: "http://localhost:7100/docs",
		},
		{
			name: "a relative open: path joins a tcp-probed service's port origin",
			in: service.Instance{
				Name: "pg", Port: 5432, Health: "tcp:localhost:5432",
				Probe: service.Probe{Kind: service.ProbeTCP, Addr: "localhost:5432"},
				Open:  "/admin",
			},
			want: "http://localhost:5432/admin",
		},
		{
			name:    "open: with a foreign scheme is refused even with an origin",
			in:      service.Instance{Name: "api", Port: 7100, Open: "javascript:alert(1)"},
			wantErr: "only http and https",
		},
		{
			name:    "a path open: on a portless service cannot be joined",
			in:      service.Instance{Name: "worker", Open: "/docs"},
			wantErr: "no origin to join it against",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := browseURL(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("browseURL = (%q, %v), want an error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("browseURL: unexpected error %v", err)
			}
			if got != tc.want {
				t.Fatalf("browseURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLookPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tool := filepath.Join(bin, "tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	notExec := filepath.Join(bin, "data")
	if err := os.WriteFile(notExec, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Run("found on the service PATH", func(t *testing.T) {
		got, err := lookPath("tool", dir, bin)
		if err != nil {
			t.Fatalf("lookPath: %v", err)
		}
		if got != tool {
			t.Fatalf("lookPath = %q, want %q", got, tool)
		}
	})
	t.Run("relative path resolves against the service dir", func(t *testing.T) {
		got, err := lookPath("bin/tool", dir, "")
		if err != nil {
			t.Fatalf("lookPath: %v", err)
		}
		if got != tool {
			t.Fatalf("lookPath = %q, want %q", got, tool)
		}
	})
	t.Run("a non-executable file is not a match", func(t *testing.T) {
		if got, err := lookPath("data", dir, bin); err == nil {
			t.Fatalf("lookPath = %q, want an error for a non-executable file", got)
		}
	})
	t.Run("the error names the PATH that was searched", func(t *testing.T) {
		_, err := lookPath("absent", dir, bin)
		if err == nil {
			t.Fatal("lookPath: want an error")
		}
		if !strings.Contains(err.Error(), bin) {
			t.Fatalf("error %q does not name the PATH %q", err, bin)
		}
	})
}

// TestCompletionScripts checks every supported shell emits something, and that
// an unsupported shell is a usage error rather than an empty file.
func TestCompletionScripts(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		h := newHarness(t, "completion", shell)
		if code := h.run(); code != exitOK {
			t.Fatalf("completion %s exit code = %d (stderr: %s)", shell, code, h.stderr)
		}
		if !strings.Contains(h.stdout.String(), "mabo-ctl") {
			t.Fatalf("completion %s produced no script:\n%s", shell, h.stdout)
		}
	}
	h := newHarness(t, "completion", "tcsh")
	if code := h.run(); code != exitUsage {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, h.stderr)
	}
}

// TestHelpNeedsNoConfig checks that help works in a directory with no
// mabo-ctl.yaml, and that the exit codes are documented where a script author
// would look for them.
func TestHelpNeedsNoConfig(t *testing.T) {
	h := newHarnessAt(t, t.TempDir(), "--help")
	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	out := h.stdout.String()
	for _, want := range []string{"Exit codes:", "3  mabo-ctl.yaml", "4  a service failed to become ready"} {
		if !strings.Contains(out, want) {
			t.Fatalf("root help does not document %q:\n%s", want, out)
		}
	}
}

// TestUnknownFlagIsUsageError checks that cobra's own parse errors carry exit 2.
func TestUnknownFlagIsUsageError(t *testing.T) {
	h := newHarness(t, "status", "--nonsense")
	if code := h.run(); code != exitUsage {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "Usage:") {
		t.Fatalf("a usage error must be followed by the usage text:\n%s", h.stderr)
	}
}

// TestNotReady enumerates the phases that mean "not usable yet".
func TestNotReady(t *testing.T) {
	t.Parallel()
	sts := []supervisor.Status{
		{Name: "a", Phase: supervisor.PhaseReady},
		{Name: "b", Phase: supervisor.PhaseRunning},
		{Name: "c", Phase: supervisor.PhaseStopped},
		{Name: "d", Phase: supervisor.PhaseSlow},
		{Name: "e", Phase: supervisor.PhaseFailed},
	}
	want := []string{"d", "e"}
	if got := notReady(sts); !reflect.DeepEqual(got, want) {
		t.Fatalf("notReady = %v, want %v", got, want)
	}
}

func TestFilterStatus(t *testing.T) {
	t.Parallel()
	sts := []supervisor.Status{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	if got := filterStatus(sts, nil); len(got) != 3 {
		t.Fatalf("an empty selection must keep everything, got %v", got)
	}
	got := filterStatus(sts, []string{"c", "a"})
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Fatalf("filterStatus = %v, want a then c in supervisor order", got)
	}
}

func TestJoinAnd(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   []string
		want string
	}{
		{in: nil, want: ""},
		{in: []string{"a"}, want: "a"},
		{in: []string{"a", "b"}, want: "a and b"},
		{in: []string{"a", "b", "c"}, want: "a, b and c"},
	}
	for _, tc := range tests {
		if got := joinAnd(tc.in); got != tc.want {
			t.Fatalf("joinAnd(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// readFile reads a file the test expects to exist.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// writeFile writes a file the test needs, creating nothing else.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// mkdir creates a directory the test needs.
func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

// TestAllFlagSelectsEveryServiceIncludingManualOnes pins the distinction that
// was collapsed: naming nothing is a DEFAULT, which autostart narrows; --all is
// an INSTRUCTION, which it must not. A stack where every service opts out — a
// dozen workers you normally start by name — turned "Start all" into a control
// that did nothing.
func TestAllFlagSelectsEveryServiceIncludingManualOnes(t *testing.T) {
	const body = `
services:
  - name: a
    dir: .
    autostart: false
    cmd: [echo, hi]
  - name: b
    dir: .
    autostart: false
    cmd: [echo, hi]
`
	h := newHarnessWithConfig(t, body, "start", "--all")
	h.run()

	got := h.sup.startedNames()
	if len(got) != 1 {
		t.Fatalf("start calls = %d, want 1", len(got))
	}
	if len(got[0]) != 2 {
		t.Errorf("--all selected %v, want both services", got[0])
	}
}

// TestBareStartHonoursAutostart is the other side: with no names and no --all,
// the per-service default applies.
func TestBareStartHonoursAutostart(t *testing.T) {
	const body = `
services:
  - name: a
    dir: .
    cmd: [echo, hi]
  - name: b
    dir: .
    autostart: false
    cmd: [echo, hi]
`
	h := newHarnessWithConfig(t, body, "start")
	h.run()

	got := h.sup.startedNames()
	if len(got) != 1 {
		t.Fatalf("start calls = %d, want 1", len(got))
	}
	// nil means "the supervisor's default selection", which is where the
	// autostart filter lives; the CLI must not pre-expand it.
	if len(got[0]) != 0 {
		t.Errorf("bare start selected %v, want the default selection (nil)", got[0])
	}
}

// TestNamedPortsFlagAccumulates: repeatable --port SERVICE=PORT, duplicates
// rejected rather than resolved by order of appearance.
func TestNamedPortsFlagAccumulates(t *testing.T) {
	t.Parallel()
	var f namedPortsFlag
	if err := f.Set("backend=7999"); err != nil {
		t.Fatalf("Set backend: %v", err)
	}
	if err := f.Set("web = 7100 "); err != nil {
		t.Fatalf("Set web (with spaces): %v", err)
	}
	if f.values["backend"] != 7999 || f.values["web"] != 7100 {
		t.Fatalf("values = %v, want both overrides kept", f.values)
	}
	if err := f.Set("backend=8000"); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("duplicate backend: err = %v, want a more-than-once error", err)
	}
	for _, bad := range []string{"backend", "=7999", "backend=", "backend=nope", "backend=99999"} {
		var g namedPortsFlag
		if err := g.Set(bad); err == nil {
			t.Errorf("Set(%q) accepted a malformed override", bad)
		}
	}
}

// mabo-ctl init

// TestInitScaffoldsFromDetection: package.json + .nvmrc, manage.py and
// Cargo.toml are each recognised; EVERY service line stays commented so nothing
// can run until a human intervenes.
func TestInitScaffoldsFromDetection(t *testing.T) {
	h := newHarnessAt(t, t.TempDir(), "init")
	// frontend: node with a dev script and an .nvmrc
	fe := filepath.Join(h.root, "frontend")
	if err := os.MkdirAll(fe, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fe, "package.json"), `{"scripts": {"dev": "vite"}}`)
	writeFile(t, filepath.Join(fe, ".nvmrc"), "v24.4.0\n")
	// backend: Django
	be := filepath.Join(h.root, "backend")
	if err := os.MkdirAll(be, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(be, "manage.py"), "")
	// api: Rust — must be ignored for the run: it is a guess, not a command
	if err := os.MkdirAll(filepath.Join(h.root, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}

	code := h.run()
	if code != 0 {
		t.Fatalf("init exited %d, stderr: %s", code, h.stderr.String())
	}

	body, err := os.ReadFile(filepath.Join(h.root, "mabo-ctl.yaml"))
	if err != nil {
		t.Fatalf("no scaffolded config: %v", err)
	}
	text := string(body)

	for _, want := range []string{
		"#   cmd: [npm, run, dev]",
		"#   runtime: node:v24.4.0",
		"# - name: backend",
		"#   cmd: [python, manage.py, runserver]",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("scaffold does not contain %q:\n%s", want, text)
		}
	}
	// The generated file itself must PARSE cleanly but declare no runnable
	// service yet: an untouched scaffold refuses to start anything, and the
	// refusal tells the reader exactly what is missing.
	cfg, err := config.Load(filepath.Join(h.root, "mabo-ctl.yaml"))
	if err == nil {
		t.Fatalf("an untouched scaffold loaded as %+v; nothing may run before a human edits it", cfg)
	}
	var ve *config.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("scaffold error = %v, want a *ValidationError", err)
	}
}

// TestInitRefusesToOverwrite: both spellings of the config are protected.
func TestInitRefusesToOverwrite(t *testing.T) {
	h := newHarnessAt(t, t.TempDir(), "init")
	writeFile(t, filepath.Join(h.root, "mabo-ctl.yaml"), "services: []\n")
	if code := h.run(); code == 0 {
		t.Fatal("init over an existing config exited 0")
	}

	h2 := newHarnessAt(t, t.TempDir(), "init")
	writeFile(t, filepath.Join(h2.root, config.LegacyFileName), "services: []\n")
	out := h2.run()
	if out == 0 {
		t.Fatal("init over a legacy-named config exited 0")
	}
	if !strings.Contains(h2.stderr.String(), "rename") {
		t.Errorf("legacy refusal should say rename; stderr = %s", h2.stderr.String())
	}
}

// TestInitAddsDevToGitIgnore once, not twice.
func TestInitAddsDevToGitIgnore(t *testing.T) {
	h := newHarnessAt(t, t.TempDir(), "init")
	writeFile(t, filepath.Join(h.root, ".gitignore"), "node_modules/\n")
	if code := h.run(); code != 0 {
		t.Fatalf("init exited %d", code)
	}
	body, _ := os.ReadFile(filepath.Join(h.root, ".gitignore"))
	if !strings.Contains(string(body), ".dev/") {
		t.Errorf(".gitignore = %q, want .dev/ appended", body)
	}

	// Second run on a tree whose gitignore already carries it says nothing new,
	// and the file is not duplicated.
	h2 := newHarnessAt(t, t.TempDir(), "init")
	writeFile(t, filepath.Join(h2.root, ".gitignore"), ".dev/\n")
	if code := h2.run(); code != 0 {
		t.Fatalf("second init exited %d", code)
	}
	body2, _ := os.ReadFile(filepath.Join(h2.root, ".gitignore"))
	if strings.Count(string(body2), ".dev/") != 1 {
		t.Errorf(".gitignore = %q, want exactly one .dev/", body2)
	}
}

// --notify: the crash watcher behind the resident front ends

// TestNotifierFiresOnlyOnLiveToDeadTransitions: a watcher that announced the
// state it STARTED with would pop a dialog for every already-dead service on
// boot; only a death DURING residency is news.
func TestNotifierFiresOnlyOnLiveToDeadTransitions(t *testing.T) {
	sup := &fakeSup{statuses: []supervisor.Status{
		{Name: "api", Phase: supervisor.PhaseExited, Detail: "killed by SIGSEGV, 0s ago\nlog line"},
	}}
	n := newNotifier(sup, func(context.Context, string, string) error { return nil })

	// First poll learns the world; nothing may fire.
	n.poll(t.Context())
	var sent []string
	n.send = func(_ context.Context, title, body string) error {
		sent = append(sent, title+" | "+body)
		return nil
	}

	// The service comes up and then dies: one notification.
	sup.statuses = []supervisor.Status{
		{Name: "api", Phase: supervisor.PhaseReady},
	}
	n.poll(t.Context())
	sup.statuses = []supervisor.Status{
		{Name: "api", Phase: supervisor.PhaseExited, Detail: "killed by SIGKILL, 1s ago\npanic: gone"},
	}
	n.poll(t.Context())

	if len(sent) != 1 {
		t.Fatalf("notifications = %v, want exactly one for the live→dead transition", sent)
	}
	if !strings.Contains(sent[0], "api exited") || !strings.Contains(sent[0], "killed by SIGKILL") {
		t.Errorf("notification %q does not name the service, phase and cause", sent[0])
	}
	// And a second poll of the same dead state stays silent.
	n.poll(t.Context())
	if len(sent) != 1 {
		t.Errorf("repeat polls re-fired: %v", sent)
	}
}

// TestNotifierStaysQuietForDeliberateStop: stopped is not news.
func TestNotifierStaysQuietForDeliberateStop(t *testing.T) {
	sup := &fakeSup{statuses: []supervisor.Status{{Name: "api", Phase: supervisor.PhaseReady}}}
	n := newNotifier(sup, func(context.Context, string, string) error { return nil })
	n.poll(t.Context())

	n.send = func(context.Context, string, string) error {
		t.Error("a deliberate stop produced a notification")
		return nil
	}
	sup.statuses = []supervisor.Status{{Name: "api", Phase: supervisor.PhaseStopped}}
	n.poll(t.Context())
}

// TestAppleScriptEscapesAndTruncates: the body is interpolated into an
// AppleScript string literal, so a quote in a log line must not end it early,
// and a stack-trace line must not flood the notification centre.
func TestAppleScriptEscapesAndTruncates(t *testing.T) {
	got := appleScriptNotification(`t"itle`, `he said "hi" \ done`)
	want := `display notification "he said \"hi\" \\ done" with title "t\"itle"`
	if got != want {
		t.Errorf("appleScriptNotification = %s, want %s", got, want)
	}
	long := strings.Repeat("x", 500)
	if got := truncateRunes(long, notifyBodyLimit); len([]rune(got)) > notifyBodyLimit+1 {
		t.Errorf("truncateRunes kept %d runes over a %d limit", len([]rune(got)), notifyBodyLimit)
	}
}

// TestNotifyFlagIsWiredToStartAndServe: the flag exists where residency lives.
func TestNotifyFlagIsWiredToStartAndServe(t *testing.T) {
	h := newHarnessAt(t, t.TempDir(), "start", "--help")
	h.run()
	if !strings.Contains(h.stdout.String(), "--notify") && !strings.Contains(h.stderr.String(), "--notify") {
		t.Errorf("start help does not mention --notify")
	}
}

// preflight's machine-readiness pass (#9) and expect: on tcp checks

// TestPreflightMachinePassFailsWhenTheRuntimeIsMissing: a service whose
// interpreter cannot resolve on this machine is a FAIL before any declared
// check runs, with the same message start would have produced.
func TestPreflightMachinePassFailsWhenTheRuntimeIsMissing(t *testing.T) {
	h := newHarnessWithConfig(t, `
services:
  - name: ghosted
    runtime: node:18.99.9-not-installed
    cmd: [node, server.js]
checks:
  - name: always-green
    command: [true]
`, "preflight")
	if code := h.run(); code != exitFailure {
		t.Fatalf("exit = %d, want %d; stderr:\n%s", code, exitFailure, h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, "MACHINE") || !strings.Contains(out, "cannot start") {
		t.Fatalf("machine pass missing from output:\n%s", out)
	}
	if !strings.Contains(out, "always-green") || !strings.Contains(out, "passed") {
		t.Fatalf("declared checks must still run beside the machine pass:\n%s", out)
	}
	if want := "ghosted"; !strings.Contains(out, want) {
		t.Fatalf("the failing service must be named:\n%s", out)
	}
}

// TestPreflightMachinePassAdvisesOnNvmrcWithoutRuntimeVersion: an .nvmrc in a
// service directory with no runtime: version pinned is a WARN with a hint —
// advisory, never an exit-1 failure.
func TestPreflightMachinePassAdvisesOnNvmrcWithoutRuntimeVersion(t *testing.T) {
	root := t.TempDir()
	svc := filepath.Join(root, "frontend")
	if err := os.MkdirAll(svc, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(svc, ".nvmrc"), "v24.4.0\n")
	writeFile(t, filepath.Join(root, "mabo-ctl.yaml"), `
services:
  - name: frontend
    dir: frontend
    cmd: [echo, up]
`)

	h := newHarnessAt(t, root, "preflight")
	if code := h.run(); code != exitOK {
		t.Fatalf("an advisory warning must not fail preflight; exit %d, stderr:\n%s", code, h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, ".nvmrc pins v24") || !strings.Contains(out, "warn") {
		t.Fatalf("advisory line missing:\n%s", out)
	}
}

// TestPreflightExpectFreeNamesAListenerHoldingThePort: expect: free flips the
// dial around — connecting becomes the failure, and the failure names WHO holds it.
func TestPreflightExpectFreeNamesAListenerHoldingThePort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	addr := l.Addr().String()

	bad := newHarnessWithConfig(t, fmt.Sprintf(`
services:
  - name: api
    cmd: [echo, hi]
checks:
  - name: api-port-free
    tcp: %s
    expect: free
`, addr), "preflight")
	if code := bad.run(); code != exitFailure {
		t.Fatalf("occupied port should fail expect: free; exit %d", code)
	}
	out := bad.stdout.String()
	if !strings.Contains(out, "should be free but is listening") {
		t.Fatalf("failure does not say the port should be free:\n%s", out)
	}

	_ = l.Close() // now free: the same check passes
	good := newHarnessWithConfig(t, fmt.Sprintf(`
services:
  - name: api
    cmd: [echo, hi]
checks:
  - name: api-port-free
    tcp: %s
    expect: free
`, addr), "preflight")
	if code := good.run(); code != exitOK {
		t.Fatalf("a refused dial should pass expect: free; exit %d, stderr:\n%s", code, good.stderr.String())
	}
	if !strings.Contains(good.stdout.String(), "is free") {
		t.Fatalf("passing detail does not say the port is free:\n%s", good.stdout.String())
	}
}

// logs --timestamps (#16): opt-in, follow-only, read-time stamps stated as
// such. The stamps are applied in runTail over whatever the lifecycle feeds it,
// so the tests drive fakeSup's canned lines; supervisor.Tail itself is covered
// in its own package.

// TestLogsTimestampsIsFollowOnly: the flag is refused on a historical tail —
// a stamp there would be when the tailer read the line, not when it was written.
func TestLogsTimestampsIsFollowOnly(t *testing.T) {
	h := newHarnessWithConfig(t, fixture, "logs", "alpha", "--timestamps")
	if code := h.run(); code != exitUsage {
		t.Fatalf("exit = %d, want %d; stderr:\n%s", code, exitUsage, h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "follow-only") {
		t.Fatalf("refusal does not say why:\n%s", h.stderr.String())
	}
}

// TestLogsTimestampsStampsEveryStreamedLine: with -f --timestamps each line the
// lifecycle hands runTail carries an HH:MM:SS.mmm prefix — single service and
// interleaved alike.
func TestLogsTimestampsStampsEveryStreamedLine(t *testing.T) {
	h := newHarnessWithConfig(t, fixture, "logs", "alpha", "-f", "--timestamps")
	h.sup.lines = map[string][]string{"alpha": {"first", "second"}}

	if code := h.run(); code != exitOK {
		t.Fatalf("exit = %d, stderr:\n%s", code, h.stderr.String())
	}
	for _, want := range []string{"first", "second"} {
		i := strings.Index(h.stdout.String(), want)
		if i < 0 {
			t.Fatalf("line %q missing:\n%s", want, h.stdout.String())
		}
		prefix := h.stdout.String()[max(0, i-13):i]
		if len(prefix) < 12 || prefix[2] != ':' || prefix[5] != ':' || prefix[8] != '.' {
			t.Fatalf("line %q lacks an HH:MM:SS.mmm stamp (prefix %q):\n%s", want, prefix, h.stdout.String())
		}
	}

	interleaved := newHarnessWithConfig(t, fixture, "logs", "all", "-f", "--timestamps")
	interleaved.sup.lines = map[string][]string{
		"alpha": {"a-one"}, "beta": {"b-one"},
	}
	if code := interleaved.run(); code != exitOK {
		t.Fatalf("interleaved exit = %d, stderr:\n%s", code, interleaved.stderr.String())
	}
	for _, want := range []string{"a-one", "b-one"} {
		i := strings.Index(interleaved.stdout.String(), want)
		prefix := interleaved.stdout.String()[max(0, i-13):i]
		if len(prefix) < 12 || prefix[8] != '.' {
			t.Fatalf("interleaved line %q not stamped (prefix %q):\n%s", want, prefix, interleaved.stdout.String())
		}
	}
}

// tty: + attach (#17 done the way the roadmap allowed)

// TestAttachRefusesAServiceWithoutTTY: pretend-attaching to an /dev/null-stdin
// service would be a lie; the refusal names the field that would fix it.
func TestAttachRefusesAServiceWithoutTTY(t *testing.T) {
	h := newHarnessWithConfig(t, fixture, "attach", "alpha")
	if code := h.run(); code != exitUsage {
		t.Fatalf("exit = %d, want %d; stderr:\\n%s", code, exitUsage, h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "tty: true") {
		t.Fatalf("refusal does not name the missing declaration:\\n%s", h.stderr.String())
	}

	bad := newHarnessWithConfig(t, fixture, "attach", "nosuch")
	if code := bad.run(); code != exitUsage || !strings.Contains(bad.stderr.String(), "unknown service") {
		t.Fatalf("unknown service not refused; exit %d stderr:\n%s", code, bad.stderr.String())
	}
}
