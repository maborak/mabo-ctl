package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestActivatePrintsAHookForEachShell(t *testing.T) {
	for _, sh := range []string{"zsh", "bash", "fish"} {
		h := newHarness(t, "activate", sh)
		if code := h.run(); code != exitOK {
			t.Fatalf("%s: exit %d (%s)", sh, code, h.stderr)
		}
		if out := h.stdout.String(); !strings.Contains(out, "mabo-ctl hook-status --last-root") {
			t.Errorf("%s hook does not call hook-status:\n%s", sh, out)
		}
	}
	if code := newHarness(t, "activate", "tcsh").run(); code != exitUsage {
		t.Errorf("activate tcsh exit %d, want %d", code, exitUsage)
	}
}

// TestHookStatusIsReadOnlyAndSpeaksOnlyOnChange: the hook runs on every cd,
// so it must never create .mabo-ctl/, and it prints the summary only when the
// repository changed.
func TestHookStatusIsReadOnlyAndSpeaksOnlyOnChange(t *testing.T) {
	h := newHarness(t, "hook-status")
	if code := h.run(); code != exitOK {
		t.Fatalf("exit %d (%s)", code, h.stderr)
	}
	lines := strings.Split(strings.TrimSpace(h.stdout.String()), "\n")
	if len(lines) != 2 || lines[0] != h.root || !strings.Contains(lines[1], "nothing started here yet") {
		t.Errorf("first visit printed %q, want the root and the summary", lines)
	}
	if _, err := os.Stat(filepath.Join(h.root, ".mabo-ctl")); !os.IsNotExist(err) {
		t.Errorf("hook-status created .mabo-ctl/ (stat err %v)", err)
	}

	again := newHarnessAt(t, h.root, "hook-status", "--last-root", h.root)
	if code := again.run(); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if got := strings.TrimSpace(again.stdout.String()); got != h.root {
		t.Errorf("same repo printed %q, want only the root", got)
	}

	outside := newHarnessAt(t, t.TempDir(), "hook-status")
	if code := outside.run(); code != exitOK || outside.stdout.String() != "\n" {
		t.Errorf("outside a repo: exit %d, output %q; want an empty root line", code, outside.stdout.String())
	}
}
