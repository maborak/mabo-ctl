package main

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/maborak/mabo-ctl/internal/state"
)

// leaderlessGroup builds the shape a crashed shell wrapper leaves behind: a
// process-group leader that has exited while a child it started still runs
// in its group. It returns the leader's pid (the group id) and the child's.
func leaderlessGroup(t *testing.T) (leader, child int) {
	t.Helper()
	sh := exec.Command("sh", "-c", "sleep 60 >/dev/null 2>&1 & echo $!")
	sh.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := sh.Output() // waits for sh, which reaps the leader
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	child, err = strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("child pid %q: %v", out, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	leader = sh.Process.Pid
	if state.Alive(leader) || !state.Alive(child) {
		t.Fatalf("want leader %d gone and child %d alive", leader, child)
	}
	return leader, child
}

func writeRecord(t *testing.T, root, svc string, pid int) {
	t.Helper()
	mkdir(t, filepath.Join(root, ".mabo-ctl", "pids"))
	writeFile(t, filepath.Join(root, ".mabo-ctl", "pids", svc+".pid"),
		`{"pid":`+strconv.Itoa(pid)+`,"started_at":"`+time.Now().UTC().Format(time.RFC3339Nano)+`"}`)
}

// TestDoctorNamesALiveGroupBehindAnExitedLeader: a record whose leader exited
// while its group lives on is not the stale file a reboot leaves, and doctor
// must not advise clearing it as one.
func TestDoctorNamesALiveGroupBehindAnExitedLeader(t *testing.T) {
	h := healthyHarness(t, "doctor")
	leader, _ := leaderlessGroup(t)
	writeRecord(t, h.root, "alpha", leader)

	if code := h.run(); code != exitOK {
		t.Fatalf("exit %d (stderr: %s)", code, h.stderr)
	}
	out := h.stdout.String()
	if !strings.Contains(out, "still run in its group") || strings.Contains(out, "stale pid file") {
		t.Errorf("doctor did not report the live group:\n%s", out)
	}
}

// TestHookCountsALiveGroupBehindAnExitedLeaderAsRunning: the shell hook shows
// such a service as running, not stopped.
func TestHookCountsALiveGroupBehindAnExitedLeaderAsRunning(t *testing.T) {
	root := t.TempDir()
	leader, _ := leaderlessGroup(t)
	writeRecord(t, root, "api", leader)
	if got := hookSummary(root); got != "api running" {
		t.Errorf("hook summary = %q, want %q", got, "api running")
	}
}
