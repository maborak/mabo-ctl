package main

import (
	"context"
	"errors"
	"testing"
)

// upgradeSeam records the one call the command may make and lets each test
// decide what the real upgrader would have done.
type upgradeSeam struct {
	calls int
	force bool
}

// run is the Env.RunUpgrader shape.
func (s *upgradeSeam) run(_ context.Context, force bool) error {
	s.calls++
	s.force = force
	return nil
}

// TestUpgradeRoutesThroughTheSeamAndNeedsNoConfig: `upgrade` must work in a
// directory with no mabo-ctl.yaml — it replaces a binary, it does not resolve
// services — and must hand the --force decision through.
func TestUpgradeRoutesThroughTheSeamAndNeedsNoConfig(t *testing.T) {
	h := newHarnessAt(t, t.TempDir(), "upgrade") // no mabo-ctl.yaml here
	var seam upgradeSeam
	h.env.RunUpgrader = seam.run

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	if seam.calls != 1 {
		t.Fatalf("the upgrader ran %d times, want exactly once", seam.calls)
	}
	if seam.force {
		t.Error("force = true without the flag")
	}
}

// TestUpgradeForceFlagPassesThrough.
func TestUpgradeForceFlagPassesThrough(t *testing.T) {
	h := newHarnessAt(t, t.TempDir(), "upgrade", "--force")
	var seam upgradeSeam
	h.env.RunUpgrader = seam.run

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	if !seam.force {
		t.Error("--force did not reach the upgrader")
	}
}

// TestUpgradeFailureExitsOne: a failed upgrade is a runtime failure, not a
// usage error, and its message lands on stderr.
func TestUpgradeFailureExitsOne(t *testing.T) {
	h := newHarnessAt(t, t.TempDir(), "upgrade")
	h.env.RunUpgrader = func(context.Context, bool) error { return errors.New("no release published yet") }

	if code := h.run(); code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	if msg := h.stderr.String(); msg == "" {
		t.Error("the failure was silent; an upgrade that did nothing must say why")
	}
}

// TestUpgradeRejectsArguments: services are not arguments to upgrade.
func TestUpgradeRejectsArguments(t *testing.T) {
	h := newHarnessAt(t, t.TempDir(), "upgrade", "backend")
	h.env.RunUpgrader = func(context.Context, bool) error { return nil }

	if code := h.run(); code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
}
