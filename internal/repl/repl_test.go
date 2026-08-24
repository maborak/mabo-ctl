package repl

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe to read while the session writes to it.
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

// fakeDispatcher records the argv vectors it was handed and replays canned
// results, so the loop can be exercised without a command tree.
type fakeDispatcher struct {
	mu    sync.Mutex
	calls [][]string
	cmds  []Command
	// errs maps argv[0] to the error Dispatch returns for it.
	errs map[string]error
	// entered receives once per call, before Dispatch blocks.
	entered chan struct{}
	// hold makes Dispatch wait for ctx to be cancelled.
	hold bool
}

// Commands returns the canned command list.
func (f *fakeDispatcher) Commands() []Command { return f.cmds }

// Dispatch records argv and replays the canned outcome.
func (f *fakeDispatcher) Dispatch(ctx context.Context, argv []string) error {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), argv...))
	err := f.errs[argv[0]]
	hold := f.hold
	f.mu.Unlock()

	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if hold {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

// recorded returns a copy of the recorded calls.
func (f *fakeDispatcher) recorded() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.calls...)
}

// fakeMonitor replays canned statuses and counts how often it was asked.
type fakeMonitor struct {
	mu    sync.Mutex
	sts   []Status
	polls int
}

// Status returns the current canned statuses.
func (m *fakeMonitor) Status(context.Context) []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.polls++
	return append([]Status(nil), m.sts...)
}

// set replaces the canned statuses.
func (m *fakeMonitor) set(sts ...Status) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sts = sts
}

// polled reports how many times Status was called.
func (m *fakeMonitor) polled() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.polls
}

// runLines feeds script to a session and returns everything it printed.
func runLines(t *testing.T, opt Options, script string) string {
	t.Helper()
	out := &syncBuffer{}
	opt.In = strings.NewReader(script)
	opt.Out = out
	if err := Run(context.Background(), opt); err != nil {
		t.Fatalf("Run: unexpected error %v", err)
	}
	return out.String()
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTokenize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    []string
		wantErr string
	}{
		{name: "empty", in: "", want: nil},
		{name: "blank", in: "   \t ", want: nil},
		{name: "one word", in: "status", want: []string{"status"}},
		{name: "words and flags", in: "logs web -f --tail=20",
			want: []string{"logs", "web", "-f", "--tail=20"}},
		{name: "collapses runs of space", in: "  start   api  ", want: []string{"start", "api"}},
		{name: "double quotes group", in: `exec api pytest -k "not slow"`,
			want: []string{"exec", "api", "pytest", "-k", "not slow"}},
		{name: "single quotes are literal", in: `exec api sh -c 'echo "hi"'`,
			want: []string{"exec", "api", "sh", "-c", `echo "hi"`}},
		{name: "empty quoted argument survives", in: `exec api echo ""`,
			want: []string{"exec", "api", "echo", ""}},
		{name: "escaped quote inside quotes", in: `echo "a\"b"`, want: []string{"echo", `a"b`}},
		{name: "backslash escapes a space", in: `logs my\ service`, want: []string{"logs", "my service"}},
		{name: "quotes glue to a word", in: `--tail="20"`, want: []string{"--tail=20"}},
		{name: "no variable expansion", in: `echo $HOME`, want: []string{"echo", "$HOME"}},
		{name: "no glob expansion", in: `echo *.go`, want: []string{"echo", "*.go"}},
		{name: "unbalanced double quote", in: `echo "oops`, wantErr: `unbalanced "`},
		{name: "unbalanced single quote", in: `echo 'oops`, wantErr: "unbalanced '"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tokenize(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("tokenize(%q) = %q, want an error containing %q", tc.in, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("tokenize(%q) error = %q, want it to contain %q", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("tokenize(%q): unexpected error %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("tokenize(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

// TestDispatchReachesTheCommandTree is the whole design in one assertion: a
// typed line becomes an argv vector and is handed to the tree unchanged, with
// no per-verb code anywhere in between.
func TestDispatchReachesTheCommandTree(t *testing.T) {
	t.Parallel()
	fd := &fakeDispatcher{}
	runLines(t, Options{Commands: fd}, "status\nstart api\nlogs web -f\nexec api pytest -k \"not slow\"\nquit\n")

	want := [][]string{
		{"status"},
		{"start", "api"},
		{"logs", "web", "-f"},
		{"exec", "api", "pytest", "-k", "not slow"},
	}
	if got := fd.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("dispatched %#v, want %#v", got, want)
	}
}

// TestBlankLinesAreNotDispatched keeps an empty prompt from running the tree's
// default action, which on a terminal would open the full-screen console from
// inside the line-oriented one.
func TestBlankLinesAreNotDispatched(t *testing.T) {
	t.Parallel()
	fd := &fakeDispatcher{}
	runLines(t, Options{Commands: fd}, "\n   \n\t\nquit\n")
	if got := fd.recorded(); len(got) != 0 {
		t.Fatalf("dispatched %#v, want nothing", got)
	}
}

// TestUnknownCommandDoesNotExit is the requirement that a typo costs a message
// and not the session.
func TestUnknownCommandDoesNotExit(t *testing.T) {
	t.Parallel()
	fd := &fakeDispatcher{
		cmds: []Command{{Name: "status", Short: "print status"}, {Name: "start", Short: "start"}},
		errs: map[string]error{"nosuchverb": errors.New(`unknown service "nosuchverb"`)},
	}
	out := runLines(t, Options{Commands: fd}, "nosuchverb\nstatus\nquit\n")

	if !strings.Contains(out, "commands: status, start, serve, unserve, help, quit, exit") {
		t.Fatalf("an unknown word must print the valid set; got:\n%s", out)
	}
	want := [][]string{{"nosuchverb"}, {"status"}}
	if got := fd.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the session did not continue past the unknown word: dispatched %#v, want %#v", got, want)
	}
	if !strings.Contains(out, "leaving the console") {
		t.Fatalf("the session must end at quit, not at the unknown word; got:\n%s", out)
	}
}

// TestCommandErrorDoesNotExit covers the other half: a KNOWN command that
// fails. One failed start must not end the session, and it must not drag the
// unknown-command list in with it.
func TestCommandErrorDoesNotExit(t *testing.T) {
	t.Parallel()
	fd := &fakeDispatcher{
		cmds: []Command{{Name: "start", Short: "start"}, {Name: "status", Short: "status"}},
		errs: map[string]error{"start": errors.New("api did not become ready")},
	}
	out := runLines(t, Options{Commands: fd}, "start api\nstatus\nquit\n")

	if !strings.Contains(out, "error: api did not become ready") {
		t.Fatalf("the error must be printed; got:\n%s", out)
	}
	if strings.Contains(out, "commands: start, status") {
		t.Fatalf("a known command that failed must not be reported as an unknown word; got:\n%s", out)
	}
	want := [][]string{{"start", "api"}, {"status"}}
	if got := fd.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the session did not continue past the failure: dispatched %#v, want %#v", got, want)
	}
}

// TestUnbalancedQuoteDoesNotExit keeps a mistyped quote at the level of a
// message rather than a dead session, and keeps it away from the tree.
func TestUnbalancedQuoteDoesNotExit(t *testing.T) {
	t.Parallel()
	fd := &fakeDispatcher{}
	out := runLines(t, Options{Commands: fd}, "exec api echo \"oops\nstatus\nquit\n")
	if !strings.Contains(out, "unbalanced") {
		t.Fatalf("want the unbalanced-quote message; got:\n%s", out)
	}
	want := [][]string{{"status"}}
	if got := fd.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("dispatched %#v, want %#v", got, want)
	}
}

// TestQuitLeavesServicesRunning asserts both halves of the promise: nothing is
// dispatched on the way out, and the exit line says so.
func TestQuitLeavesServicesRunning(t *testing.T) {
	t.Parallel()
	for _, line := range []string{"quit\n", "exit\n", ""} {
		fd := &fakeDispatcher{}
		out := runLines(t, Options{Commands: fd}, line)
		if got := fd.recorded(); len(got) != 0 {
			t.Fatalf("leaving must not run anything, but it dispatched %#v", got)
		}
		if !strings.Contains(out, "STILL RUNNING") || !strings.Contains(out, `"stop"`) {
			t.Fatalf("the exit line must say the services keep running and how to stop them; got:\n%s", out)
		}
	}
}

// TestEndOfInputQuits covers Ctrl-D, including a last line with no newline
// after it.
func TestEndOfInputQuits(t *testing.T) {
	t.Parallel()
	fd := &fakeDispatcher{}
	out := runLines(t, Options{Commands: fd}, "status")
	if got, want := fd.recorded(), [][]string{{"status"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a final line without a newline must still run: dispatched %#v, want %#v", got, want)
	}
	if !strings.Contains(out, "leaving the console") {
		t.Fatalf("end of input must leave the console; got:\n%s", out)
	}
}

// TestPromptCarriesTheRepoName pins the prompt, including that it carries no
// service counter — see the package comment on why a live count cannot be live
// behind a blocking read.
func TestPromptCarriesTheRepoName(t *testing.T) {
	t.Parallel()
	out := runLines(t, Options{Commands: &fakeDispatcher{}, Repo: "amazon-watcher"}, "quit\n")
	if !strings.Contains(out, "mabo-ctl(amazon-watcher)> ") {
		t.Fatalf("want the repo in the prompt; got:\n%s", out)
	}
	if strings.Contains(out, "↑") {
		t.Fatalf("the prompt must not carry a counter it cannot keep true; got:\n%s", out)
	}
	if got := promptFor(""); got != "mabo-ctl> " {
		t.Fatalf("promptFor(\"\") = %q, want %q", got, "mabo-ctl> ")
	}
}

// TestHelpListsBothCommandSets checks that help is derived from the tree rather
// than written out here, and that a verb this package owns is listed once.
func TestHelpListsBothCommandSets(t *testing.T) {
	t.Parallel()
	fd := &fakeDispatcher{cmds: []Command{
		{Name: "status", Short: "print one line per service"},
		{Name: "serve", Short: "serve the web console until interrupted"},
	}}
	out := runLines(t, Options{Commands: fd}, "help\nquit\n")

	if !strings.Contains(out, "print one line per service") {
		t.Fatalf("help must list the tree's commands; got:\n%s", out)
	}
	if strings.Contains(out, "serve the web console until interrupted") {
		t.Fatalf("the tree's blocking serve must not be advertised; the console owns serve. got:\n%s", out)
	}
	if n := strings.Count(out, "\n  serve "); n != 1 {
		t.Fatalf("serve must be listed exactly once, got %d times:\n%s", n, out)
	}
	if !strings.Contains(out, "unserve") {
		t.Fatalf("help must list unserve; got:\n%s", out)
	}
}

// TestInterruptAtThePromptDoesNotEndTheSession is the Ctrl-C requirement: it
// abandons the line, it does not kill anything.
func TestInterruptAtThePromptDoesNotEndTheSession(t *testing.T) {
	t.Parallel()
	fd := &fakeDispatcher{}
	interrupts := make(chan struct{}, 1)
	out := &syncBuffer{}
	pr, pw := io.Pipe()

	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{
			In: pr, Out: out, Commands: fd, Interrupts: interrupts, Interactive: true,
		})
	}()

	waitFor(t, "the first prompt", func() bool { return strings.Contains(out.String(), "mabo-ctl> ") })
	interrupts <- struct{}{}
	waitFor(t, "the interrupt to be acknowledged", func() bool { return strings.Contains(out.String(), "^C") })

	if _, err := io.WriteString(pw, "status\nquit\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not end after quit")
	}
	_ = pw.Close()

	if got, want := fd.recorded(), [][]string{{"status"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the session must survive Ctrl-C: dispatched %#v, want %#v", got, want)
	}
	if !strings.Contains(out.String(), eraseLine) {
		t.Fatalf("on a terminal the prompt line must be erased before an asynchronous line; got:\n%q", out.String())
	}
}

// TestInterruptCancelsTheRunningCommand is the other half: Ctrl-C during a long
// operation cancels THAT operation and hands the prompt back.
func TestInterruptCancelsTheRunningCommand(t *testing.T) {
	t.Parallel()
	fd := &fakeDispatcher{hold: true, entered: make(chan struct{}, 1)}
	interrupts := make(chan struct{}, 1)
	out := &syncBuffer{}
	pr, pw := io.Pipe()

	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{
			In: pr, Out: out, Commands: fd, Interrupts: interrupts,
		})
	}()

	if _, err := io.WriteString(pw, "start --all\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-fd.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the command never started")
	}

	interrupts <- struct{}{}
	waitFor(t, "the prompt to come back", func() bool {
		return strings.Count(out.String(), "mabo-ctl> ") >= 2
	})

	if _, err := io.WriteString(pw, "quit\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not end after quit")
	}
	_ = pw.Close()

	if !strings.Contains(out.String(), "context canceled") {
		t.Fatalf("the cancelled command's error must be reported; got:\n%s", out.String())
	}
}

// TestCrashIsAnnouncedOnce covers the reason residency is worth having: a death
// that happens while the user sits at the prompt reaches the scrollback, once,
// without the prompt being clobbered.
func TestCrashIsAnnouncedOnce(t *testing.T) {
	t.Parallel()
	mon := &fakeMonitor{sts: []Status{{Name: "api"}}}
	out := &syncBuffer{}
	pr, pw := io.Pipe()

	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{
			In: pr, Out: out, Commands: &fakeDispatcher{}, Watch: mon,
			Poll: time.Millisecond, Interactive: true,
		})
	}()

	waitFor(t, "the baseline poll", func() bool { return mon.polled() >= 2 })
	mon.set(Status{
		Name: "api", Dead: true, ExitCode: 1,
		StartedAt: time.Now().Add(-time.Minute), ExitedAt: time.Now(),
	})
	waitFor(t, "the crash line", func() bool {
		return strings.Contains(out.String(), "api exited (code 1)")
	})
	waitFor(t, "several more polls of the same death", func() bool { return mon.polled() >= 20 })

	if _, err := io.WriteString(pw, "quit\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not end after quit")
	}
	_ = pw.Close()

	if n := strings.Count(out.String(), "api exited (code 1)"); n != 1 {
		t.Fatalf("one death must be announced exactly once, got %d:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), eraseLine+"api exited") {
		t.Fatalf("the notice must erase the prompt line before writing; got:\n%q", out.String())
	}
	if !strings.Contains(out.String(), `run "logs api"`) {
		t.Fatalf("the notice must point at the log; got:\n%s", out.String())
	}
}

// TestCrashAlreadyPresentIsNotAnnounced keeps the console from reporting, as
// news, the state it was opened to look at.
func TestCrashAlreadyPresentIsNotAnnounced(t *testing.T) {
	t.Parallel()
	dead := Status{
		Name: "api", Dead: true, ExitCode: 2,
		StartedAt: time.Now().Add(-time.Hour), ExitedAt: time.Now().Add(-time.Minute),
	}
	mon := &fakeMonitor{sts: []Status{dead}}
	out := &syncBuffer{}
	pr, pw := io.Pipe()

	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{
			In: pr, Out: out, Commands: &fakeDispatcher{}, Watch: mon, Poll: time.Millisecond,
		})
	}()
	waitFor(t, "several polls", func() bool { return mon.polled() >= 10 })

	if _, err := io.WriteString(pw, "quit\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = pw.Close()

	if strings.Contains(out.String(), "api exited") {
		t.Fatalf("a death present before the console opened is not news; got:\n%s", out.String())
	}
}

// TestStartupDeathIsNotAnnounced keeps the watcher from repeating, two seconds
// later, the failure the foreground `start` has already printed in full.
func TestStartupDeathIsNotAnnounced(t *testing.T) {
	t.Parallel()
	mon := &fakeMonitor{sts: []Status{{Name: "api"}}}
	out := &syncBuffer{}
	pr, pw := io.Pipe()

	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{
			In: pr, Out: out, Commands: &fakeDispatcher{}, Watch: mon, Poll: time.Millisecond,
		})
	}()
	waitFor(t, "the baseline poll", func() bool { return mon.polled() >= 2 })
	mon.set(Status{Name: "api", Dead: true, Startup: true, ExitCode: 1, ExitedAt: time.Now()})
	waitFor(t, "several more polls", func() bool { return mon.polled() >= 20 })

	if _, err := io.WriteString(pw, "quit\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = pw.Close()

	if strings.Contains(out.String(), "api exited") {
		t.Fatalf("a startup death is the foreground start's to report; got:\n%s", out.String())
	}
}

func TestCrashLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   Status
		want string
	}{
		{name: "exit code", in: Status{Name: "api", ExitCode: 1}, want: "api exited (code 1)"},
		{name: "signal", in: Status{Name: "api", ExitSignal: "SIGKILL"}, want: "api exited (killed by SIGKILL)"},
		{name: "unwitnessed", in: Status{Name: "api", ExitCode: -1}, want: "api exited (exit status unknown)"},
		{name: "clean exit is still a death", in: Status{Name: "api"}, want: "api exited (code 0)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := crashLine(tc.in); !strings.HasPrefix(got, tc.want) {
				t.Fatalf("crashLine = %q, want it to start with %q", got, tc.want)
			}
		})
	}
}

// portListener is a Listener over a real socket, so the tests can assert that
// unserve actually released the port rather than only said it had.
type portListener struct {
	mu      sync.Mutex
	addr    string
	ln      net.Listener
	listens int
}

// Listen binds the socket and records the address it landed on.
func (p *portListener) Listen() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listens++
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		return err
	}
	p.ln, p.addr = ln, ln.Addr().String()
	return nil
}

// URL renders the bound address.
func (p *portListener) URL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return "http://" + p.addr + "/?token=abc"
}

// ListenAndServe holds the socket open until ctx is done, then closes it.
func (p *portListener) ListenAndServe(ctx context.Context) error {
	<-ctx.Done()
	p.mu.Lock()
	ln := p.ln
	p.ln = nil
	p.mu.Unlock()
	if ln == nil {
		return nil
	}
	return ln.Close()
}

// bound reports the address and how many binds were attempted.
func (p *portListener) bound() (string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.addr, p.listens
}

// TestServeTwiceDoesNotBindTwice is why serve is native: a second `serve` must
// report the console that is already open, not fail with an address in use.
func TestServeTwiceDoesNotBindTwice(t *testing.T) {
	t.Parallel()
	pl := &portListener{addr: "127.0.0.1:0"}
	made := 0
	out := runLines(t, Options{
		Commands: &fakeDispatcher{},
		NewListener: func(string) (Listener, error) {
			made++
			return pl, nil
		},
	}, "serve\nserve\nquit\n")

	addr, listens := pl.bound()
	if listens != 1 {
		t.Fatalf("bound %d times, want 1", listens)
	}
	if made != 1 {
		t.Fatalf("built %d listeners, want 1", made)
	}
	if !strings.Contains(out, "console serving at http://"+addr) {
		t.Fatalf("the first serve must print the bound URL; got:\n%s", out)
	}
	if !strings.Contains(out, "already serving at http://"+addr) {
		t.Fatalf("the second serve must print the existing URL; got:\n%s", out)
	}
}

// TestUnserveReleasesThePort asserts the socket is closed, not just reported
// closed: the port is rebound afterwards.
func TestUnserveReleasesThePort(t *testing.T) {
	t.Parallel()
	pl := &portListener{addr: "127.0.0.1:0"}
	out := runLines(t, Options{
		Commands:    &fakeDispatcher{},
		NewListener: func(string) (Listener, error) { return pl, nil },
	}, "serve\nunserve\nunserve\nquit\n")

	addr, _ := pl.bound()
	if !strings.Contains(out, "console stopped") {
		t.Fatalf("unserve must say so; got:\n%s", out)
	}
	if !strings.Contains(out, `no console is running; type "serve" to start one`) {
		t.Fatalf("a second unserve must say there is nothing to stop; got:\n%s", out)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("unserve did not release %s: %v", addr, err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestQuitReleasesTheServedPort covers the listener's lifetime being the
// SESSION: leaving must not leak the socket.
func TestQuitReleasesTheServedPort(t *testing.T) {
	t.Parallel()
	pl := &portListener{addr: "127.0.0.1:0"}
	runLines(t, Options{
		Commands:    &fakeDispatcher{},
		NewListener: func(string) (Listener, error) { return pl, nil },
	}, "serve\nquit\n")

	addr, _ := pl.bound()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("quitting did not release %s: %v", addr, err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestServeReportsABindFailureAndStaysUp keeps a taken port from ending the
// session, and keeps the console from claiming to serve when it did not bind.
func TestServeReportsABindFailureAndStaysUp(t *testing.T) {
	t.Parallel()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = busy.Close() }()

	pl := &portListener{addr: busy.Addr().String()}
	fd := &fakeDispatcher{}
	out := runLines(t, Options{
		Commands:    fd,
		NewListener: func(string) (Listener, error) { return pl, nil },
	}, "serve\nstatus\nunserve\nquit\n")

	if strings.Contains(out, "console serving at") {
		t.Fatalf("a failed bind must not be announced as serving; got:\n%s", out)
	}
	if !strings.Contains(out, "no console is running") {
		t.Fatalf("a failed bind must leave nothing registered; got:\n%s", out)
	}
	if got, want := fd.recorded(), [][]string{{"status"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the session must survive a failed bind: dispatched %#v, want %#v", got, want)
	}
}

// TestServeArgumentIsPassedThrough checks the optional address reaches the
// factory, which is what makes `serve 127.0.0.1:0` a usable escape from a
// taken default port.
func TestServeArgumentIsPassedThrough(t *testing.T) {
	t.Parallel()
	var got []string
	runLines(t, Options{
		Commands: &fakeDispatcher{},
		NewListener: func(addr string) (Listener, error) {
			got = append(got, addr)
			return &portListener{addr: "127.0.0.1:0"}, nil
		},
	}, "serve 127.0.0.1:0\nunserve\nserve\nquit\n")

	if want := []string{"127.0.0.1:0", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("factory saw %#v, want %#v", got, want)
	}
}

// TestAdoptedConsoleIsServedAndReleased covers Options.Console: a console the
// caller bound and announced — which is what `mabo-ctl start --web-console` hands
// over — is served for the session, reported by `serve` rather than bound a
// second time, and released when the session ends.
//
// The release is the load-bearing half. A listener the caller kept for itself
// would need a second shutdown path, and a socket with two owners is a socket
// with none.
func TestAdoptedConsoleIsServedAndReleased(t *testing.T) {
	t.Parallel()
	adopted := &portListener{addr: "127.0.0.1:0"}
	if err := adopted.Listen(); err != nil {
		t.Fatalf("bind the console the caller hands over: %v", err)
	}
	made := 0

	out := runLines(t, Options{
		Commands: &fakeDispatcher{},
		Console:  adopted,
		NewListener: func(string) (Listener, error) {
			made++
			return &portListener{addr: "127.0.0.1:0"}, nil
		},
	}, "serve\nquit\n")

	addr, listens := adopted.bound()
	if listens != 1 {
		t.Fatalf("the adopted console was bound %d times, want the caller's single bind", listens)
	}
	if made != 0 {
		t.Fatalf("built %d listeners, want 0: `serve` must report the adopted console, not bind another", made)
	}
	if !strings.Contains(out, "already serving at http://"+addr) {
		t.Fatalf("`serve` did not report the adopted console; got:\n%s", out)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("quitting did not release the adopted console's port %s: %v", addr, err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestUnserveStopsAnAdoptedConsole checks the adopted console goes through the
// same verb the session's own does, rather than being a listener only quitting
// can stop.
func TestUnserveStopsAnAdoptedConsole(t *testing.T) {
	t.Parallel()
	adopted := &portListener{addr: "127.0.0.1:0"}
	if err := adopted.Listen(); err != nil {
		t.Fatalf("bind: %v", err)
	}

	out := runLines(t, Options{Commands: &fakeDispatcher{}, Console: adopted}, "unserve\nquit\n")
	if !strings.Contains(out, "console stopped") {
		t.Fatalf("unserve did not stop the adopted console; got:\n%s", out)
	}
	addr, _ := adopted.bound()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("unserve did not release %s: %v", addr, err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestServeWithoutAListenerSaysSo covers the degenerate wiring rather than
// panicking on a nil factory.
func TestServeWithoutAListenerSaysSo(t *testing.T) {
	t.Parallel()
	out := runLines(t, Options{Commands: &fakeDispatcher{}}, "serve\nquit\n")
	if !strings.Contains(out, "`serve` is unavailable here") {
		t.Fatalf("want the unavailable message; got:\n%s", out)
	}
}

// TestReadErrorIsReported distinguishes a broken input stream, which is a real
// failure, from end of input, which is a normal exit.
func TestReadErrorIsReported(t *testing.T) {
	t.Parallel()
	err := Run(context.Background(), Options{
		Commands: &fakeDispatcher{},
		In:       errReader{},
		Out:      io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "read input") {
		t.Fatalf("Run error = %v, want a read failure", err)
	}
}

// errReader fails every read.
type errReader struct{}

// Read always fails.
func (errReader) Read([]byte) (int, error) { return 0, errors.New("device not configured") }
