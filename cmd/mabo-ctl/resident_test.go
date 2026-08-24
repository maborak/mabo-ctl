package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// syncBuffer is a bytes.Buffer safe to read while mabo-ctl writes to it, which is
// what a test that inspects the output of a session that is still running
// needs.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// Write appends p under the lock.
func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// String returns everything written so far.
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitForURL polls out until a bare URL appears on a line of its own, which is
// how every mabo-ctl console announces itself: token included, nothing else on
// the line, and only once the socket is bound.
func waitForURL(t *testing.T, out *syncBuffer) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, line := range strings.Split(out.String(), "\n") {
			if line = strings.TrimSpace(line); strings.HasPrefix(line, "http://") {
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no console URL on a line of its own in:\n%s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStartAttachHandsOffToTheConsole is the cheap half of the operator's ask:
// start, then stay, in the full-screen console that already exists.
func TestStartAttachHandsOffToTheConsole(t *testing.T) {
	for _, flag := range []string{"-a", "--attach"} {
		t.Run(flag, func(t *testing.T) {
			h := newHarness(t, "start", flag, "alpha")
			h.env.IsTTY = func() bool { return true }

			if code := h.run(); code != exitOK {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
			}
			if h.console != 1 {
				t.Fatalf("console ran %d time(s), want exactly 1", h.console)
			}
			if got, want := len(h.sup.startedNames()), 1; got != want {
				t.Fatalf("start calls = %d, want %d; --attach must still start what it was asked to", got, want)
			}
		})
	}
}

// TestStartAttachWithoutATerminalStartsAndExits is the rule that keeps every
// pipeline alive: no terminal, no residency, and the exit code and stdout of a
// plain `mabo-ctl start`.
func TestStartAttachWithoutATerminalStartsAndExits(t *testing.T) {
	plain := newHarness(t, "start", "alpha")
	if code := plain.run(); code != exitOK {
		t.Fatalf("plain start exit code = %d (stderr: %s)", code, plain.stderr)
	}

	h := newHarness(t, "start", "--attach", "alpha")
	h.env.IsTTY = func() bool { return false }

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	if h.console != 0 {
		t.Fatalf("console ran %d time(s) without a terminal; it must never be launched into a pipe", h.console)
	}
	if got, want := h.stdout.String(), plain.stdout.String(); got != want {
		t.Fatalf("stdout with --attach = %q, want the plain start's %q byte for byte", got, want)
	}
	if msg := h.stderr.String(); !strings.Contains(msg, "--attach") || !strings.Contains(msg, "terminal") {
		t.Fatalf("stderr does not say the flag was ignored for want of a terminal:\n%s", msg)
	}
}

// TestStartInteractiveOpensThePrompt covers the second flag on a terminal.
func TestStartInteractiveOpensThePrompt(t *testing.T) {
	for _, flag := range []string{"-i", "--interactive"} {
		t.Run(flag, func(t *testing.T) {
			h := newHarness(t, "start", flag, "alpha")
			h.env.IsTTY = func() bool { return true }
			h.env.Stdin = strings.NewReader("status\nquit\n")

			if code := h.run(); code != exitOK {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
			}
			out := h.stdout.String()
			if !strings.Contains(out, "mabo-ctl(") {
				t.Fatalf("want the prompt after the start; got:\n%s", out)
			}
			if !strings.Contains(out, "STILL RUNNING") {
				t.Fatalf("want the prompt's farewell, which promises the services outlive it; got:\n%s", out)
			}
			if h.console != 0 {
				t.Fatalf("the full-screen console ran %d time(s); --interactive is the other one", h.console)
			}
		})
	}
}

// TestStartInteractiveWithoutATerminalStartsAndExits is the non-TTY half.
func TestStartInteractiveWithoutATerminalStartsAndExits(t *testing.T) {
	h := newHarness(t, "start", "-i", "alpha")
	h.env.IsTTY = func() bool { return false }
	h.env.Stdin = strings.NewReader("status\nquit\n")

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	if out := h.stdout.String(); strings.Contains(out, "mabo-ctl(") {
		t.Fatalf("a prompt was opened without a terminal:\n%s", out)
	}
	if msg := h.stderr.String(); !strings.Contains(msg, "--interactive") {
		t.Fatalf("stderr does not name the flag it ignored:\n%s", msg)
	}
}

// TestStartWebConsoleServesAndPrintsItsURL is the whole of the third flag: the
// URL is printed on a line of its own with the port that was actually bound and
// the token that opens it, the console answers on it, and leaving the prompt
// releases the port — one listener, one shutdown path.
func TestStartWebConsoleServesAndPrintsItsURL(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "on its own", args: []string{"start", "--web-console"}},
		{name: "with --interactive, which it implies", args: []string{"start", "-i", "--web-console"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.args...)
			out := &syncBuffer{}
			h.env.Stdout = out
			h.env.IsTTY = func() bool { return true }

			// A pipe keeps the session open while the test drives the console,
			// which a fixed script could not: it would have quit already.
			pr, pw := io.Pipe()
			h.env.Stdin = pr

			code := make(chan int, 1)
			go func() { code <- run(h.env) }()

			raw := waitForURL(t, out)
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("printed URL %q does not parse: %v", raw, err)
			}
			if token := u.Query().Get("token"); len(token) != 64 {
				t.Errorf("printed URL carries token %q, want the 32-byte session token hex encoded", token)
			}
			if port := u.Port(); port == "" || port == "0" {
				t.Errorf("printed URL %q carries port %q, so it was printed before the bind resolved it", raw, port)
			}
			if host := u.Hostname(); host != "127.0.0.1" {
				t.Errorf("printed URL %q is bound to %q, want loopback", raw, host)
			}

			resp, err := http.Get(raw) //nolint:noctx // the deadline is the test's own.
			if err != nil {
				t.Fatalf("GET %s: %v — the adopted console was bound but never served", raw, err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s = %d, want 200", raw, resp.StatusCode)
			}

			if _, err := io.WriteString(pw, "quit\n"); err != nil {
				t.Fatalf("write to the prompt: %v", err)
			}
			_ = pw.Close()

			select {
			case got := <-code:
				if got != exitOK {
					t.Fatalf("exit code = %d, want %d (stderr: %s)", got, exitOK, h.stderr)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("mabo-ctl did not return after the prompt was quit")
			}

			// Safe to read now: the session has returned, so it has already
			// waited for the listener to close.
			ln, err := net.Listen("tcp", u.Host)
			if err != nil {
				t.Fatalf("leaving the prompt did not release %s: %v", u.Host, err)
			}
			if err := ln.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			msg := h.stderr.String()
			if !strings.Contains(msg, "password") {
				t.Errorf("stderr does not say the token is a credential:\n%s", msg)
			}
			if strings.Contains(msg, "Ctrl-C stops the console") {
				t.Errorf("stderr tells the user to press Ctrl-C, which the prompt swallows:\n%s", msg)
			}
			if !strings.Contains(msg, "unserve") {
				t.Errorf("stderr does not say how to stop this console:\n%s", msg)
			}
		})
	}
}

// TestStartWebConsoleWithoutATerminalBindsNothing keeps mabo-ctl from opening a
// socket it would drop a moment later.
func TestStartWebConsoleWithoutATerminalBindsNothing(t *testing.T) {
	h := newHarness(t, "start", "--web-console")
	h.env.IsTTY = func() bool { return false }

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	if out := h.stdout.String(); strings.Contains(out, "http://") {
		t.Fatalf("a console URL was printed without a terminal:\n%s", out)
	}
	if msg := h.stderr.String(); !strings.Contains(msg, "--web-console") {
		t.Fatalf("stderr does not name the flag it ignored:\n%s", msg)
	}
}

// TestStartWebConsoleNeverImpliesTheDangerFlag is the control that must not
// move: asking for a console is not authorising one on the network. The refusal
// is a usage error, and it happens before anything is started or bound.
func TestStartWebConsoleNeverImpliesTheDangerFlag(t *testing.T) {
	h := newHarness(t, "start", "--web-console", "--web-addr", "0.0.0.0:0")
	h.env.IsTTY = func() bool { return true }
	h.env.Stdin = strings.NewReader("quit\n")

	if code := h.run(); code != exitUsage {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, h.stderr)
	}
	msg := h.stderr.String()
	for _, want := range []string{"0.0.0.0:0", "--i-know-this-is-dangerous"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, msg)
		}
	}
	if out := h.stdout.String(); strings.Contains(out, "http://") {
		t.Errorf("a URL was printed for a bind that was refused:\n%s", out)
	}
	if got := h.sup.startedNames(); len(got) != 0 {
		t.Errorf("a refused command line still started %v; it must not half-happen", got)
	}
}

// TestStartResidentFlagsAreMutuallyExclusive covers every pair that would hand
// one terminal to two things. Each is a usage error that starts nothing.
//
// --interactive with --web-console is deliberately NOT here: the console needs
// a resident host and the prompt is it, so asking for both asks for one thing.
func TestStartResidentFlagsAreMutuallyExclusive(t *testing.T) {
	tests := [][]string{
		{"start", "-a", "-i"},
		{"start", "--attach", "--web-console"},
		{"start", "-f", "-a"},
		{"start", "-f", "-i"},
		{"start", "--follow", "--web-console"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			h := newHarness(t, args...)
			h.env.IsTTY = func() bool { return true }
			h.env.Stdin = strings.NewReader("quit\n")

			if code := h.run(); code != exitUsage {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, h.stderr)
			}
			if msg := h.stderr.String(); !strings.Contains(msg, "cannot be combined") {
				t.Fatalf("stderr does not say the flags conflict:\n%s", msg)
			}
			if got := h.sup.startedNames(); len(got) != 0 {
				t.Fatalf("a rejected command line started %v", got)
			}
			if h.console != 0 {
				t.Fatalf("a rejected command line opened the console %d time(s)", h.console)
			}
		})
	}
}

// TestStartWebFlagsNeedWebConsole keeps a flag that would silently do nothing
// from being accepted. A --web-addr nobody reads is how a user ends up
// convinced they bound an address they did not.
func TestStartWebFlagsNeedWebConsole(t *testing.T) {
	tests := [][]string{
		{"start", "--web-addr", "127.0.0.1:0"},
		{"start", "--i-know-this-is-dangerous"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			h := newHarness(t, args...)
			h.env.IsTTY = func() bool { return true }

			if code := h.run(); code != exitUsage {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, h.stderr)
			}
			if msg := h.stderr.String(); !strings.Contains(msg, "--web-console") {
				t.Fatalf("stderr does not name the flag that was missing:\n%s", msg)
			}
			if got := h.sup.startedNames(); len(got) != 0 {
				t.Fatalf("a rejected command line started %v", got)
			}
		})
	}
}

// TestResidentModeKeepsAFailedStartsExitCode is the promise the root help
// makes: the failure already happened, and quitting the session cleanly does
// not erase it.
func TestResidentModeKeepsAFailedStartsExitCode(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "attach", args: []string{"start", "-a", "beta"}},
		{name: "interactive", args: []string{"start", "-i", "beta"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.args...)
			h.env.IsTTY = func() bool { return true }
			h.env.Stdin = strings.NewReader("quit\n")
			h.sup.statuses = []supervisor.Status{
				{Name: "beta", Phase: supervisor.PhaseFailed, Port: 7101, Detail: "log is empty"},
			}

			if code := h.run(); code != exitNotReady {
				t.Fatalf("exit code = %d, want %d; a clean quit must not swallow the failed start (stderr: %s)",
					code, exitNotReady, h.stderr)
			}
			// The session still ran: a failed start is exactly when a human
			// wants the logs and a restart key.
			if tc.name == "attach" && h.console != 1 {
				t.Fatalf("console ran %d time(s), want 1 even though the start failed", h.console)
			}
			if tc.name == "interactive" && !strings.Contains(h.stdout.String(), "mabo-ctl(") {
				t.Fatalf("the prompt did not open after a failed start:\n%s", h.stdout)
			}
		})
	}
}

// TestSessionFailureDoesNotHideAFailedStart pins the other half of that rule:
// when the session ALSO fails, both are reported and the start's code is the
// one mabo-ctl exits with.
func TestSessionFailureDoesNotHideAFailedStart(t *testing.T) {
	t.Parallel()
	joined := errors.Join(
		withCode(exitNotReady, errors.New("beta did not become ready")),
		errors.New("the console lost the terminal"),
	)
	if got := exitCodeFor(joined); got != exitNotReady {
		t.Fatalf("exitCodeFor = %d, want %d: the start's failure is the more actionable half", got, exitNotReady)
	}
	for _, want := range []string{"beta did not become ready", "the console lost the terminal"} {
		if !strings.Contains(joined.Error(), want) {
			t.Fatalf("the reported error drops %q: %v", want, joined)
		}
	}
}

// TestResidentModeIsRefusedInsideThePrompt keeps two loops off one stdin, and
// keeps the web console from being bound by something that cannot own it.
func TestResidentModeIsRefusedInsideThePrompt(t *testing.T) {
	for _, line := range []string{"start -a alpha", "start -i alpha", "start --web-console alpha"} {
		t.Run(line, func(t *testing.T) {
			h := newHarness(t, "repl")
			// This exercises what happens INSIDE a prompt, and a prompt only
			// opens on a terminal — `mabo-ctl repl` without one is refused before
			// the loop starts, which is a different test.
			h.env.IsTTY = func() bool { return true }
			h.env.Stdin = strings.NewReader(line + "\nquit\n")

			if code := h.run(); code != exitOK {
				t.Fatalf("exit code = %d, want %d; a refused line must not end the session", code, exitOK)
			}
			out := h.stdout.String()
			if !strings.Contains(out, "cannot be used from inside the interactive console") {
				t.Fatalf("want the refusal; got:\n%s", out)
			}
			if strings.Contains(out, "http://") {
				t.Fatalf("a console was bound by a line that was refused:\n%s", out)
			}
			if got := h.sup.startedNames(); len(got) != 0 {
				t.Fatalf("a refused line started %v", got)
			}
		})
	}
}

// TestRootHelpDocumentsTheResidentFlags checks the flags are discoverable where
// a user looks first, together with the two rules that surprise people: no
// terminal means no residency, and a failed start survives the quit.
func TestRootHelpDocumentsTheResidentFlags(t *testing.T) {
	h := newHarnessAt(t, t.TempDir(), "--help")
	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	out := h.stdout.String()
	for _, want := range []string{
		"mabo-ctl start -a", "mabo-ctl start -i", "--web-console",
		"--i-know-this-is-dangerous", "terminal", "still exit 4",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("root help does not document %q:\n%s", want, out)
		}
	}
}

// TestREPLRefusesWithoutATerminal is the guard every other resident entry point
// already had. `mabo-ctl repl` piped into a script or a CI step would otherwise
// sit forever on a prompt nobody is typing at, and the pipeline hangs.
func TestREPLRefusesWithoutATerminal(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "repl")
	h.env.IsTTY = func() bool { return false }
	h.env.Stdin = strings.NewReader("status\nquit\n")

	if code := h.run(); code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(h.stderr.String(), "needs a terminal") {
		t.Fatalf("want an explanation on stderr; got:\n%s", h.stderr.String())
	}
}

// TestUnsafeWebAddrIsRefusedWithoutATerminal pins the fix for a security check
// whose answer depended on whether stdin was a terminal.
//
// The loopback check lived inside newWebConsole, which prepareHandOff skips
// when there is no terminal — so the same command line was refused with exit 2
// at a prompt and accepted with exit 0 through a pipe, having started every
// service first. Nothing was ever bound either way, so nothing was exposed; the
// defect is that a command mabo-ctl will not carry out half-happened, and that a
// control nobody can predict is not a control.
func TestUnsafeWebAddrIsRefusedWithoutATerminal(t *testing.T) {
	t.Parallel()
	for _, tty := range []bool{true, false} {
		name := "tty"
		if !tty {
			name = "pipe"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "start", "--web-console", "--web-addr", "0.0.0.0:0", "alpha")
			h.env.IsTTY = func() bool { return tty }

			if code := h.run(); code != exitUsage {
				t.Fatalf("exit code = %d, want %d; the refusal must not depend on a terminal", code, exitUsage)
			}
			if got := h.stderr.String() + h.stdout.String(); !strings.Contains(got, "--i-know-this-is-dangerous") {
				t.Errorf("the refusal does not say how to override:\n%s", got)
			}
			// Refused BEFORE anything ran: a command line mabo-ctl will not carry
			// out must not half-happen.
			if n := len(h.sup.startedNames()); n != 0 {
				t.Errorf("%d service(s) were started by a command that was refused", n)
			}
		})
	}
}

// TestSafeWebAddrIsAcceptedOnBothPaths is the other half: the check must not
// have become a blanket refusal.
func TestSafeWebAddrIsAcceptedOnBothPaths(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "start", "--web-console", "--web-addr", "127.0.0.1:0", "alpha")
	h.env.IsTTY = func() bool { return false }

	if code := h.run(); code == exitUsage {
		t.Fatalf("a loopback --web-addr was refused as a usage error:\n%s", h.stderr.String())
	}
}
