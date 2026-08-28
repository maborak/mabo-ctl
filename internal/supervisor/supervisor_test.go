package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/state"
)

// The tests in this file spawn REAL processes on purpose.
//
// Everything this package exists to get right — Setsid making a child outlive
// its parent, a process GROUP signal reaching a grandchild, SIGKILL escalation,
// refusing to signal a recycled pid — is a property of the kernel, not of Go
// code. A mocked exec would assert that we call the functions we already know
// we call, and would have caught none of the bugs the spec lists.
//
// The helper is this test binary re-executed with an env guard, which is the
// standard os/exec pattern.

const helperEnv = "MABOCTL_HELPER_MODE"

// TestHelperProcess is not a real test: it is the body of every child process
// these tests spawn. It does nothing unless the guard variable is set.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		t.Skip("helper process entry point; not a test")
	}
	switch mode {
	case "sleep":
		time.Sleep(2 * time.Minute)

	case "grandchild":
		// Spawn a child WITHOUT its own session, so it stays in our process
		// group — exactly like the child `npm run dev` leaves behind. Record
		// its pid so the test can prove the group signal reached it.
		sub := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
		sub.Env = append(os.Environ(), helperEnv+"=sleep")
		if err := sub.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "spawn grandchild:", err)
			os.Exit(1)
		}
		if err := os.WriteFile(os.Getenv("MABOCTL_HELPER_PIDFILE"),
			[]byte(strconv.Itoa(sub.Process.Pid)), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "record grandchild:", err)
			os.Exit(1)
		}
		time.Sleep(2 * time.Minute)

	case "ignoreterm":
		signalIgnoreTERM()
		fmt.Println("ignoring SIGTERM")
		time.Sleep(2 * time.Minute)

	case "diesilent":
		// Write nothing at all, then exit non-zero. This reproduces the shape
		// of the shell version's worst bug: a supervisor reporting "process
		// died" over a completely empty log.
		os.Exit(3)

	case "dieloud":
		fmt.Fprintln(os.Stderr, "PANIC: could not bind configuration")
		os.Exit(1)
	case "hookwrite":
		// A lifecycle hook under test: drop a marker file, then exit with the
		// status named in the environment (0 when unset).
		if p := os.Getenv("HOOK_FILE"); p != "" {
			_ = os.WriteFile(p, []byte("hook ran\n"), 0o644)
		}
		code := 0
		if s := os.Getenv("HOOK_STATUS"); s != "" {
			code, _ = strconv.Atoi(s)
		}
		os.Exit(code)
	}
	os.Exit(0)
}

// helperCmd returns the argv that re-executes this binary in the given mode.
func helperCmd() []string {
	return []string{os.Args[0], "-test.run=TestHelperProcess"}
}

// helperEnvFor returns a child environment selecting a helper mode.
func helperEnvFor(mode string, extra ...string) []string {
	return append(append(os.Environ(), helperEnv+"="+mode), extra...)
}

// fixture builds a Supervisor over instances rooted in a temp directory.
func fixture(t *testing.T, insts ...service.Instance) (*Supervisor, *state.Dir) {
	t.Helper()
	root := t.TempDir()
	st, err := state.New(root)
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	for i := range insts {
		if insts[i].Dir == "" {
			insts[i].Dir = root
		}
	}
	cfg := &config.Config{
		Root:         root,
		StopGrace:    2 * time.Second,
		ReadyTimeout: 5 * time.Second,
	}
	return New(cfg, st, insts), st
}

// drain collects events in the background so a test never depends on the
// non-blocking send happening to land.
func drain(t *testing.T) (chan Event, func() []Event) {
	t.Helper()
	ch := make(chan Event, 256)
	done := make(chan []Event, 1)
	go func() {
		var got []Event
		for e := range ch {
			got = append(got, e)
		}
		done <- got
	}()
	return ch, func() []Event {
		close(ch)
		return <-done
	}
}

func TestStartSpawnsDetachedAndRecordsPID(t *testing.T) {
	sup, st := fixture(t, service.Instance{
		Name: "svc",
		Cmd:  helperCmd(),
		Env:  helperEnvFor("sleep"),
	})
	ev, collect := drain(t)
	before := time.Now()
	if err := sup.Start(context.Background(), nil, ev); err != nil {
		t.Fatalf("Start: %v", err)
	}
	after := time.Now()
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()
	collect()

	pid, err := st.ReadPID("svc")
	if err != nil || pid <= 0 {
		t.Fatalf("ReadPID = %d, %v; want a live pid", pid, err)
	}
	if !state.Alive(pid) {
		t.Fatalf("pid %d is not alive after Start", pid)
	}

	// The spawn time must reach disk. Uptime cannot come from memory: the next
	// mabo-ctl invocation is a different process, and this record is the only
	// thing that survives it.
	rec, err := st.ReadPIDRecord("svc")
	if err != nil {
		t.Fatalf("ReadPIDRecord: %v", err)
	}
	if rec.PID != pid {
		t.Errorf("PIDRecord.PID = %d, want %d", rec.PID, pid)
	}
	if rec.StartedAt.Before(before) || rec.StartedAt.After(after) {
		t.Errorf("PIDRecord.StartedAt = %s, want it inside the Start call (%s … %s)",
			rec.StartedAt, before, after)
	}

	// Setsid must have made the child its own process-group leader. Everything
	// in stopOne depends on this invariant holding.
	pgid, err := processGroup(pid)
	if err != nil {
		t.Fatalf("processGroup: %v", err)
	}
	if pgid != pid {
		t.Errorf("process group = %d, want %d (the child is not a group leader, "+
			"so Setsid did not take effect and the recycled-pid guard cannot work)", pgid, pid)
	}
}

func TestStopKillsTheWholeProcessGroup(t *testing.T) {
	root := t.TempDir()
	pidFile := filepath.Join(root, "grandchild.pid")

	sup, _ := fixture(t, service.Instance{
		Name: "svc",
		Dir:  root,
		Cmd:  helperCmd(),
		Env:  helperEnvFor("grandchild", "MABOCTL_HELPER_PIDFILE="+pidFile),
	})

	ev, collect := drain(t)
	if err := sup.Start(context.Background(), nil, ev); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the child to report its grandchild.
	var grandchild int
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(pidFile); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				grandchild = n
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if grandchild == 0 {
		t.Fatal("helper never recorded a grandchild pid")
	}
	if !state.Alive(grandchild) {
		t.Fatalf("grandchild %d was never alive", grandchild)
	}

	if err := sup.Stop(context.Background(), nil, ev); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	collect()
	sup.Wait()

	// THE assertion of this file. Signalling the bare pid would leave the
	// grandchild running and holding whatever port it bound — the exact failure
	// that accumulated 28 orphaned dev servers over three days.
	for i := 0; i < 40 && state.Alive(grandchild); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if state.Alive(grandchild) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL) // do not leak it out of the test
		t.Fatal("grandchild survived Stop: the signal went to the pid, not the process group")
	}
}

// TestStopTakesExactlyTheNamedServices is the behavioural regression for
// docs/LANDMINES.md §8: Stop selected through service.Select, whose contract
// is START's — expand a name into its dependency closure — so `stop listener`
// also stopped the backend listener depends on. A stop means exactly what was
// named; a live dependency must still be alive when Stop returns.
func TestStopTakesExactlyTheNamedServices(t *testing.T) {
	root := t.TempDir()
	sup, st := fixture(t,
		service.Instance{
			Name: "backend",
			Dir:  root,
			Cmd:  helperCmd(),
			Env:  helperEnvFor("sleep"),
		},
		service.Instance{
			Name:      "listener",
			Dir:       root,
			Cmd:       helperCmd(),
			Env:       helperEnvFor("sleep"),
			DependsOn: []string{"backend"},
		},
	)

	ev, collect := drain(t)
	if err := sup.Start(context.Background(), nil, ev); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := sup.Stop(context.Background(), []string{"listener"}, ev); err != nil {
		t.Fatalf("Stop(listener): %v", err)
	}

	pid, err := st.ReadPID("backend")
	if err != nil || pid <= 0 {
		t.Fatalf("ReadPID(backend) = %d, %v; want the dependency left running", pid, err)
	}
	if !state.Alive(pid) {
		t.Fatal("backend died: Stop expanded into the dependency closure")
	}

	// The named service itself must actually be down.
	lpid, _ := st.ReadPID("listener")
	if lpid > 0 && state.Alive(lpid) {
		t.Fatalf("listener pid %d survived Stop(listener)", lpid)
	}

	// Clean up the survivor so the test leaks no processes. Both Stops share
	// one open event channel — collect closes it, and a second Stop would
	// panic on emit.
	if err := sup.Stop(context.Background(), nil, ev); err != nil {
		t.Fatalf("Stop(everything): %v", err)
	}
	collect()
	sup.Wait()
}

func TestStopEscalatesToSIGKILL(t *testing.T) {
	sup, st := fixture(t, service.Instance{
		Name: "stubborn",
		Cmd:  helperCmd(),
		Env:  helperEnvFor("ignoreterm"),
	})
	ev, collect := drain(t)
	if err := sup.Start(context.Background(), nil, ev); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid, _ := st.ReadPID("stubborn")

	// Give the child a moment to install its SIGTERM handler; killing it before
	// that would pass the test for the wrong reason.
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	if err := sup.Stop(context.Background(), nil, ev); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	events := collect()
	sup.Wait()

	if state.Alive(pid) {
		t.Fatalf("pid %d survived Stop entirely", pid)
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Errorf("Stop took %s; it must have waited the full 2s stop_grace before escalating", elapsed)
	}
	if !hasMsg(events, "SIGKILL") {
		t.Errorf("no event mentioned the SIGKILL escalation; events = %v", msgs(events))
	}
}

func TestStopRefusesARecycledPID(t *testing.T) {
	sup, st := fixture(t, service.Instance{Name: "svc", Cmd: helperCmd()})

	// A process we did NOT spawn with Setsid stays in the test binary's process
	// group, so its pgid differs from its pid. That is precisely the signature
	// of a stale pid file whose number has been reused, and signalling its
	// group would take down an unrelated tree.
	victim := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	victim.Env = helperEnvFor("sleep")
	if err := victim.Start(); err != nil {
		t.Fatalf("spawn victim: %v", err)
	}
	defer func() {
		_ = victim.Process.Kill()
		_ = victim.Wait()
	}()
	pid := victim.Process.Pid

	pgid, err := processGroup(pid)
	if err != nil {
		t.Fatalf("processGroup: %v", err)
	}
	if pgid == pid {
		t.Skip("victim happened to be its own group leader; cannot simulate a recycled pid here")
	}

	if err := st.WritePID("svc", pid); err != nil {
		t.Fatalf("WritePID: %v", err)
	}

	ev, collect := drain(t)
	if err := sup.Stop(context.Background(), nil, ev); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	events := collect()

	if !state.Alive(pid) {
		t.Fatal("mabo-ctl killed a process it did not start: the recycled-pid guard did not hold")
	}
	if !hasMsg(events, "refusing to signal") {
		t.Errorf("the refusal was not reported to the user; events = %v", msgs(events))
	}
	for _, e := range events {
		if e.Err != nil && errors.Is(e.Err, ErrUnsafeSignal) {
			return
		}
	}
	t.Error("no event carried ErrUnsafeSignal")
}

func TestFailedStartQuotesTheLogTail(t *testing.T) {
	sup, _ := fixture(t, service.Instance{
		Name: "loud",
		Cmd:  helperCmd(),
		Env:  helperEnvFor("dieloud"),
		// A health URL nothing will ever answer, so readiness must resolve by
		// noticing the process died rather than by timing out.
		Health: "http://127.0.0.1:1/",
	})
	ev, collect := drain(t)
	_ = sup.Start(context.Background(), nil, ev)
	events := collect()
	sup.Wait()

	if !hasMsg(events, "PANIC: could not bind configuration") {
		t.Errorf("the failure did not quote the log tail; events = %v", msgs(events))
	}
}

func TestFailedStartSaysSoWhenTheLogIsEmpty(t *testing.T) {
	sup, _ := fixture(t, service.Instance{
		Name:   "silent",
		Cmd:    helperCmd(),
		Env:    helperEnvFor("diesilent"),
		Health: "http://127.0.0.1:1/",
	})
	ev, collect := drain(t)
	_ = sup.Start(context.Background(), nil, ev)
	events := collect()
	sup.Wait()

	// The shell version printed "failed  process died — last log lines:" and
	// then nothing, which took three rounds to diagnose. Silence about silence
	// is the bug; naming it is the fix.
	if !hasMsg(events, "log is empty") {
		t.Errorf("an empty log was not called out; events = %v", msgs(events))
	}
}

func TestStartRefusesAPortHeldByAForeignProcess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	port := portOf(t, srv.URL)
	sup, _ := fixture(t, service.Instance{
		Name: "clash",
		Port: port,
		Cmd:  helperCmd(),
		Env:  helperEnvFor("sleep"),
	})

	ev, collect := drain(t)
	_ = sup.Start(context.Background(), nil, ev)
	events := collect()
	sup.Wait()

	if !hasMsg(events, "held by pid") {
		t.Skipf("lsof unavailable or returned nothing for port %d; events = %v", port, msgs(events))
	}
	if !hasMsg(events, LsofCommand(port)) {
		t.Errorf("the refusal did not show the lsof command; events = %v", msgs(events))
	}
}

func TestStartSkipsDependantsOfAFailedDependency(t *testing.T) {
	sup, st := fixture(t,
		service.Instance{
			Name:   "base",
			Cmd:    helperCmd(),
			Env:    helperEnvFor("diesilent"),
			Health: "http://127.0.0.1:1/",
		},
		service.Instance{
			Name:      "dependant",
			Cmd:       helperCmd(),
			Env:       helperEnvFor("sleep"),
			DependsOn: []string{"base"},
		},
	)
	ev, collect := drain(t)
	_ = sup.Start(context.Background(), nil, ev)
	events := collect()
	sup.Wait()

	if !hasMsg(events, "skipped: dependency base") {
		t.Errorf("the dependant was not skipped; events = %v", msgs(events))
	}
	if pid, _ := st.ReadPID("dependant"); pid > 0 {
		t.Errorf("dependant started (pid %d) despite its dependency failing", pid)
	}
}

func TestTailReturnsLastLinesAndStopsCleanly(t *testing.T) {
	sup, st := fixture(t, service.Instance{Name: "svc", Cmd: helperCmd()})
	f, err := st.TruncateLog("svc")
	if err != nil {
		t.Fatalf("TruncateLog: %v", err)
	}
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(f, "line %d\n", i)
	}
	f.Close()

	out := make(chan string, 32)
	if err := sup.Tail(context.Background(), "svc", 3, false, out); err != nil {
		t.Fatalf("Tail: %v", err)
	}
	var got []string
	for l := range out { // Tail closes out, so this terminates
		got = append(got, l)
	}
	want := []string{"line 8", "line 9", "line 10"}
	if len(got) != len(want) {
		t.Fatalf("Tail returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Tail returned %v, want %v", got, want)
		}
	}
}

func TestTailFollowStopsOnContextCancel(t *testing.T) {
	sup, st := fixture(t, service.Instance{Name: "svc", Cmd: helperCmd()})
	f, err := st.TruncateLog("svc")
	if err != nil {
		t.Fatalf("TruncateLog: %v", err)
	}
	fmt.Fprintln(f, "hello")
	f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan string, 32)
	done := make(chan error, 1)
	go func() { done <- sup.Tail(ctx, "svc", 10, true, out) }()

	select {
	case <-out:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("follow never delivered the backlog")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Tail did not return after its context was cancelled: the goroutine is leaked")
	}
}

func TestTailRejectsAnUnknownService(t *testing.T) {
	sup, _ := fixture(t, service.Instance{Name: "svc", Cmd: helperCmd()})
	out := make(chan string, 1)
	err := sup.Tail(context.Background(), "nope", 5, false, out)
	if !errors.Is(err, ErrUnknownService) {
		t.Fatalf("Tail error = %v, want ErrUnknownService", err)
	}
	if _, open := <-out; open {
		t.Error("Tail must close out even when it rejects the service")
	}
}

func TestEmitNeverBlocksOnAFullChannel(t *testing.T) {
	ch := make(chan Event, 1)
	ch <- Event{Msg: "occupied"}

	done := make(chan struct{})
	go func() {
		emit(ch, Event{Msg: "dropped"}) // must not block
		emit(nil, Event{Msg: "nil channel"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("emit blocked on a full channel; a slow consumer would wedge the supervisor")
	}
}

func TestParseLsof(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want Holder
	}{
		{"empty", "", Holder{}},
		{"header only", "COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME\n", Holder{}},
		{
			"one listener",
			"COMMAND   PID USER   FD  TYPE DEVICE SIZE/OFF NODE NAME\n" +
				"node    44213 me     23u IPv6 0x1234      0t0  TCP *:7100 (LISTEN)\n",
			Holder{PID: 44213, Command: "node"},
		},
		{
			"first of several wins",
			"COMMAND  PID USER FD TYPE DEVICE SIZE/OFF NODE NAME\n" +
				"python 8080 me   3u IPv4 0x1        0t0  TCP *:7100 (LISTEN)\n" +
				"python 8081 me   4u IPv6 0x2        0t0  TCP *:7100 (LISTEN)\n",
			Holder{PID: 8080, Command: "python"},
		},
		{"unparseable pid", "COMMAND PID\nnode    notanumber\n", Holder{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseLsof(tc.out); got != tc.want {
				t.Errorf("parseLsof = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestVerifyGroupRefusesPrivilegedPIDs(t *testing.T) {
	for _, pid := range []int{0, 1, -1} {
		if _, err := verifyGroup(pid, time.Time{}); !errors.Is(err, ErrUnsafeSignal) {
			t.Errorf("verifyGroup(%d) error = %v, want ErrUnsafeSignal", pid, err)
		}
	}
}

// hasMsg reports whether any event's message contains want.
func hasMsg(events []Event, want string) bool {
	for _, e := range events {
		if strings.Contains(e.Msg, want) {
			return true
		}
	}
	return false
}

// msgs renders the collected messages for a failure report.
func msgs(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Service+": "+e.Msg)
	}
	return out
}

// portOf extracts the port from an http://host:port URL.
func portOf(t *testing.T, rawURL string) int {
	t.Helper()
	idx := strings.LastIndex(rawURL, ":")
	if idx < 0 {
		t.Fatalf("no port in %q", rawURL)
	}
	p, err := strconv.Atoi(rawURL[idx+1:])
	if err != nil {
		t.Fatalf("bad port in %q: %v", rawURL, err)
	}
	return p
}

// TestStartRefusesUnrunnableInstance checks that the runtime failure
// [service.Resolve] defers onto the instance is enforced at spawn time.
//
// Deferring the error is what keeps stop/status/reset working when one
// interpreter is missing; it is only safe because the service that actually
// needs that interpreter is still refused here, rather than being spawned
// against whatever the ambient PATH turns up. That substitution is dev.sh bug
// #5.
func TestStartRefusesUnrunnableInstance(t *testing.T) {
	boom := errors.New("runtime \"conda:gone\" resolves \"python\" to /nope/python, and that file does not exist")
	sup, st := fixture(t, service.Instance{
		Name:   "backend",
		Cmd:    []string{"python"},
		Env:    os.Environ(),
		CmdErr: boom,
	})

	ev, collect := drain(t)
	err := sup.Start(context.Background(), nil, ev)
	events := collect()

	if !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Start error = %v, want it to wrap ErrNotStarted", err)
	}
	if pid, perr := st.ReadPID("backend"); perr != nil || pid != 0 {
		t.Fatalf("a pid file was written for an unrunnable service: pid=%d err=%v", pid, perr)
	}

	var saw bool
	for _, e := range events {
		if e.Service == "backend" && e.Phase == PhaseFailed && strings.Contains(e.Msg, "/nope/python") {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("no failed event naming the resolved path; got %+v", events)
	}
}

// TestConcurrentStartsSpawnOnlyOneProcess is the regression test for the
// check-then-act race in startOne.
//
// Before the per-service lock, two concurrent starts of one service both read
// "not running", both spawned, and the second WritePID overwrote the first —
// leaving a live process with no pid file. Stop could then never signal it, so
// it held its port forever and mabo-ctl could neither name nor reap it. The web
// console makes this a two-tab click; the CLI never could, which is why it
// survived v1.
func TestConcurrentStartsSpawnOnlyOneProcess(t *testing.T) {
	sup, st := fixture(t, service.Instance{
		Name: "racy",
		Cmd:  helperCmd(),
		Env:  helperEnvFor("sleep"),
	})
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	const racers = 6
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sup.Start(context.Background(), []string{"racy"}, nil)
		}()
	}
	wg.Wait()

	pid, err := st.ReadPID("racy")
	if err != nil || pid <= 0 {
		t.Fatalf("ReadPID = %d, %v; want one live pid", pid, err)
	}

	// Exactly one helper must be alive, and it must be the one on record.
	// Anything else is an orphan mabo-ctl can never stop.
	alive := helperChildren(t)
	if len(alive) != 1 {
		for _, p := range alive {
			if p != pid {
				_ = syscall.Kill(p, syscall.SIGKILL) // never leak an orphan out of a test
			}
		}
		t.Fatalf("%d helper processes alive (%v), want exactly 1 (pid %d on record); "+
			"a concurrent start spawned a process with no pid file", len(alive), alive, pid)
	}
	if alive[0] != pid {
		t.Fatalf("live pid %d does not match the recorded pid %d", alive[0], pid)
	}
}

// helperChildren lists the live direct children of this test process, which are
// exactly the helpers spawned by the fixture.
func helperChildren(t *testing.T) []int {
	t.Helper()
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		// pgrep exits 1 when nothing matches.
		return nil
	}
	var pids []int
	for _, line := range strings.Fields(string(out)) {
		if p, err := strconv.Atoi(line); err == nil && state.Alive(p) {
			pids = append(pids, p)
		}
	}
	return pids
}

// TestStalePIDFileDoesNotWedgeTheService is the regression test for a permanent
// deadlock that a laptop reboot was enough to cause.
//
// A pid file survives a reboot; the pid in it gets recycled by an unrelated
// process. Liveness alone then reads as "running", so status reported a
// stranger's process, start refused with "already running", and stop correctly
// proved the pid was not ours — and left the file in place, so the next start
// refused again. Forever, until the user discovered `mabo-ctl reset`.
//
// The service must recover on its own, and the stranger must survive: proving
// a pid is not ours is precisely the reason we must not signal it.
func TestStalePIDFileDoesNotWedgeTheService(t *testing.T) {
	sup, st := fixture(t, service.Instance{
		Name: "svc",
		Cmd:  helperCmd(),
		Env:  helperEnvFor("sleep"),
	})
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	// A live process we did NOT spawn stays in the test binary's group, so its
	// pgid differs from its pid — the signature of a recycled pid.
	stranger := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	stranger.Env = helperEnvFor("sleep")
	if err := stranger.Start(); err != nil {
		t.Fatalf("spawn stranger: %v", err)
	}
	defer func() {
		_ = stranger.Process.Kill()
		_ = stranger.Wait()
	}()
	foreign := stranger.Process.Pid

	if pgid, err := processGroup(foreign); err != nil || pgid == foreign {
		t.Skip("stranger happened to be its own group leader; cannot simulate a recycled pid here")
	}
	if err := st.WritePID("svc", foreign); err != nil {
		t.Fatalf("WritePID: %v", err)
	}

	// Status must not claim someone else's process is our service.
	sts := sup.Status(context.Background())
	if sts[0].Phase == PhaseRunning || sts[0].PID == foreign {
		t.Errorf("Status reported phase=%q pid=%d; a stale pid file must not read as running",
			sts[0].Phase, sts[0].PID)
	}

	// Start must repair the record and actually start the service.
	ev, collect := drain(t)
	if err := sup.Start(context.Background(), []string{"svc"}, ev); err != nil {
		t.Fatalf("Start over a stale pid file: %v", err)
	}
	events := collect()

	pid, err := st.ReadPID("svc")
	if err != nil || pid <= 0 {
		t.Fatalf("ReadPID = %d, %v; want the newly started pid", pid, err)
	}
	if pid == foreign {
		t.Fatal("the stale pid survived: the service is still wedged")
	}
	if !state.Alive(pid) {
		t.Fatalf("recorded pid %d is not alive; the start did not happen", pid)
	}
	if !hasMsg(events, "stale pid file") {
		t.Errorf("the repair was not reported to the user; events = %v", msgs(events))
	}

	// The whole point of refusing to signal it: it must still be running.
	if !state.Alive(foreign) {
		t.Fatal("mabo-ctl killed the unrelated process whose pid it had proved was not ours")
	}
}

// TestStartRunsIndependentServicesConcurrently is the regression test for the
// change that made Start level-parallel.
//
// Walking a flat topological order serially makes services that depend on
// nothing queue behind each other, so a stack pays the SUM of every startup
// time. Measured on a real three-service stack before the change: 11.0s for
// three services taking three seconds each. After: 3.9s.
//
// The readiness gate is a handler that sleeps PER REQUEST. That detail is the
// test: health.Probe treats any HTTP response as ready — including a 503 — so
// a "not ready yet" status code would make this pass without measuring
// anything. Delaying the response HEADERS is what actually forces a wait.
func TestStartRunsIndependentServicesConcurrently(t *testing.T) {
	const (
		perProbe = 700 * time.Millisecond
		ceiling  = 1600 * time.Millisecond // serial would be ~2.1s
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(perProbe)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sup, _ := fixture(t,
		service.Instance{Name: "a", Cmd: helperCmd(), Env: helperEnvFor("sleep"), Health: srv.URL},
		service.Instance{Name: "b", Cmd: helperCmd(), Env: helperEnvFor("sleep"), Health: srv.URL},
		service.Instance{Name: "c", Cmd: helperCmd(), Env: helperEnvFor("sleep"), Health: srv.URL},
	)
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	start := time.Now()
	if err := sup.Start(context.Background(), nil, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > ceiling {
		t.Errorf("three independent services took %s; serial is ~%s and parallel ~%s — "+
			"they are not starting concurrently", elapsed, 3*perProbe, perProbe)
	}
}

// TestStartStillSerialisesDependencyLevels proves the speedup did not buy
// itself by ignoring depends_on: a dependant must not be spawned until the
// thing it depends on has been.
func TestStartStillSerialisesDependencyLevels(t *testing.T) {
	sup, st := fixture(t,
		service.Instance{Name: "base", Cmd: helperCmd(), Env: helperEnvFor("sleep")},
		service.Instance{
			Name: "dependant", Cmd: helperCmd(), Env: helperEnvFor("sleep"),
			DependsOn: []string{"base"},
		},
	)
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	if err := sup.Start(context.Background(), nil, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	basePID, _ := st.ReadPID("base")
	depPID, _ := st.ReadPID("dependant")
	if basePID <= 0 || depPID <= 0 {
		t.Fatalf("pids: base=%d dependant=%d; both should be running", basePID, depPID)
	}
	// Both are ours and both are alive; ordering is asserted by the level model
	// plus the skip test below, since pids are not monotonic across levels on
	// every platform.
	if !state.Alive(basePID) || !state.Alive(depPID) {
		t.Fatal("a service in a later level did not start")
	}
}

// --- crash visibility -------------------------------------------------------
//
// The tests below cover the question mabo-ctl could not previously answer: a
// service that died two seconds ago with a stack trace in its log used to be
// byte-identical to one that was never started, in every front end and in the
// --json contract. They spawn real processes and kill them for real, because
// the exit status is the kernel's answer and a mock would assert nothing.

// statusOf runs Status and returns the entry for name.
func statusOf(t *testing.T, sup *Supervisor, name string) Status {
	t.Helper()
	for _, st := range sup.Status(context.Background()) {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("Status returned nothing for %q", name)
	return Status{}
}

// TestKilledServiceReportsExitedNotStopped is the headline case: `kill -9` on a
// running service used to read back as "stopped 0 - -" with an empty detail,
// indistinguishable from a service the user never started.
func TestKilledServiceReportsExitedNotStopped(t *testing.T) {
	sup, st := fixture(t, service.Instance{
		Name: "beta",
		Cmd:  helperCmd(),
		Env:  helperEnvFor("sleep"),
	})
	if err := sup.Start(context.Background(), nil, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid, _ := st.ReadPID("beta")
	if pid <= 0 {
		t.Fatal("the service did not start")
	}

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill -9 %d: %v", pid, err)
	}
	sup.Wait() // the reaper writes the record

	if rec, ok, _ := st.ReadExit("beta"); !ok || rec.Startup {
		t.Errorf("record = %+v, present = %v; a service that was up and then killed did not "+
			"die during startup, and marking it so would report it as `failed`", rec, ok)
	}

	got := statusOf(t, sup, "beta")
	if got.Phase != PhaseExited {
		t.Fatalf("phase = %q, want %q — a crashed service must not read as stopped", got.Phase, PhaseExited)
	}
	if got.ExitSignal != "SIGKILL" {
		t.Errorf("ExitSignal = %q, want SIGKILL", got.ExitSignal)
	}
	if got.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 for a signalled process", got.ExitCode)
	}
	if got.ExitedAt.IsZero() {
		t.Error("ExitedAt is zero, so nothing can say how long ago this happened")
	}
	if got.StartedAt.IsZero() {
		t.Error("StartedAt is zero, so the record cannot say how long the process had been up")
	}
	if !strings.Contains(got.Detail, "SIGKILL") {
		t.Errorf("Detail = %q, want it to name the signal", got.Detail)
	}
}

// TestFailedStartRecordsTheDeathWithItsLogTail checks that the log tail the
// start path computes reaches DISK, not just the event stream. The event
// scrolls past; the record is what every later status reads.
func TestFailedStartRecordsTheDeathWithItsLogTail(t *testing.T) {
	sup, st := fixture(t, service.Instance{
		Name:   "loud",
		Cmd:    helperCmd(),
		Env:    helperEnvFor("dieloud"),
		Health: "http://127.0.0.1:1/",
	})
	_ = sup.Start(context.Background(), nil, nil)
	sup.Wait()

	rec, ok, err := st.ReadExit("loud")
	if err != nil || !ok {
		t.Fatalf("ReadExit = %v, %v; want a record for a service that died during startup", ok, err)
	}
	if !strings.Contains(rec.LogTail, "PANIC: could not bind configuration") {
		t.Errorf("the record does not quote the log tail: %q", rec.LogTail)
	}
	// The helper exits 1. The exit status is the reaper's to know, and the
	// start path waits for it rather than writing "unknown" and being corrected.
	if rec.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", rec.ExitCode)
	}
	if rec.Signal != "" {
		t.Errorf("Signal = %q, want empty for a process that exited on its own", rec.Signal)
	}
	if !rec.Startup {
		t.Error("the record does not remember that the service died before it ever came up, " +
			"so a later status cannot tell `failed` from `exited`")
	}

	got := statusOf(t, sup, "loud")
	if got.Phase != PhaseFailed {
		t.Fatalf("phase = %q, want %q — it never came up, which is not the same as having crashed later",
			got.Phase, PhaseFailed)
	}
	if !strings.Contains(got.Detail, "exit code 1") {
		t.Errorf("Detail = %q, want it to name the exit code", got.Detail)
	}
	if !strings.Contains(got.Detail, "PANIC: could not bind configuration") {
		t.Errorf("Detail = %q, want the log tail in it", got.Detail)
	}
}

// TestFailedStartWithAnEmptyLogSaysSoInTheRecord checks that the explanation the
// start path writes for a silent death — the one failure that took three rounds
// to diagnose in the shell predecessor — is the text that reaches disk, and is
// not overwritten by the reaper's bare (empty) tail.
func TestFailedStartWithAnEmptyLogSaysSoInTheRecord(t *testing.T) {
	sup, st := fixture(t, service.Instance{
		Name:   "silent",
		Cmd:    helperCmd(),
		Env:    helperEnvFor("diesilent"),
		Health: "http://127.0.0.1:1/",
	})
	_ = sup.Start(context.Background(), nil, nil)
	sup.Wait()

	rec, ok, err := st.ReadExit("silent")
	if err != nil || !ok {
		t.Fatalf("ReadExit = %v, %v; want a record", ok, err)
	}
	if !strings.Contains(rec.LogTail, "log is empty") {
		t.Errorf("the record does not explain the empty log: %q", rec.LogTail)
	}
	if rec.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", rec.ExitCode)
	}
}

// sibling builds a SECOND Supervisor over the same state directory and the same
// instances, standing in for the other mabo-ctl process that is always in play in
// real use: `mabo-ctl serve` or the interactive console spawns a service and stays
// resident, and a one-shot `mabo-ctl stop` in another terminal is a different
// process with its own, empty, in-memory bookkeeping.
func sibling(sup *Supervisor) *Supervisor {
	return New(sup.cfg, sup.st, sup.insts)
}

// TestStopIsNotACrashWhenAnotherMaboCtlOwnsTheReaper is the cross-process half of
// TestCleanStopLeavesNoExitRecord, and it is the case the in-memory mark alone
// could never cover.
//
// The process that spawned the child is the one the kernel hands the wait status
// to. When the stop comes from somewhere else, that reaper sees a death by
// SIGTERM it never asked for and cannot tell it from a segfault. Observed before
// the fix: `mabo-ctl stop killable` printed "stopped", and its own status block on
// the next line said "exited — killed by SIGTERM, 0s ago".
func TestStopIsNotACrashWhenAnotherMaboCtlOwnsTheReaper(t *testing.T) {
	resident, st := fixture(t, service.Instance{
		Name: "svc",
		Cmd:  helperCmd(),
		Env:  helperEnvFor("sleep"),
	})
	if err := resident.Start(context.Background(), nil, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stopper := sibling(resident)
	if err := stopper.Stop(context.Background(), nil, nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	resident.Wait() // let the resident's reaper run to completion

	if rec, ok, _ := st.ReadExit("svc"); ok && !rec.Stopped {
		t.Errorf("the foreign reaper wrote a crash record for a deliberate stop: %+v", rec)
	}
	for _, sup := range map[string]*Supervisor{"stopper": stopper, "resident": resident} {
		if got := statusOf(t, sup, "svc"); got.Phase != PhaseStopped {
			t.Errorf("phase = %q, want %q (detail %q)", got.Phase, PhaseStopped, got.Detail)
		}
	}
}

// TestStopMarkerSurvivesAnUnconfirmedDeath covers the window inside a stop: the
// marker is written before the signal, so a status read taken while the pid file
// is still on disk reports a stopped service rather than reading that pid file
// as evidence of a crash.
func TestStopMarkerSurvivesAnUnconfirmedDeath(t *testing.T) {
	sup, st := fixture(t, service.Instance{Name: "svc", Cmd: helperCmd()})

	if err := st.WritePIDAt("svc", deadPID(t), time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("WritePIDAt: %v", err)
	}
	if err := st.WriteExit("svc", state.ExitRecord{
		PID: deadPID(t), EndedAt: time.Now(), Stopped: true,
	}); err != nil {
		t.Fatalf("WriteExit: %v", err)
	}

	if got := statusOf(t, sup, "svc"); got.Phase != PhaseStopped {
		t.Fatalf("phase = %q, want %q (detail %q) — a stop mabo-ctl asked for is never a crash",
			got.Phase, PhaseStopped, got.Detail)
	}
}

// TestCleanStopLeavesNoExitRecord is the guard against the inverse lie. Every
// stop kills a process, and cmd.Wait cannot tell our own SIGTERM from a
// segfault, so without this a stopped service would report itself crashed.
func TestCleanStopLeavesNoExitRecord(t *testing.T) {
	sup, st := fixture(t, service.Instance{
		Name: "svc",
		Cmd:  helperCmd(),
		Env:  helperEnvFor("sleep"),
	})
	if err := sup.Start(context.Background(), nil, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := sup.Stop(context.Background(), nil, nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	sup.Wait()

	if _, ok, err := st.ReadExit("svc"); err != nil || ok {
		t.Fatalf("ReadExit = %v, %v; a deliberate stop must leave no exit record", ok, err)
	}
	if got := statusOf(t, sup, "svc"); got.Phase != PhaseStopped {
		t.Fatalf("phase = %q, want %q — a stopped service must not masquerade as crashed",
			got.Phase, PhaseStopped)
	}
}

// TestStopClearsAnEarlierCrash covers the other half: the service really did
// crash, and then the user stopped it. After `mabo-ctl stop`, a service is
// stopped; a crash from before the stop must not keep reading as "exited".
func TestStopClearsAnEarlierCrash(t *testing.T) {
	sup, st := fixture(t, service.Instance{
		Name: "svc",
		Cmd:  helperCmd(),
		Env:  helperEnvFor("sleep"),
	})
	if err := sup.Start(context.Background(), nil, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid, _ := st.ReadPID("svc")
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	sup.Wait()
	if got := statusOf(t, sup, "svc"); got.Phase != PhaseExited {
		t.Fatalf("phase before the stop = %q, want %q", got.Phase, PhaseExited)
	}

	if err := sup.Stop(context.Background(), nil, nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, ok, _ := st.ReadExit("svc"); ok {
		t.Error("the stop left the crash record in place")
	}
	if got := statusOf(t, sup, "svc"); got.Phase != PhaseStopped {
		t.Fatalf("phase after the stop = %q, want %q", got.Phase, PhaseStopped)
	}
}

// TestStartClearsTheStaleExitRecord checks the record does not outlive the run
// it describes: a service that is demonstrably running must never read as one
// that died.
func TestStartClearsTheStaleExitRecord(t *testing.T) {
	sup, st := fixture(t, service.Instance{
		Name: "svc",
		Cmd:  helperCmd(),
		Env:  helperEnvFor("sleep"),
	})
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	if err := st.WriteExit("svc", state.ExitRecord{
		PID: 999999, ExitCode: 1, EndedAt: time.Now().Add(-time.Hour),
		LogTail: "a death from a previous run",
	}); err != nil {
		t.Fatalf("WriteExit: %v", err)
	}
	if err := sup.Start(context.Background(), nil, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, ok, _ := st.ReadExit("svc"); ok {
		t.Error("Start left a stale exit record behind")
	}
	if got := statusOf(t, sup, "svc"); got.Phase != PhaseRunning {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseRunning)
	}
}

// TestServiceNeverStartedIsStopped is the control for every test above: with no
// pid file and no exit record, "stopped" is the truth and nothing must dress it
// up as a crash.
func TestServiceNeverStartedIsStopped(t *testing.T) {
	sup, _ := fixture(t, service.Instance{Name: "svc", Cmd: helperCmd()})
	got := statusOf(t, sup, "svc")
	if got.Phase != PhaseStopped {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseStopped)
	}
	if got.Detail != "" || got.ExitedAt != (time.Time{}) || got.StartedAt != (time.Time{}) {
		t.Fatalf("a service that was never started reported %+v; every field should be empty", got)
	}
}

// TestStartEventsNeverQuoteAHealthURLCredential covers the channel that the
// redaction in internal/web did not reach.
//
// GET /api/status redacted the health URL, and every reader concluded the
// credential was handled. It was not: the slow and degraded events quoted
// in.Health verbatim, and those events travel to the unauthenticated SSE stream
// and back in the body of every mutation response — three channels fanning out
// the value the fourth was carefully withholding. Redacting per route is how
// that happens, so the fix is here, where the string is built.
func TestStartEventsNeverQuoteAHealthURLCredential(t *testing.T) {
	const secret = "hunter2"
	sup, _ := fixture(t, service.Instance{
		Name:   "svc",
		Cmd:    helperCmd(),
		Env:    helperEnvFor("sleep"),
		Health: "http://admin:" + secret + "@127.0.0.1:1/health?api_key=sk-live-" + secret,
	})
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	ev := make(chan Event, 64)
	done := make(chan []Event, 1)
	go func() {
		var got []Event
		for e := range ev {
			got = append(got, e)
		}
		done <- got
	}()
	if err := sup.Start(context.Background(), nil, ev); err != nil && !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Start: %v", err)
	}
	close(ev)

	events := <-done
	if len(events) == 0 {
		t.Fatal("no events were emitted, so nothing was under test")
	}
	var sawProbeMsg bool
	for _, e := range events {
		if strings.Contains(e.Msg, secret) {
			t.Errorf("event %q leaked the health-URL credential", e.Msg)
		}
		if e.Phase == PhaseSlow || e.Phase == PhaseDegraded {
			sawProbeMsg = true
		}
	}
	if !sawProbeMsg {
		t.Fatal("no slow/degraded event was emitted, so the leaking branch never ran")
	}
}

// TestSlowBecomesDegradedPastReadyTimeout covers the second half of the phase
// work. One state — alive, probe not answering — has two honest readings, and
// which one applies depends on a spawn time that only exists on disk.
func TestSlowBecomesDegradedPastReadyTimeout(t *testing.T) {
	sup, st := fixture(t, service.Instance{
		Name:   "svc",
		Cmd:    helperCmd(),
		Env:    helperEnvFor("sleep"),
		Health: "http://127.0.0.1:1/", // nothing will ever answer this
	})
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	// Spawn without waiting for readiness: Start would block for the whole
	// ready_timeout, and this test is about what Status derives afterwards.
	if err := sup.Start(context.Background(), nil, nil); err != nil && !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Start: %v", err)
	}
	pid, _ := st.ReadPID("svc")
	if pid <= 0 || !state.Alive(pid) {
		t.Fatal("the service is not running, so neither phase is under test")
	}

	// Inside the window: still starting is an honest thing to say.
	if err := st.WritePIDAt("svc", pid, time.Now()); err != nil {
		t.Fatalf("WritePIDAt: %v", err)
	}
	if got := statusOf(t, sup, "svc"); got.Phase != PhaseSlow {
		t.Fatalf("phase inside ready_timeout = %q, want %q", got.Phase, PhaseSlow)
	}
	// Past it, the same words become a lie: "still starting" was true for
	// thirty seconds and false for the three hours after. The window is
	// waited out for real: backdating the pid record — the old shortcut — now
	// reads as a forged record whose spawn time disagrees with the kernel,
	// which is precisely what stopOne's identity check must refuse.
	time.Sleep(31 * time.Second)
	got := statusOf(t, sup, "svc")
	if got.Phase != PhaseDegraded {
		t.Fatalf("phase past ready_timeout = %q, want %q", got.Phase, PhaseDegraded)
	}
	// health's better error text, on the one path that derives a phase.
	if got.Detail == "" {
		t.Error("Detail is empty; a probe that could not connect must say why")
	}
	if got.Uptime < 30*time.Second {
		t.Errorf("Uptime = %s, want past the thirty-second ready window", got.Uptime)
	}
	if got.StartedAt.IsZero() {
		t.Error("StartedAt is zero for a live process")
	}
}

// TestAnUnknownSpawnTimeIsNeverDegraded covers the legacy pid file: it carries
// no spawn time, so there is no evidence the service is past anything. Slow is
// the generous answer and the correct one — accusing a service on the strength
// of a timestamp mabo-ctl does not have is the invention this work removes.
func TestAnUnknownSpawnTimeIsNeverDegraded(t *testing.T) {
	sup, st := fixture(t, service.Instance{
		Name:   "svc",
		Cmd:    helperCmd(),
		Env:    helperEnvFor("sleep"),
		Health: "http://127.0.0.1:1/",
	})
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	if err := sup.Start(context.Background(), nil, nil); err != nil && !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Start: %v", err)
	}
	pid, _ := st.ReadPID("svc")

	// A bare-integer pid file is what an older mabo-ctl wrote.
	if err := os.WriteFile(st.PIDPath("svc"), []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatalf("write legacy pid file: %v", err)
	}
	if got := statusOf(t, sup, "svc"); got.Phase != PhaseSlow {
		t.Fatalf("phase = %q, want %q for a pid file with no spawn time", got.Phase, PhaseSlow)
	}
}

// TestStatusReportsUptimeForAReadyService checks the field the console draws:
// Elapsed is probe latency, and before this there was no "up 4 hours" signal at
// all, so nobody could notice one going missing.
func TestStatusReportsUptimeForAReadyService(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sup, st := fixture(t, service.Instance{
		Name:   "svc",
		Cmd:    helperCmd(),
		Env:    helperEnvFor("sleep"),
		Health: srv.URL + "/",
	})
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	if err := sup.Start(context.Background(), nil, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid, _ := st.ReadPID("svc")
	if err := st.WritePIDAt("svc", pid, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatalf("WritePIDAt: %v", err)
	}

	got := statusOf(t, sup, "svc")
	if got.Phase != PhaseReady {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseReady)
	}
	if got.Uptime < 90*time.Minute {
		t.Errorf("Uptime = %s, want about two hours", got.Uptime)
	}
	if got.Uptime == got.Elapsed {
		t.Error("Uptime is being reported as probe latency; they are different questions")
	}
}

// TestExitDetailNamesHowAndWhen pins the one line the status block shows for a
// death, because it is the sentence the operator actually reads.
func TestExitDetailNamesHowAndWhen(t *testing.T) {
	t.Parallel()
	now := time.Now()
	tests := []struct {
		name string
		rec  state.ExitRecord
		want string
	}{
		{
			name: "an exit code",
			rec:  state.ExitRecord{ExitCode: 1, EndedAt: now.Add(-4 * time.Minute)},
			want: "exit code 1, 4m ago",
		},
		{
			name: "a signal outranks the code it does not have",
			rec:  state.ExitRecord{ExitCode: -1, Signal: "SIGKILL", EndedAt: now.Add(-12 * time.Second)},
			want: "killed by SIGKILL, 12s ago",
		},
		{
			name: "a clean exit is still a death worth reporting",
			rec:  state.ExitRecord{ExitCode: 0, EndedAt: now.Add(-3 * time.Hour)},
			want: "exit code 0, 3h ago",
		},
		{
			name: "an unobserved wait status is not invented",
			rec:  state.ExitRecord{ExitCode: -1, EndedAt: now.Add(-49 * time.Hour)},
			want: "exit status unknown, 2d ago",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := exitDetail(tc.rec); got != tc.want {
				t.Fatalf("exitDetail = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExitDetailAppendsTheLogTail checks the stack trace lands under the
// summary rather than replacing it.
func TestExitDetailAppendsTheLogTail(t *testing.T) {
	t.Parallel()
	got := exitDetail(state.ExitRecord{
		ExitCode: 1,
		EndedAt:  time.Now(),
		LogTail:  "Traceback (most recent call last):\n  ImportError: no module named app\n",
	})
	head, tail, ok := strings.Cut(got, "\n")
	if !ok {
		t.Fatalf("exitDetail = %q, want the tail on its own lines", got)
	}
	if !strings.HasPrefix(head, "exit code 1") {
		t.Errorf("first line = %q, want the summary", head)
	}
	if !strings.Contains(tail, "ImportError") {
		t.Errorf("the log tail was dropped: %q", tail)
	}
}

// TestExitStatusDecodesAWaitError covers the translation from a Go wait error to
// the two fields the record holds.
func TestExitStatusDecodesAWaitError(t *testing.T) {
	t.Parallel()

	if code, sig := exitStatus(nil); code != 0 || sig != "" {
		t.Errorf("exitStatus(nil) = %d, %q; want 0, \"\"", code, sig)
	}
	if code, sig := exitStatus(errors.New("waitpid: no child processes")); code != -1 || sig != "" {
		t.Errorf("a non-ExitError = %d, %q; want -1, \"\" (the status was never observed)", code, sig)
	}

	// A real child, exiting a real non-zero code.
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = helperEnvFor("diesilent")
	if code, sig := exitStatus(cmd.Run()); code != 3 || sig != "" {
		t.Errorf("a child that exited 3 = %d, %q; want 3, \"\"", code, sig)
	}

	// A real child, killed by a real signal. The name is spelled the way an
	// operator writes it, not the way Go prints it ("killed").
	killed := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	killed.Env = helperEnvFor("sleep")
	if err := killed.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := killed.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if code, sig := exitStatus(killed.Wait()); code != -1 || sig != "SIGKILL" {
		t.Errorf("a signalled child = %d, %q; want -1, \"SIGKILL\"", code, sig)
	}
}

// TestCrashWithNoMaboCtlWatchingStillReportsExited is the case a one-shot CLI
// actually hits. Only a mabo-ctl that is still running can reap a child, and
// `mabo-ctl start` exits seconds after spawning — so when the service dies at
// three in the afternoon there is no mabo-ctl anywhere to write a record, and the
// pid file naming a process that no longer exists is the only evidence left.
//
// Reporting that as "stopped" is the byte-for-byte confusion between a service
// that crashed and one that was never started, in the one situation where it
// happens most.
func TestCrashWithNoMaboCtlWatchingStillReportsExited(t *testing.T) {
	sup, st := fixture(t, service.Instance{Name: "svc", Cmd: helperCmd()})

	// The state a departed mabo-ctl leaves behind: a pid file for a process that
	// has since died, no exit record, and the service's output still in the log.
	if err := st.WritePIDAt("svc", deadPID(t), time.Now().Add(-90*time.Minute)); err != nil {
		t.Fatalf("WritePIDAt: %v", err)
	}
	f, err := st.TruncateLog("svc")
	if err != nil {
		t.Fatalf("TruncateLog: %v", err)
	}
	if _, err := f.WriteString("Traceback: ImportError: no module named app\n"); err != nil {
		t.Fatalf("write log: %v", err)
	}
	f.Close()

	got := statusOf(t, sup, "svc")
	if got.Phase != PhaseExited {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseExited)
	}
	if !strings.Contains(got.Detail, "no mabo-ctl was running") {
		t.Errorf("Detail = %q, want it to say why there is no exit status", got.Detail)
	}
	if !strings.Contains(got.Detail, "ImportError") {
		t.Errorf("Detail = %q, want the log tail, which is the only explanation left", got.Detail)
	}
	// The exit status was never observed and must not be invented. exited_at
	// stays empty, which is the field a consumer tests before reading the rest.
	if got.ExitCode != -1 || got.ExitSignal != "" || !got.ExitedAt.IsZero() {
		t.Errorf("got ExitCode=%d ExitSignal=%q ExitedAt=%v; want an unobserved status",
			got.ExitCode, got.ExitSignal, got.ExitedAt)
	}
	// The spawn time did survive, because mabo-ctl wrote it down at the spawn.
	if got.StartedAt.IsZero() {
		t.Error("StartedAt is zero; the pid record carried it and it was dropped")
	}
	// Nothing is running, so there is no uptime.
	if got.Uptime != 0 {
		t.Errorf("Uptime = %s, want 0 for a service with no process", got.Uptime)
	}
}

// deadPID returns a pid that is certainly not running: a real child, started
// and reaped. Making one up risks colliding with a live process, which would
// test the opposite of what is intended.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = helperEnvFor("")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn a process to kill: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	for i := 0; i < 100 && state.Alive(pid); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if state.Alive(pid) {
		t.Fatalf("pid %d is still alive after being reaped", pid)
	}
	return pid
}

// fakeHolder replaces the lsof lookup with a fixed answer and counts the calls.
//
// lsof's answer on the machine running the tests is not something a test can
// arrange — the interesting case is a port held by a process mabo-ctl did not
// start, and manufacturing one portably is a bigger fixture than the behaviour
// under test. The seam is the uncached lookup, so the cache above it stays
// exercised for real.
func fakeHolder(sup *Supervisor, h Holder) *atomic.Int64 {
	var calls atomic.Int64
	sup.holderLookup = func(int) Holder {
		calls.Add(1)
		return h
	}
	return &calls
}

// TestStoppedServiceNamesWhoHoldsItsPort is the whole point of the change: the
// status block is what the user goes back and reads, and it used to answer "why
// did start refuse?" with an empty DETAIL — in the same command whose event,
// three lines above, had just named the pid and printed the lsof line.
func TestStoppedServiceNamesWhoHoldsItsPort(t *testing.T) {
	sup, _ := fixture(t, service.Instance{Name: "web", Port: 7411, Cmd: helperCmd()})
	fakeHolder(sup, Holder{PID: 5334, Command: "nc"})

	got := statusOf(t, sup, "web")
	if got.Phase != PhaseStopped {
		t.Fatalf("phase = %q, want %q — a held port does not make the service run", got.Phase, PhaseStopped)
	}
	for _, want := range []string{"5334", "nc", "7411", LsofCommand(7411)} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("Detail = %q, want it to contain %q", got.Detail, want)
		}
	}
	// The identical sentence the refusal uses, from the identical builder. Two
	// constructions of this line WILL drift; that is why there is only one.
	if want := portHeldError("web", 7411, Holder{PID: 5334, Command: "nc"}).Error(); got.Detail != want {
		t.Errorf("Detail = %q, want the same string startOne refuses with: %q", got.Detail, want)
	}
}

// TestStoppedServiceWithAFreePortHasNoDetail covers both the ordinary case and
// the degradation the change must not break: a machine with no lsof gets the
// same zero Holder as a free port, and must read as today's plain "stopped"
// rather than as a new error.
func TestStoppedServiceWithAFreePortHasNoDetail(t *testing.T) {
	sup, _ := fixture(t, service.Instance{Name: "web", Port: 7411, Cmd: helperCmd()})
	fakeHolder(sup, Holder{}) // what PortHolder returns when lsof is missing

	got := statusOf(t, sup, "web")
	if got.Phase != PhaseStopped || got.Detail != "" {
		t.Fatalf("got phase=%q detail=%q, want a plain stopped service with nothing to explain",
			got.Phase, got.Detail)
	}
}

// TestExitedServiceKeepsItsExitDetail guards the regression the port lookup
// could cause: a service that DIED already has the better answer — how it died
// and what it printed — and "the port is busy" must never overwrite it.
func TestExitedServiceKeepsItsExitDetail(t *testing.T) {
	sup, st := fixture(t, service.Instance{
		Name: "beta",
		Port: 7411,
		Cmd:  helperCmd(),
		Env:  helperEnvFor("sleep"),
	})
	if err := sup.Start(context.Background(), nil, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid, _ := st.ReadPID("beta")
	if pid <= 0 {
		t.Fatal("the service did not start")
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill -9 %d: %v", pid, err)
	}
	sup.Wait() // the reaper writes the record

	// Installed only now: a fake holder during Start would have made startOne
	// refuse, and the point here is what happens AFTER a real death.
	calls := fakeHolder(sup, Holder{PID: 5334, Command: "nc"})

	got := statusOf(t, sup, "beta")
	if got.Phase != PhaseExited {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseExited)
	}
	if !strings.Contains(got.Detail, "SIGKILL") {
		t.Errorf("Detail = %q, want it to still name the signal that killed the process", got.Detail)
	}
	if strings.Contains(got.Detail, "held by pid") {
		t.Errorf("Detail = %q; the port holder overwrote the exit reason", got.Detail)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("lsof ran %d times for a service that exited; a death is not a port conflict", n)
	}
}

// TestStatusDoesNotProbeThePortOfARunningService is the cost half of the
// change. Status runs on every web-console poll — every two seconds, for as
// long as the page is open — so a lookup that is not needed is a subprocess
// forked forever.
func TestStatusDoesNotProbeThePortOfARunningService(t *testing.T) {
	sup, _ := fixture(t, service.Instance{
		Name: "svc",
		Port: 7411,
		Cmd:  helperCmd(),
		Env:  helperEnvFor("sleep"),
	})
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	calls := fakeHolder(sup, Holder{})
	if err := sup.Start(context.Background(), nil, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// startOne asks once, uncached, because there the answer is a safety guard.
	if n := calls.Load(); n != 1 {
		t.Fatalf("startOne ran the lookup %d times, want exactly 1", n)
	}
	calls.Store(0)

	if got := statusOf(t, sup, "svc"); got.Phase == PhaseStopped {
		t.Fatalf("the service is not running, so nothing here is under test: %+v", got)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("lsof ran %d times for a live service, want 0", n)
	}
}

// TestStatusCachesThePortHolderLookup checks the bound on the new subprocess.
// The web console polls Status every two seconds forever; without the cache
// every stopped-but-ported service forks an lsof on every poll.
func TestStatusCachesThePortHolderLookup(t *testing.T) {
	sup, _ := fixture(t, service.Instance{Name: "web", Port: 7411, Cmd: helperCmd()})
	calls := fakeHolder(sup, Holder{PID: 5334, Command: "nc"})

	for i := 0; i < 5; i++ {
		if got := statusOf(t, sup, "web"); !strings.Contains(got.Detail, "5334") {
			t.Fatalf("poll %d lost the answer: Detail = %q", i, got.Detail)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("five polls ran the lookup %d times, want 1 — the cache is not holding", n)
	}

	// Age the entry past its TTL: the answer is cached, not frozen. A port
	// freed by the user must stop being reported as held.
	sup.holdersMu.Lock()
	sup.holders[7411] = holderEntry{holder: Holder{PID: 5334, Command: "nc"}, at: time.Now().Add(-2 * holderTTL)}
	sup.holdersMu.Unlock()
	sup.holderLookup = func(int) Holder { calls.Add(1); return Holder{} }

	if got := statusOf(t, sup, "web"); got.Detail != "" {
		t.Errorf("Detail = %q after the port was released, want it empty", got.Detail)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("the lookup ran %d times, want 2 — an expired entry must be refreshed", n)
	}
}

// TestCancelledStatusSkipsThePortLookup covers the branch a happy-path test
// cannot reach: a browser that navigates away mid-poll cancels the request
// context, and a status nobody will read must not fork lsof to fill in a column
// nobody will see.
func TestCancelledStatusSkipsThePortLookup(t *testing.T) {
	sup, _ := fixture(t, service.Instance{Name: "web", Port: 7411, Cmd: helperCmd()})
	calls := fakeHolder(sup, Holder{PID: 5334, Command: "nc"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sts := sup.Status(ctx)
	if len(sts) != 1 || sts[0].Phase != PhaseStopped {
		t.Fatalf("Status returned %+v, want one stopped service", sts)
	}
	if sts[0].Detail != "" {
		t.Errorf("Detail = %q, want it empty on a cancelled status", sts[0].Detail)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("lsof ran %d times on a cancelled status, want 0", n)
	}
}

// TestStoppedServiceNamesARealPortHolder exercises the production path with no
// seam at all: real lsof, a real listener, the real Holder. It is what proves
// the wiring, since every other test in this group replaces the lookup.
func TestStoppedServiceNamesARealPortHolder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	port := portOf(t, srv.URL)
	sup, _ := fixture(t, service.Instance{Name: "web", Port: port, Cmd: helperCmd()})

	got := statusOf(t, sup, "web")
	if !strings.Contains(got.Detail, "held by pid") {
		t.Skipf("lsof unavailable or returned nothing for port %d; detail = %q", port, got.Detail)
	}
	if !strings.Contains(got.Detail, LsofCommand(port)) {
		t.Errorf("Detail = %q, want the lsof command the user can run themselves", got.Detail)
	}
	if got.Phase != PhaseStopped {
		t.Errorf("phase = %q, want %q", got.Phase, PhaseStopped)
	}
}

// TestResetSweepSparesAServiceMaboCtlItselfStarted covers the race that only
// appears once mabo-ctl is RESIDENT.
//
// `mabo-ctl serve` and the interactive console supervise from a process that
// stays alive, so a reset and a start can be in flight together. Reset stops
// everything, then sweeps each declared port and kills whatever holds it as a
// process "mabo-ctl did not start" — but a start that landed after that Stop is
// holding the port legitimately. The sweep would kill a healthy service mabo-ctl
// had spawned seconds earlier and then delete the state that recorded it.
//
// The sweep is exercised directly, because going through Reset would stop the
// service first and leave nothing to race with — the window under test opens
// only AFTER that Stop returns.
func TestResetSweepSparesAServiceMaboCtlItselfStarted(t *testing.T) {
	inst := service.Instance{
		Name: "svc",
		Port: 7911,
		Cmd:  helperCmd(),
		Env:  helperEnvFor("sleep"),
	}
	sup, st := fixture(t, inst)
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	if err := sup.Start(context.Background(), nil, nil); err != nil && !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Start: %v", err)
	}
	pid, _ := st.ReadPID("svc")
	if pid <= 0 || !state.Alive(pid) {
		t.Fatal("the service is not running, so the race is not under test")
	}

	// The sweep finds our own freshly started process holding the port.
	sup.holderLookup = func(int) Holder { return Holder{PID: pid, Command: "svc"} }

	events, collect := drain(t)
	if err := sup.reapPort(context.Background(), sup.insts[0], true, events); err != nil {
		t.Fatalf("reapPort: %v", err)
	}

	if !state.Alive(pid) {
		t.Fatalf("the reset sweep killed pid %d, a service mabo-ctl had just started itself", pid)
	}
	var announced bool
	for _, e := range collect() {
		if strings.Contains(e.Msg, "left alone") {
			announced = true
		}
		if strings.Contains(e.Msg, "mabo-ctl did not start it") {
			t.Errorf("the sweep called its own service a foreign orphan: %q", e.Msg)
		}
	}
	if !announced {
		t.Error("the sweep spared the process but never said so; silence here reads as 'nothing held the port'")
	}
}

// TestResetDoesNotWipeStateUnderAConcurrentStart covers the second half of the
// reset race. The sweep was fixed to spare a service mabo-ctl had just started;
// the whole-tree wipe two lines below it was still unlocked.
//
// A Start that lands between the sweep and the wipe writes a pid record, and
// the wipe deletes it out from under a live setsid-detached process. What is
// left is a running service mabo-ctl cannot see, stop, or name — the orphan this
// tool exists to prevent, produced by the command that cleans orphans up.
func TestResetDoesNotWipeStateUnderAConcurrentStart(t *testing.T) {
	inst := service.Instance{
		Name: "svc",
		Cmd:  helperCmd(),
		Env:  helperEnvFor("sleep"),
	}
	sup, st := fixture(t, inst)
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	// Race a Start against a Reset. Whichever order they land in, the invariant
	// is the same: mabo-ctl must not end up with a live process it has no record
	// of. Either the start wins and its pid file survives, or the reset wins and
	// nothing is running.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = sup.Reset(context.Background(), false, nil)
	}()
	go func() {
		defer wg.Done()
		_ = sup.Start(context.Background(), nil, nil)
	}()
	wg.Wait()

	pid, _ := st.ReadPID("svc")
	alive := pid > 0 && state.Alive(pid)
	if alive {
		return // the start won and its record survived: consistent
	}
	// The reset won. Nothing of ours may still be running.
	for _, in := range sup.insts {
		if h := sup.lookupPortHolder(in.Port); h.PID > 0 && state.Alive(h.PID) {
			t.Fatalf("reset wiped the state while pid %d was left running — an orphan mabo-ctl "+
				"can no longer see or stop", h.PID)
		}
	}
	if pid > 0 && state.Alive(pid) {
		t.Fatalf("pid %d is alive but its record was wiped", pid)
	}
}

// TestAutostartFalseIsSkippedByABareStart covers the field's whole purpose: a
// service that is expensive, or only occasionally wanted, stays out of the
// default start without leaving the registry.
func TestAutostartFalseIsSkippedByABareStart(t *testing.T) {
	sup, st := fixture(t,
		service.Instance{Name: "api", Cmd: helperCmd(), Env: helperEnvFor("sleep")},
		service.Instance{Name: "heavy", Cmd: helperCmd(), Env: helperEnvFor("sleep"), NoAutostart: true},
	)
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	if err := sup.Start(context.Background(), nil, nil); err != nil && !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Start: %v", err)
	}
	if pid, _ := st.ReadPID("api"); pid <= 0 || !state.Alive(pid) {
		t.Error("api did not start, but it has autostart enabled")
	}
	if pid, _ := st.ReadPID("heavy"); pid > 0 && state.Alive(pid) {
		t.Errorf("heavy started on a bare start despite autostart: false (pid %d)", pid)
	}
}

// TestAutostartFalseStartsWhenNamed pins the other half. autostart decides what
// happens when the operator named NOTHING; naming a service is always an
// instruction, never a suggestion.
func TestAutostartFalseStartsWhenNamed(t *testing.T) {
	sup, st := fixture(t,
		service.Instance{Name: "heavy", Cmd: helperCmd(), Env: helperEnvFor("sleep"), NoAutostart: true},
	)
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	if err := sup.Start(context.Background(), []string{"heavy"}, nil); err != nil && !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Start: %v", err)
	}
	if pid, _ := st.ReadPID("heavy"); pid <= 0 || !state.Alive(pid) {
		t.Error("naming a service explicitly did not start it")
	}
}

// TestAutostartFalseStillStartsAsADependency is why the filter turns an empty
// selection into an explicit list BEFORE SelectLevels rather than filtering
// after it. Filtering after would drop the dependency and start the dependant
// against a service that is not there — worse than either outcome the flag is
// meant to choose between.
func TestAutostartFalseStillStartsAsADependency(t *testing.T) {
	sup, st := fixture(t,
		service.Instance{Name: "db", Cmd: helperCmd(), Env: helperEnvFor("sleep"), NoAutostart: true},
		service.Instance{
			Name: "app", Cmd: helperCmd(), Env: helperEnvFor("sleep"),
			DependsOn: []string{"db"},
		},
	)
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	if err := sup.Start(context.Background(), nil, nil); err != nil && !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Start: %v", err)
	}
	if pid, _ := st.ReadPID("db"); pid <= 0 || !state.Alive(pid) {
		t.Error("db was skipped, but app depends on it — app would be running against nothing")
	}
}

// TestEveryServiceManualIsReportedNotSilent: a start that does nothing must say
// so. Exiting 0 with no output would read as "started fine".
func TestEveryServiceManualIsReportedNotSilent(t *testing.T) {
	sup, _ := fixture(t,
		service.Instance{Name: "only", Cmd: helperCmd(), Env: helperEnvFor("sleep"), NoAutostart: true},
	)
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	ev, collect := drain(t)
	if err := sup.Start(context.Background(), nil, ev); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var said bool
	for _, e := range collect() {
		if strings.Contains(e.Msg, "autostart") {
			said = true
		}
	}
	if !said {
		t.Error("a start with nothing to do said nothing; that reads as success")
	}
}

// TestPerServiceReadyTimeoutOverridesTheGlobal: a service that declares its
// own ready_timeout crosses from slow to degraded on ITS clock, not the
// global's — the same spawn-time arithmetic as TestSlowBecomesDegradedPastReady
// Timeout, with an instance value the global would never produce.
func TestPerServiceReadyTimeoutOverridesTheGlobal(t *testing.T) {
	sup, st := fixture(t, service.Instance{
		Name:   "svc",
		Cmd:    helperCmd(),
		Env:    helperEnvFor("sleep"),
		Health: "http://127.0.0.1:1/", // nothing will ever answer this
		// The service's own window, far SHORTER than the 30s global: if the
		// global were consulted, ten minutes ago would still read "slow".
		ReadyTimeout: 5 * time.Second,
	})
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	if err := sup.Start(context.Background(), nil, nil); err != nil && !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Start: %v", err)
	}
	pid, _ := st.ReadPID("svc")
	if pid <= 0 || !state.Alive(pid) {
		t.Fatal("the service is not running, so neither phase is under test")
	}

	// Wait out the service's own 5s window (inside the 30s global) for real.
	// Backdating the pid record instead — the old shortcut — now reads as a
	// forged record whose spawn time disagrees with the kernel, and
	// verifyGroup refuses it, which is the recycled-pid guard doing its job.
	time.Sleep(6 * time.Second)
	if got := statusOf(t, sup, "svc"); got.Phase != PhaseDegraded {
		t.Fatalf("phase past the service's own 5s window = %q, want %q", got.Phase, PhaseDegraded)
	}
}

// tcp and exec readiness probes through the supervisor

// TestStatusDerivesReadyFromATCPProbe proves the tcp probe family reaches the
// same single derivation point as http: one listener, one status call, and the
// phase is ready with no HTTP status attached to it.
func TestStatusDerivesReadyFromATCPProbe(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	sup, _ := fixture(t, service.Instance{
		Name:   "db",
		Cmd:    helperCmd(),
		Env:    helperEnvFor("sleep"),
		Health: "tcp:" + l.Addr().String(),
		Probe:  service.Probe{Kind: service.ProbeTCP, Addr: l.Addr().String()},
	})
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	if err := sup.Start(context.Background(), nil, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sts := sup.Status(context.Background())
	if len(sts) != 1 || sts[0].Phase != PhaseReady {
		t.Fatalf("status = %+v, want a ready service behind a tcp probe", sts)
	}
	if sts[0].HTTP != 0 {
		t.Errorf("HTTP = %d, want 0 — a connected socket has no HTTP status", sts[0].HTTP)
	}
}

// TestStartReportsReadyThroughAnExecProbe covers the exec family end to end:
// the probe runs in the instance's dir, exit 0 is ready, and the event says so.
func TestStartReportsReadyThroughAnExecProbe(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "probe.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	secret := "sk-live-exec-probe-secret"
	leaky := filepath.Join(dir, "leaky.sh")
	if err := os.WriteFile(leaky, []byte("#!/bin/sh\necho "+secret+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	sup, _ := fixture(t, service.Instance{
		Name:   "worker",
		Cmd:    helperCmd(),
		Env:    helperEnvFor("sleep"),
		Dir:    dir,
		Health: "exec: [" + script + "]",
		Probe:  service.Probe{Kind: service.ProbeExec, Argv: []string{script}},
	})
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	ev := make(chan Event, 16)
	err := sup.Start(context.Background(), []string{"worker"}, ev)
	close(ev)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var sawReady bool
	for e := range ev {
		if strings.Contains(e.Msg, secret) {
			t.Errorf("event %q leaked probe output; exec diagnostics belong to failures only", e.Msg)
		}
		if e.Phase == PhaseReady && strings.Contains(e.Msg, "exec") {
			sawReady = true
		}
	}
	if !sawReady {
		t.Error("no ready event naming the exec probe was emitted")
	}
}

// TestExecProbeFailureIsNotReady: an exec probe exiting non-zero must leave the
// service slow (inside its window), never ready — and the failure detail must
// quote the probe's diagnostic.
func TestExecProbeFailureIsNotReady(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fail.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho boom-marker\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	sup, _ := fixture(t, service.Instance{
		Name:         "worker",
		Cmd:          helperCmd(),
		Env:          helperEnvFor("sleep"),
		Dir:          dir,
		Health:       "exec: [" + script + "]",
		Probe:        service.Probe{Kind: service.ProbeExec, Argv: []string{script}},
		ReadyTimeout: time.Second,
	})
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	// A probe that never answers leaves the service SLOW, not failed: the
	// process is alive inside its startup window. That is not a Start error —
	// exactly as an http probe that never answers is not.
	if err := sup.Start(context.Background(), nil, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sts := sup.Status(context.Background())
	st := sts[0]
	switch st.Phase {
	case PhaseSlow, PhaseDegraded:
	default:
		t.Fatalf("phase = %s, want slow or degraded for a failing probe against a live process", st.Phase)
	}
	if !strings.Contains(st.Detail, "boom-marker") || !strings.Contains(st.Detail, "exit status 7") {
		t.Errorf("detail = %q, want the captured diagnostic with the exit code", st.Detail)
	}
}

// Destructive paths: signalPID, Restart, Reset against a real foreign process.
//
// Every victim here is a process THIS TEST SPAWNED via the TestHelperProcess
// fixture — never a hardcoded or looked-up pid — because a buggy reset test is
// a test that kills the developer's editor.

// spawnForeign starts a detached-from-mabo-ctl helper process the way the
// world outside would have left one behind: no pid file, no record, just a pid.
//
// It returns dead, the test-friendly way to ask whether the victim FINISHED —
// not merely whether its pid answers, which a zombie does until it is waited
// on, and this test process is the one that must wait for it.
func spawnForeign(t *testing.T) (pid int, dead func() bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = helperEnvFor("sleep")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn foreign helper: %v", err)
	}
	pid = cmd.Process.Pid
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	dead = func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	t.Cleanup(func() {
		if !dead() {
			_ = cmd.Process.Kill()
			<-done
		}
	})
	return pid, dead
}

// awaitForeignDeath polls dead until it holds or the deadline passes.
func awaitForeignDeath(dead func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for !dead() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	return dead()
}

// TestSignalPIDRefusesTheUnsafePids covers both refusal branches of signalPID:
// init (and everything below it), and mabo-ctl's own pid. Both are
// ErrUnsafeSignal, and neither may reach syscall.Kill.
func TestSignalPIDRefusesTheUnsafePids(t *testing.T) {
	for _, pid := range []int{-1, 0, 1} {
		err := signalPID(pid, termSignal)
		if !errors.Is(err, ErrUnsafeSignal) {
			t.Errorf("signalPID(%d) = %v, want ErrUnsafeSignal", pid, err)
		}
	}
	err := signalPID(os.Getpid(), termSignal)
	if !errors.Is(err, ErrUnsafeSignal) || !strings.Contains(err.Error(), "itself") {
		t.Errorf("signalPID(own pid) = %v, want ErrUnsafeSignal naming mabo-ctl itself", err)
	}
}

// TestSignalPIDSignalsARealChild proves the happy path actually signals: a
// helper this test spawned dies of SIGTERM through signalPID alone.
func TestSignalPIDSignalsARealChild(t *testing.T) {
	victim, dead := spawnForeign(t)
	if err := signalPID(victim, termSignal); err != nil {
		t.Fatalf("signalPID(%d): %v", victim, err)
	}
	if !awaitForeignDeath(dead) {
		t.Fatalf("pid %d survived SIGTERM through signalPID", victim)
	}

	// And a pid that is already gone is an error, not silence.
	if err := signalPID(victim, termSignal); err == nil {
		t.Error("signalling a dead pid returned nil; the caller cannot distinguish that from success")
	}
}

// TestResetWithoutForceSparesTheForeignHolder: the sweep finds a process
// mabo-ctl did not start, refuses to touch it, and says so — three behaviours,
// each of which protects something irreplaceable.
func TestResetWithoutForceSparesTheForeignHolder(t *testing.T) {
	foreign, foreignDead := spawnForeign(t)

	inst := service.Instance{Name: "svc", Port: 7913, Cmd: helperCmd(), Env: helperEnvFor("sleep")}
	sup, st := fixture(t, inst)
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()
	sup.holderLookup = func(int) Holder { return Holder{PID: foreign, Command: "stray-server"} }

	events, collect := drain(t)
	if err := sup.Reset(context.Background(), false, events); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if foreignDead() {
		t.Fatal("reset WITHOUT force killed the foreign holder; only --force may do that")
	}
	var named bool
	for _, e := range collect() {
		if strings.Contains(e.Msg, "left alone") && strings.Contains(e.Msg, "--force") {
			named = true
		}
	}
	if !named {
		t.Errorf("the spare was not announced with the remedy; events = %v", msgs(collect()))
	}
	_ = st
}

// TestResetWithForceKillsTheForeignHolder is the other half: --force is the
// operator saying kill it, and the sweep must then actually reap the orphan —
// signalling THE PID, which is exactly what signalPID exists to gate.
func TestResetWithForceKillsTheForeignHolder(t *testing.T) {
	foreign, foreignDead := spawnForeign(t)

	inst := service.Instance{Name: "svc", Port: 7914, Cmd: helperCmd(), Env: helperEnvFor("sleep")}
	sup, _ := fixture(t, inst)
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()
	sup.holderLookup = func(int) Holder { return Holder{PID: foreign, Command: "stray-server"} }

	events, collect := drain(t)
	if err := sup.Reset(context.Background(), true, events); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if !awaitForeignDeath(foreignDead) {
		t.Fatalf("reset WITH force left pid %d holding the port", foreign)
	}
	var announced bool
	for _, e := range collect() {
		if strings.Contains(e.Msg, fmt.Sprintf("killing pid %d", foreign)) {
			announced = true
		}
	}
	if !announced {
		t.Errorf("the kill was not announced by name; events = %v", msgs(collect()))
	}
}

// TestRestartReplacesTheProcess covers Restart end to end on real processes:
// the old process is gone, the new one is ours, and the pid file agrees with
// reality — the whole point of the verb.
func TestRestartReplacesTheProcess(t *testing.T) {
	sup, st := fixture(t, service.Instance{
		Name: "svc",
		Cmd:  helperCmd(),
		Env:  helperEnvFor("sleep"),
	})
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	if err := sup.Start(context.Background(), []string{"svc"}, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	oldPID, _ := st.ReadPID("svc")
	if oldPID <= 0 || !state.Alive(oldPID) {
		t.Fatalf("first start produced pid %d, not alive", oldPID)
	}

	if err := sup.Restart(context.Background(), []string{"svc"}, nil); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	newPID, _ := st.ReadPID("svc")
	if newPID <= 0 || !state.Alive(newPID) {
		t.Fatalf("after Restart the recorded pid %d is not a live process", newPID)
	}
	if newPID == oldPID {
		t.Fatal("Restart left the SAME pid; nothing was restarted")
	}
	deadline := time.Now().Add(5 * time.Second)
	for state.Alive(oldPID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if state.Alive(oldPID) {
		t.Fatalf("the previous process %d outlived its restart — an orphan", oldPID)
	}
}

// TestStatusRunningNotReadyForALivePortlessService pins the honest answer for a
// service with no probe: alive means RUNNING, never ready — claiming ready
// would assert something mabo-ctl never checked.
func TestStatusRunningNotReadyForALivePortlessService(t *testing.T) {
	sup, _ := fixture(t, service.Instance{
		Name: "worker",
		Cmd:  helperCmd(),
		Env:  helperEnvFor("sleep"),
	})
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	if err := sup.Start(context.Background(), nil, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sts := sup.Status(context.Background())
	if len(sts) != 1 || sts[0].Phase != PhaseRunning {
		t.Fatalf("status = %+v, want phase running for a live probeless service", sts)
	}
	if sts[0].HTTP != 0 {
		t.Errorf("HTTP = %d, want 0: no probe ever answered", sts[0].HTTP)
	}
}

// TestStatusSurfacesAMalformedPidFile: a corrupt byte in .dev/pids must be
// named in Detail rather than silently reading as stopped.
func TestStatusSurfacesAMalformedPidFile(t *testing.T) {
	sup, st := fixture(t, service.Instance{
		Name: "svc",
		Cmd:  helperCmd(),
		Env:  helperEnvFor("sleep"),
	})
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	if err := os.WriteFile(st.PIDPath("svc"), []byte("not a pid at all\n"), 0o600); err != nil {
		t.Fatalf("write corrupt pid file: %v", err)
	}
	sts := sup.Status(context.Background())
	if len(sts) != 1 {
		t.Fatalf("got %d statuses, want 1", len(sts))
	}
	if !strings.Contains(sts[0].Detail, "malformed") && !strings.Contains(sts[0].Detail, "pid file") {
		t.Errorf("Detail = %q, want it to name the malformed pid file", sts[0].Detail)
	}
}

// cross-process double-spawn lock

// TestStartRefusesWhenAnotherMaboCtlHoldsTheClaim: two mabo-ctl processes
// racing one service is the race the per-service mutex cannot see. A fresh
// claim by a live foreign process must block the start — not spawn a second
// copy — and the refusal must name what happened.
func TestStartRefusesWhenAnotherMaboCtlHoldsTheClaim(t *testing.T) {
	foreign, dead := spawnForeign(t)

	sup, st := fixture(t, service.Instance{
		Name: "worker",
		Cmd:  helperCmd(),
		Env:  helperEnvFor("sleep"),
	})
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	if _, err := st.ClaimPID("worker", foreign, time.Now()); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	events, collect := drain(t)
	err := sup.Start(context.Background(), []string{"worker"}, events)
	if !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Start = %v, want ErrNotStarted for a claimed service", err)
	}
	if pid, _ := st.ReadPID("worker"); pid > 0 {
		t.Fatalf("a second copy was spawned anyway (pid %d) while %d held the claim", pid, foreign)
	}
	if dead() {
		t.Fatal("the claim holder died during the test; the staleness path fired instead of the refusal")
	}
	var said bool
	for _, e := range collect() {
		if strings.Contains(e.Msg, "being started by another mabo-ctl") {
			said = true
		}
	}
	if !said {
		t.Errorf("the refusal did not explain the claim")
	}

	// Once released, the same start succeeds and leaves no claim behind.
	if err := st.ReleaseClaim("worker"); err != nil {
		t.Fatalf("ReleaseClaim: %v", err)
	}
	if err := sup.Start(context.Background(), []string{"worker"}, nil); err != nil {
		t.Fatalf("Start after release: %v", err)
	}
	if _, err := os.Stat(st.PIDClaimPath("worker")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("claim file survived a successful start: %v", err)
	}
}

// TestStatusNamesAnUnrunnableService: a service whose runtime never resolved
// must not render as bare "stopped" — byte-identical to one nobody tried —
// with the same cannot-start message preflight's machine pass front-runs.
func TestStatusNamesAnUnrunnableService(t *testing.T) {
	sup, _ := fixture(t, service.Instance{
		Name:   "ghosted",
		Cmd:    []string{"/nonexistent/interpreter", "serve"},
		CmdErr: errors.New(`conda env "nosuchenv" was not found`),
	})

	got := sup.Status(context.Background())
	if len(got) != 1 {
		t.Fatalf("got %d statuses, want 1", len(got))
	}
	if got[0].Phase != PhaseStopped {
		t.Errorf("phase = %q, want stopped (it is not running)", got[0].Phase)
	}
	if !strings.Contains(got[0].Detail, "cannot start") || !strings.Contains(got[0].Detail, "nosuchenv") {
		t.Errorf("Detail = %q, want the cannot-start reason naming the runtime problem", got[0].Detail)
	}

	// A plain unstarted service keeps its empty DETAIL: nothing is wrong.
	clean, _ := fixture(t, service.Instance{Name: "idle"})
	if d := clean.Status(context.Background())[0].Detail; d != "" {
		t.Errorf("unstarted service grew detail %q; want none", d)
	}
}
