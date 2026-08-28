package supervisor

import (
	"errors"
	"os/exec"
	"testing"
	"time"
)

// startLongLivedProcess spawns a real process that outlives the test —
// detached exactly the way startOne detaches, so it is its own group leader —
// and returns it. The caller's deferred cleanup kills and waits for it.
func startLongLivedProcess(t *testing.T) (*exec.Cmd, func()) {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "30")
	setDetached(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	return cmd, func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}

// TestVerifyGroupAcceptsAnHonestRecord pins the happy path: a live process we
// really did spawn at the recorded moment passes all three checks.
func TestVerifyGroupAcceptsAnHonestRecord(t *testing.T) {
	cmd, cleanup := startLongLivedProcess(t)
	defer cleanup()
	if _, err := verifyGroup(cmd.Process.Pid, time.Now()); err != nil {
		t.Fatalf("verifyGroup(pid, time of spawn) = %v, want nil", err)
	}
	// A legacy record with no spawn time skips the kernel comparison rather
	// than fail every stop of a stack managed by an older binary.
	if _, err := verifyGroup(cmd.Process.Pid, time.Time{}); err != nil {
		t.Fatalf("verifyGroup(pid, zero time) = %v, want nil", err)
	}
}

// TestVerifyGroupRefusesARecycledGroupLeader pins the H-1 fix. Setsid makes
// every process we spawn its own group leader — and so is every tmux pane and
// container init. A pid record whose spawn time disagrees with the kernel's
// start time is a recycled pid wearing our shape, and signalling its GROUP was
// exactly the blast radius the structural checks alone could not prevent.
func TestVerifyGroupRefusesARecycledGroupLeader(t *testing.T) {
	cmd, cleanup := startLongLivedProcess(t)
	defer cleanup()

	for name, recorded := range map[string]time.Time{
		"an hour ago":   time.Now().Add(-time.Hour),
		"ten days ago":  time.Now().Add(-10 * 24 * time.Hour),
		"years earlier": time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		if _, err := verifyGroup(cmd.Process.Pid, recorded); !errors.Is(err, ErrUnsafeSignal) {
			t.Errorf("record claiming %s: verifyGroup = %v, want ErrUnsafeSignal", name, err)
		}
	}
}
