//go:build unix

package state

import (
	"os"
	"os/exec"
	"testing"
)

func TestAliveSelf(t *testing.T) {
	t.Parallel()
	if !Alive(os.Getpid()) {
		t.Errorf("Alive(%d) = false for this very process, want true", os.Getpid())
	}
}

func TestAliveForeignProcess(t *testing.T) {
	t.Parallel()
	// pid 1 exists on every Unix and is not ours: signal 0 fails with EPERM,
	// which must still count as alive. Mistaking EPERM for "not running" would
	// let mabo-ctl treat a foreign port holder as a dead service.
	if !Alive(1) {
		t.Error("Alive(1) = false, want true (EPERM means it exists but is not ours)")
	}
}

func TestAliveDeadProcess(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a helper process: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper process: %v", err)
	}
	if Alive(pid) {
		t.Errorf("Alive(%d) = true for a reaped process, want false", pid)
	}
}

func TestAliveNonPositivePID(t *testing.T) {
	t.Parallel()
	// Signal 0 to pid 0 addresses the caller's whole process group and a
	// negative pid addresses another group, so neither may reach the kernel.
	for _, pid := range []int{0, -1, -os.Getpid()} {
		if Alive(pid) {
			t.Errorf("Alive(%d) = true, want false", pid)
		}
	}
}
