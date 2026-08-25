package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// healthyHarness is a repo with no state on disk: doctor's happy path, where
// every line is ok. The ports are deliberately obscure — the fixture's
// 7100-range ports are exactly what a developer's real stack listens on, and
// doctor would honestly report that as a finding.
func healthyHarness(t *testing.T, args ...string) *harness {
	t.Helper()
	const cfg = `
services:
  - name: alpha
    port: 47291
    cmd: [echo, alpha]
  - name: beta
    port: 47292
    cmd: [echo, beta]
  - name: gamma
    cmd: [echo, gamma]
`
	return newHarnessWithConfig(t, cfg, args...)
}

// TestDoctorHealthyStackIsAllOk: a resolved config with no state on disk has
// nothing to report, and exits 0.
func TestDoctorHealthyStackIsAllOk(t *testing.T) {
	h := healthyHarness(t, "doctor")

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	out := h.stdout.String()
	if !strings.Contains(out, "everything checks out") {
		t.Fatalf("the summary line is missing:\n%s", out)
	}
	if strings.Contains(out, "FAIL") || strings.Contains(out, "warn") {
		t.Fatalf("a healthy stack produced findings:\n%s", out)
	}
}

// TestDoctorStalePidFileWarnsButExitsZero: a pid file naming a dead process is
// the reboot leftover every laptop accumulates. It wants a look, not a failure.
func TestDoctorStalePidFileWarnsButExitsZero(t *testing.T) {
	h := healthyHarness(t, "doctor")
	mkdir(t, filepath.Join(h.root, ".dev", "pids"))
	// A pid that is guaranteed dead: one we spawned and waited for.
	dead := spawnAndReap(t)
	writeFile(t, filepath.Join(h.root, ".dev", "pids", "alpha.pid"),
		`{"pid":`+strconv.Itoa(dead)+`,"started_at":"`+time.Now().UTC().Format(time.RFC3339Nano)+`"}`)

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d — a warning must not fail (stderr: %s)", code, exitOK, h.stderr)
	}
	if out := h.stdout.String(); !strings.Contains(out, "stale pid file") {
		t.Fatalf("the stale pid file was not reported:\n%s", out)
	}
}

// TestDoctorRecycledPidFails: pid 1 is alive on every machine and is never one
// of ours, so it is the deterministic stand-in for a recycled pid. Signalling
// it would be a mistake, and doctor must say so with exit 1.
func TestDoctorRecycledPidFails(t *testing.T) {
	h := healthyHarness(t, "doctor")
	mkdir(t, filepath.Join(h.root, ".dev", "pids"))
	writeFile(t, filepath.Join(h.root, ".dev", "pids", "alpha.pid"),
		`{"pid":1,"started_at":"`+time.Now().UTC().Format(time.RFC3339Nano)+`"}`)

	if code := h.run(); code != exitFailure {
		t.Fatalf("exit code = %d, want %d (stdout: %s)", code, exitFailure, h.stdout)
	}
	if out := h.stdout.String(); !strings.Contains(out, "FAIL") {
		t.Fatalf("the recycled pid was not reported as a failure:\n%s", out)
	}
}

// TestDoctorCrashEvidenceWarns: an exit record that was not a deliberate stop
// stays a warning even with no pid file at all, because "it crashed and nobody
// looked" is exactly the question doctor exists to answer.
func TestDoctorCrashEvidenceWarns(t *testing.T) {
	h := healthyHarness(t, "doctor")
	mkdir(t, filepath.Join(h.root, ".dev", "exits"))
	writeFile(t, filepath.Join(h.root, ".dev", "exits", "alpha.json"),
		`{"pid":424242,"exit_code":1,"started_at":"`+time.Now().UTC().Format(time.RFC3339Nano)+`","ended_at":"`+time.Now().UTC().Format(time.RFC3339Nano)+`"}`)

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	if out := h.stdout.String(); !strings.Contains(out, "exited abnormally") {
		t.Fatalf("the crash record was not reported:\n%s", out)
	}
}

// TestDoctorLooseStatePermissionsWarn: a log the group can read is how a
// credential a child printed leaves the machine in a backup.
func TestDoctorLooseStatePermissionsWarn(t *testing.T) {
	h := healthyHarness(t, "doctor")
	logDir := filepath.Join(h.root, ".dev", "logs")
	mkdir(t, logDir)
	writeFile(t, filepath.Join(logDir, "alpha.log"), "secret=hunter2\n")
	if err := os.Chmod(filepath.Join(logDir, "alpha.log"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	if out := h.stdout.String(); !strings.Contains(out, "mode 644") {
		t.Fatalf("the loose log permissions were not reported:\n%s", out)
	}
}

// TestDoctorTakesNoArguments.
func TestDoctorTakesNoArguments(t *testing.T) {
	h := newHarness(t, "doctor", "backend")
	if code := h.run(); code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
}

// spawnAndReap starts a real process and waits for it, returning a pid that is
// provably no longer alive.
func spawnAndReap(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a process here: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	return pid
}
