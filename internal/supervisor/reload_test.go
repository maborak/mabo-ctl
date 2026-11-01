package supervisor

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/state"
)

// reloadFixture starts insts under a fresh supervisor and returns it with its
// state dir. Cleanup stops whatever is still running under the names the test
// registers through stopAll, so a reload that hands services to a second
// supervisor still cleans up through the one that owns them last.
func reloadFixture(t *testing.T, insts ...service.Instance) (*Supervisor, *state.Dir) {
	t.Helper()
	sup, st := fixture(t, insts...)
	if err := sup.Start(context.Background(), nil, nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	return sup, st
}

// stopAll stops every service sup knows and waits for every supervisor's
// reapers.
func stopAll(t *testing.T, sup *Supervisor, all ...*Supervisor) {
	t.Helper()
	t.Cleanup(func() {
		_ = sup.Stop(context.Background(), nil, nil)
		for _, s := range append(all, sup) {
			s.Wait()
		}
	})
}

// next builds the supervisor a reload would hand over: same config and state,
// new instances.
func next(old *Supervisor, insts ...service.Instance) *Supervisor {
	return New(old.cfg, old.st, insts)
}

// pidOf reads svc's recorded pid, failing the test on a read error.
func pidOf(t *testing.T, st *state.Dir, svc string) int {
	t.Helper()
	pid, err := st.ReadPID(svc)
	if err != nil {
		t.Fatalf("read pid %s: %v", svc, err)
	}
	return pid
}

// A reload restarts exactly the services whose PROCESS would differ. The
// colour edit on b is real, and b must keep its pid: restarting it would rotate
// its log away for nothing.
func TestApplyReloadRestartsOnlyAChangedProcess(t *testing.T) {
	dir := t.TempDir()
	old, st := reloadFixture(t, sleepService("a", dir), sleepService("b", dir))
	pidA, pidB := pidOf(t, st, "a"), pidOf(t, st, "b")

	a, b := sleepService("a", dir), sleepService("b", dir)
	a.Env = append(a.Env, "RELOAD_VALUE=changed")
	b.Color = "red"
	nxt := next(old, a, b)
	stopAll(t, nxt, old)

	rep, err := old.ApplyReload(context.Background(), nxt, nil)
	if err != nil || !rep.Applied {
		t.Fatalf("ApplyReload = %+v, %v", rep, err)
	}
	if !slices.Equal(rep.Restarted, []string{"a"}) {
		t.Errorf("Restarted = %v, want [a]", rep.Restarted)
	}
	if got := pidOf(t, st, "a"); got == pidA || got == 0 {
		t.Errorf("a pid = %d (was %d): the changed environment was not applied", got, pidA)
	}
	if got := pidOf(t, st, "b"); got != pidB {
		t.Errorf("b pid = %d, want the untouched %d: a colour change restarted it", got, pidB)
	}
}

// Every child's environment carries every ported service's <NAME>_PORT, so a
// naive comparison restarted the whole stack when one service was added. A
// sibling's port that CHANGED is still a reason: the process holds a stale
// value.
func TestApplyReloadIgnoresOnlyMembershipPortVariables(t *testing.T) {
	dir := t.TempDir()
	withPorts := func(name string, ports ...string) service.Instance {
		in := sleepService(name, dir)
		in.Env = append(in.Env, ports...)
		return in
	}
	old, st := reloadFixture(t, withPorts("a", "A_PORT=7001", "B_PORT=7002"), withPorts("b", "A_PORT=7001", "B_PORT=7002"))
	pidA, pidB := pidOf(t, st, "a"), pidOf(t, st, "b")

	// c is new: its variable appears in a and b without changing either one.
	added := next(old,
		withPorts("a", "A_PORT=7001", "B_PORT=7002", "C_PORT=7003"),
		withPorts("b", "A_PORT=7001", "B_PORT=7002", "C_PORT=7003"),
		withPorts("c", "A_PORT=7001", "B_PORT=7002", "C_PORT=7003"))
	stopAll(t, added, old)
	rep, err := old.ApplyReload(context.Background(), added, nil)
	if err != nil {
		t.Fatalf("ApplyReload: %v", err)
	}
	if len(rep.Restarted) != 0 {
		t.Errorf("adding a service restarted %v", rep.Restarted)
	}
	if pidOf(t, st, "a") != pidA || pidOf(t, st, "b") != pidB {
		t.Error("adding a service changed a running service's pid")
	}

	// b moved: a holds B_PORT=7002, which now points at nothing.
	moved := next(added,
		withPorts("a", "A_PORT=7001", "B_PORT=7012", "C_PORT=7003"),
		withPorts("b", "A_PORT=7001", "B_PORT=7012", "C_PORT=7003"),
		withPorts("c", "A_PORT=7001", "B_PORT=7012", "C_PORT=7003"))
	stopAll(t, moved, added)
	rep, err = added.ApplyReload(context.Background(), moved, nil)
	if err != nil {
		t.Fatalf("ApplyReload: %v", err)
	}
	if !slices.Equal(rep.Restarted, []string{"a", "b"}) {
		t.Errorf("Restarted = %v, want [a b]: a sibling's changed port is a changed environment", rep.Restarted)
	}
}

func TestApplyReloadStopsARemovedService(t *testing.T) {
	dir := t.TempDir()
	old, st := reloadFixture(t, sleepService("a", dir), sleepService("gone", dir))
	pidA, pidGone := pidOf(t, st, "a"), pidOf(t, st, "gone")
	nxt := next(old, sleepService("a", dir))
	stopAll(t, nxt, old)

	rep, err := old.ApplyReload(context.Background(), nxt, nil)
	if err != nil || !rep.Applied {
		t.Fatalf("ApplyReload = %+v, %v", rep, err)
	}
	if !slices.Equal(rep.Stopped, []string{"gone"}) {
		t.Errorf("Stopped = %v, want [gone]", rep.Stopped)
	}
	if state.Alive(pidGone) {
		t.Errorf("removed service pid %d is still alive", pidGone)
	}
	if pidOf(t, st, "a") != pidA {
		t.Error("the unchanged service was restarted")
	}
}

// A service the new configuration cannot spawn refuses the whole reload BEFORE
// anything is stopped: the previous configuration stays in force, running.
func TestApplyReloadRefusesAnUnrunnableServiceBeforeStopping(t *testing.T) {
	dir := t.TempDir()
	old, st := reloadFixture(t, sleepService("a", dir))
	stopAll(t, old)
	pidA := pidOf(t, st, "a")

	broken := sleepService("a", dir)
	broken.Cmd = []string{"no-such-interpreter"}
	broken.CmdErr = errors.New("runtime node:99 is not installed")
	rep, err := old.ApplyReload(context.Background(), next(old, broken), nil)
	if rep.Applied || err == nil {
		t.Fatalf("ApplyReload = %+v, %v; want a refusal", rep, err)
	}
	if got := pidOf(t, st, "a"); got != pidA || !state.Alive(pidA) {
		t.Errorf("a = pid %d (was %d): a refused reload must not stop anything", got, pidA)
	}
}

// A reload that stops a service and cannot start it again owes it a start:
// the next reload, after the operator fixes the definition, brings it back
// instead of skipping it as "not running".
func TestApplyReloadOwesAFailedStartToTheNextReload(t *testing.T) {
	dir := t.TempDir()
	old, st := reloadFixture(t, sleepService("a", dir))

	// A probe that never answers makes the start wait, and so see the death.
	dies := sleepService("a", dir)
	dies.Env = helperEnvFor("diesilent")
	dies.Health = "exec: [false]"
	dies.Probe = service.Probe{Kind: service.ProbeExec, Argv: []string{"false"}}
	failed := next(old, dies)
	ev, collect := drain(t)
	rep, err := old.ApplyReload(context.Background(), failed, ev)
	events := collect()
	if !rep.Applied || !errors.Is(err, ErrNotStarted) {
		t.Fatalf("ApplyReload = %+v, %v; want applied with ErrNotStarted", rep, err)
	}
	if !hasMsg(events, "failed") {
		t.Errorf("the start failure never reached the event stream: %v", msgs(events))
	}

	fixed := next(failed, sleepService("a", dir))
	stopAll(t, fixed, old, failed)
	if _, err := failed.ApplyReload(context.Background(), fixed, nil); err != nil {
		t.Fatalf("second ApplyReload: %v", err)
	}
	if pid := pidOf(t, st, "a"); pid <= 0 || !state.Alive(pid) {
		t.Error("the service the first reload took down was not started by the second")
	}
}

// The reload ignores cancellation: an abandoned reload is a stack running
// neither definition.
func TestApplyReloadIgnoresCancellation(t *testing.T) {
	dir := t.TempDir()
	old, st := reloadFixture(t, sleepService("a", dir))
	pidA := pidOf(t, st, "a")
	a := sleepService("a", dir)
	a.Env = append(a.Env, "RELOAD_VALUE=changed")
	nxt := next(old, a)
	stopAll(t, nxt, old)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := old.ApplyReload(ctx, nxt, nil); err != nil {
		t.Fatalf("ApplyReload: %v", err)
	}
	if pid := pidOf(t, st, "a"); pid == pidA || pid <= 0 || !state.Alive(pid) {
		t.Errorf("a = pid %d (was %d): a cancelled caller left the reload half applied", pid, pidA)
	}
}

// docs/LANDMINES.md §17: a readiness-gated dependant of an already running,
// READY dependency was skipped as "running but declares no probe".
func TestRestartStartsAGatedDependantOfAReadyRunningDependency(t *testing.T) {
	dir := t.TempDir()
	gated := sleepService("gated", dir)
	gated.DependsOn = []string{"base"}
	gated.DependsReadyOn = []string{"base"}
	sup, st := reloadFixture(t, readyProbeService("base", dir), gated)
	stopAll(t, sup)

	ev, collect := drain(t)
	if err := sup.Restart(context.Background(), []string{"gated"}, ev); err != nil {
		t.Fatalf("restart gated: %v (events %v)", err, msgs(collect()))
	}
	events := collect()
	if pid := pidOf(t, st, "gated"); pid <= 0 || !state.Alive(pid) {
		t.Fatalf("gated is not running after a restart behind a ready base; events = %v", msgs(events))
	}
	for _, e := range events {
		if e.Service == "base" && strings.Contains(e.Msg, "already running") && e.Phase != PhaseReady {
			t.Errorf("already-running base reported %q, want %q", e.Phase, PhaseReady)
		}
	}
}

// docs/LANDMINES.md §18: an interrupted stop used to escalate to SIGKILL the
// moment its context was cancelled. SIGKILL is earned by the grace running
// out, never by the waiter going away.
func TestCancelledStopNeverEscalatesToSIGKILL(t *testing.T) {
	in := sleepService("stubborn", t.TempDir())
	in.Env = helperEnvFor("ignoreterm")
	sup, st := reloadFixture(t, in)
	stopAll(t, sup)
	pid := pidOf(t, st, "stubborn")
	// The child must have installed its handler, or SIGTERM alone kills it and
	// the test passes for the wrong reason.
	waitForLog(t, st, "stubborn", "ignoring SIGTERM")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	ev, collect := drain(t)
	started := time.Now()
	_ = sup.Stop(ctx, []string{"stubborn"}, ev)
	events := collect()

	if elapsed := time.Since(started); elapsed >= sup.stopGrace() {
		t.Errorf("an interrupted stop waited %s, the whole grace", elapsed)
	}
	if !state.Alive(pid) {
		t.Fatal("the process is gone: a cancelled stop escalated to SIGKILL")
	}
	if !hasMsg(events, "not escalating") {
		t.Errorf("the interruption was not reported; events = %v", msgs(events))
	}
	if got := pidOf(t, st, "stubborn"); got != pid {
		t.Errorf("pid file = %d, want it still naming %d", got, pid)
	}
}

// A reset whose stop was interrupted must not wipe the pid files of services
// it never took down.
func TestInterruptedResetKeepsTheStateDirectory(t *testing.T) {
	sup, st := reloadFixture(t, sleepService("a", t.TempDir()))
	stopAll(t, sup)
	pid := pidOf(t, st, "a")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sup.Reset(ctx, false, nil); err == nil {
		t.Fatal("an interrupted reset reported success")
	}
	if got := pidOf(t, st, "a"); got != pid || !state.Alive(pid) {
		t.Errorf("pid file = %d (want %d, alive): the reset orphaned a running service", got, pid)
	}
}

// A stop that finds no pid file while another mabo-ctl holds the start claim
// must say so: that start is about to spawn the service this stop was asked
// to take down.
func TestStopReportsAStartInFlightElsewhere(t *testing.T) {
	sup, st := fixture(t, sleepService("a", t.TempDir()))
	if _, err := st.ClaimPID("a", os.Getppid(), time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	ev, collect := drain(t)
	_ = sup.Stop(context.Background(), []string{"a"}, ev)
	events := collect()
	if !hasMsg(events, "being started by another mabo-ctl") {
		t.Errorf("stop reported %v; want it to name the start in flight", msgs(events))
	}
	for _, e := range events {
		if e.Msg == "not running" {
			t.Error(`stop still said "not running" while another start held the claim`)
		}
	}
}

// waitForLog polls svc's log until it contains want.
func waitForLog(t *testing.T, st *state.Dir, svc, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(st.LogPath(svc)); err == nil && strings.Contains(string(b), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s's log never said %q", svc, want)
}
