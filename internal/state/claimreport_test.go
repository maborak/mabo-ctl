package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestClaimPIDReportsTheEviction pins the observability fix for stale-claim
// clearing: a refusal names the holder, and now so does a clear. An operator
// must be able to tell "nothing was there" from "I overtook another
// mabo-ctl's wreckage", because before hooks were bounded the latter was
// exactly the situation a wedged start produced.
func TestClaimPIDReportsTheEviction(t *testing.T) {
	d := testDir(t)

	dead := deadPID(t)
	if _, err := d.ClaimPID("svc", dead, time.Now()); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	rep, err := d.ClaimPID("svc", os.Getpid(), time.Now())
	if err != nil {
		t.Fatalf("second claim over a dead owner's claim: %v", err)
	}
	if !rep.ClearedStale {
		t.Fatal("ClaimPID did not report the eviction it performed")
	}
	if rep.PrevPID != dead {
		t.Errorf("PrevPID = %d, want the evicted claim's owner %d", rep.PrevPID, dead)
	}
	if rep.PrevWhy == "" {
		t.Error("PrevWhy is empty; the eviction must say why the claim was judged stale")
	}
}

// TestClaimPIDCleanTakeReportsNothing pins the other half: a start that finds
// no claim on arrival must NOT read as an eviction.
func TestClaimPIDCleanTakeReportsNothing(t *testing.T) {
	d := testDir(t)
	rep, err := d.ClaimPID("svc", os.Getpid(), time.Now())
	if err != nil {
		t.Fatalf("clean claim: %v", err)
	}
	if rep.ClearedStale {
		t.Errorf("clean take reported an eviction: %+v", rep)
	}
}

// TestClaimPIDStillRefusesAFreshClaim keeps the refusal contract visible next
// to the new reporting: a LIVE owner's fresh claim is not wreckage and must
// still be refused.
func TestClaimPIDStillRefusesAFreshClaim(t *testing.T) {
	d := testDir(t)
	if _, err := d.ClaimPID("svc", os.Getpid(), time.Now()); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := d.ClaimPID("svc", os.Getpid(), time.Now().Add(time.Second)); !errors.Is(err, ErrClaimed) {
		t.Errorf("second claim over a live owner = %v, want ErrClaimed", err)
	}
}

// testDir builds a state dir in a temp root.
func testDir(t *testing.T) *Dir {
	t.Helper()
	d, err := New(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}
