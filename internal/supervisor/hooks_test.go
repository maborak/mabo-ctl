package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/state"
)

// hookScript writes an executable hook that drops a marker file and exits
// with the given status ("" means 0), and returns the argv to run it.
func hookScript(t *testing.T, dir, marker, status string) []string {
	t.Helper()
	body := "#!/bin/sh\nprintf hook > " + marker + "\nexit " + status + "\n"
	p := filepath.Join(dir, filepath.Base(marker)+".sh")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write hook script: %v", err)
	}
	return []string{p}
}

func sleepService(name, dir string) service.Instance {
	return service.Instance{
		Name: name,
		Cmd:  helperCmd(),
		Env:  helperEnvFor("sleep"),
		Dir:  dir,
	}
}

func readyProbeService(name, dir string) service.Instance {
	in := sleepService(name, dir)
	in.Health = "exec: [true]"
	in.Probe = service.Probe{Kind: service.ProbeExec, Argv: []string{"true"}}
	return in
}

func TestPreStartHookRunsAndFailingItFailsTheStart(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "prestart-ran")

	in := readyProbeService("svc", dir)
	in.Hooks.PreStart = hookScript(t, dir, marker, "3")

	sup, st := fixture(t, in)
	defer sup.Wait()
	ev, collect := drain(t)
	err := sup.Start(context.Background(), []string{"svc"}, ev)
	events := collect()

	if err == nil {
		t.Fatalf("start succeeded despite a failing pre_start hook; events = %v", msgs(events))
	}
	if _, serr := os.Stat(marker); serr != nil {
		t.Errorf("pre_start hook did not run before the refusal: %v", serr)
	}
	if !hasMsg(events, "pre_start hook failed") {
		t.Errorf("no pre_start failure in events = %v", msgs(events))
	}
	rec, ok, rerr := st.ReadExit("svc")
	if rerr != nil || !ok {
		t.Fatalf("no exit record after hook refusal (ok=%v err=%v)", ok, rerr)
	}
	if !rec.Startup {
		t.Errorf("exit record Startup = false, want true: %+v", rec)
	}
	if rec.ExitCode != 3 {
		t.Errorf("exit code = %d, want the hook's 3", rec.ExitCode)
	}
	if pid, _ := st.ReadPID("svc"); pid > 0 {
		t.Errorf("service spawned (pid %d) despite hook failure", pid)
	}
}

func TestPostStartHookIsBestEffort(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "poststart-ran")

	in := readyProbeService("svc", dir)
	in.Hooks.PostStart = hookScript(t, dir, marker, "1")

	sup, st := fixture(t, in)
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	ev, collect := drain(t)
	err := sup.Start(context.Background(), []string{"svc"}, ev)
	events := collect()

	if err != nil {
		t.Fatalf("start failed: %v (events %v)", err, msgs(events))
	}
	if _, serr := os.Stat(marker); serr != nil {
		t.Errorf("post_start hook did not run: %v", serr)
	}
	if !hasMsg(events, "post_start hook failed") {
		t.Errorf("post_start failure not reported; events = %v", msgs(events))
	}
	if pid, _ := st.ReadPID("svc"); pid <= 0 {
		t.Errorf("service not running despite post_start failure")
	}
}
func TestStopHooksRunAndCannotBlockTheStop(t *testing.T) {
	dir := t.TempDir()
	pre := filepath.Join(dir, "prestop-ran")
	post := filepath.Join(dir, "poststop-ran")

	in := sleepService("svc", dir)
	in.Hooks.PreStop = hookScript(t, dir, pre, "1")   // failing on purpose
	in.Hooks.PostStop = hookScript(t, dir, post, "7") // failing on purpose

	sup, st := fixture(t, in)
	defer sup.Wait()

	ev := make(chan Event, 64)
	if err := sup.Start(context.Background(), []string{"svc"}, ev); err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sup.Stop(ctx, []string{"svc"}, nil)
	sup.Wait()

	for _, p := range []string{pre, post} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s marker missing: %v", filepath.Base(p), err)
		}
	}
	pid, _ := st.ReadPID("svc")
	if pid > 0 && state.Alive(pid) {
		t.Errorf("service survived a stop with failing stop hooks")
	}
}

func TestStartSkipsGatedDependantOfANeverReadyDependency(t *testing.T) {
	sup, st := fixture(t,
		sleepService("base", t.TempDir()), // no probe: running forever, never ready
		func() service.Instance {
			in := sleepService("gated", t.TempDir())
			in.DependsOn = []string{"base"}
			in.DependsReadyOn = []string{"base"}
			return in
		}(),
	)
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	ev, collect := drain(t)
	_ = sup.Start(context.Background(), nil, ev)
	events := collect()

	if !hasMsg(events, "skipped: dependency base") {
		t.Errorf("gated skip not reported; events = %v", msgs(events))
	}
	if !strings.Contains(strings.Join(msgs(events), "|"), "gates on its readiness") {
		t.Errorf("skip message should say why: %v", msgs(events))
	}
	if pid, _ := st.ReadPID("gated"); pid > 0 {
		t.Errorf("gated dependant started (pid %d) though base never became ready", pid)
	}
}

func TestStartRunsGatedDependantOnceDependencyIsReady(t *testing.T) {
	sup, st := fixture(t,
		readyProbeService("base", t.TempDir()),
		func() service.Instance {
			in := sleepService("gated", t.TempDir())
			in.DependsOn = []string{"base"}
			in.DependsReadyOn = []string{"base"}
			return in
		}(),
	)
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	ev := make(chan Event, 64)
	if err := sup.Start(context.Background(), nil, ev); err != nil {
		t.Fatalf("start: %v", err)
	}
	if pid, _ := st.ReadPID("gated"); pid <= 0 {
		t.Errorf("gated dependant did not start behind a ready base")
	}
}

// TestPreStartHookIsBoundedByTheReadyBudget pins the claim-wedge fix: a hung
// pre_start used to run under the bare caller context while startOne held the
// per-service lock AND the cross-process start claim, so every other
// terminal's start was refused with ErrClaimed for up to the claim age limit
// with nothing on screen explaining why. The hook now runs under the same
// budget a readiness probe gets: the start fails visibly, the claim is
// released by the normal path, and the next start is not refused.
func TestPreStartHookIsBoundedByTheReadyBudget(t *testing.T) {
	dir := t.TempDir()
	hook := filepath.Join(dir, "hang.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatalf("write hook script: %v", err)
	}
	in := sleepService("hangsvc", dir)
	in.Hooks = config.Hooks{PreStart: []string{hook}}
	sup, _ := fixture(t, in)
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	ev := make(chan Event, 64)
	started := time.Now()
	if err := sup.Start(context.Background(), []string{"hangsvc"}, ev); err == nil {
		t.Fatal("a start whose pre_start hook hangs must fail under the hook budget, not block")
	}
	close(ev)
	if d := time.Since(started); d > 15*time.Second {
		t.Fatalf("the hung hook took %s to fail; nothing bounded it", d)
	}

	// The wedge this test exists to prevent would refuse the second start
	// with ErrClaimed. Make the hook well-behaved and start for real.
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("rewrite hook script: %v", err)
	}
	ev2 := make(chan Event, 64)
	if err := sup.Start(context.Background(), []string{"hangsvc"}, ev2); err != nil {
		t.Fatalf("second start after the bounded hook failure = %v, want success (the claim was not released)", err)
	}
	close(ev2)
}

// TestStopAnnouncesThePreStopHook pins the visibility fix: pre_stop runs
// BEFORE the stopping announcement and before SIGTERM, so a slow hook used to
// leave the operator staring at a silent stop. The hook event must now be in
// the stream ahead of the stopping line and the final stopped phase.
func TestStopAnnouncesThePreStopHook(t *testing.T) {
	dir := t.TempDir()
	hook := filepath.Join(dir, "slow.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nsleep 0.3\n"), 0o755); err != nil {
		t.Fatalf("write hook script: %v", err)
	}
	in := sleepService("hookstop", dir)
	in.Hooks = config.Hooks{PreStop: []string{hook}}
	sup, _ := fixture(t, in)
	defer func() { _ = sup.Stop(context.Background(), nil, nil); sup.Wait() }()

	ev := make(chan Event, 64)
	if err := sup.Start(context.Background(), []string{"hookstop"}, ev); err != nil {
		t.Fatalf("Start: %v", err)
	}
	close(ev)

	stopev := make(chan Event, 64)
	if err := sup.Stop(context.Background(), []string{"hookstop"}, stopev); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	close(stopev)

	hookAt, stoppingAt, stoppedAt := -1, -1, -1
	for i, e := range collectEvents(stopev) {
		switch {
		case strings.Contains(e.Msg, "pre_stop hook"):
			hookAt = i
		case strings.Contains(e.Msg, "stopping…"):
			stoppingAt = i
		case e.Phase == PhaseStopped:
			stoppedAt = i
		}
	}
	if hookAt == -1 || stoppingAt == -1 || stoppedAt == -1 {
		t.Fatalf("missing events: hook=%d stopping=%d stopped=%d", hookAt, stoppingAt, stoppedAt)
	}
	if hookAt > stoppingAt || stoppingAt > stoppedAt {
		t.Fatalf("event order wrong: hook=%d stopping=%d stopped=%d", hookAt, stoppingAt, stoppedAt)
	}
}

// collectEvents drains a closed event channel into a slice.
func collectEvents(ch chan Event) []Event {
	var got []Event
	for e := range ch {
		got = append(got, e)
	}
	return got
}
