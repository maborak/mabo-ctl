package main

import (
	"strings"
	"testing"
)

// profileFixture declares one always-on service and one tied to the "mail"
// profile, so a filter either keeps both (no selection), drops exactly one
// (mail not active), or keeps exactly the always-on plus mail.
const profileFixture = `
services:
  - name: api
    cmd: [sleep, 100]
    port: 7310
  - name: mailpit
    cmd: [sleep, 100]
    profiles: [mail]
`

func TestProfilesWithoutSelectionKeepsEverything(t *testing.T) {
	t.Parallel()
	h := newHarnessWithConfig(t, profileFixture, "config", "--json")
	h.sup.statuses = readyStatuses()
	if code := h.run(); code != exitOK {
		t.Fatalf("exit = %d, stderr %s", code, h.stderr)
	}
	for _, want := range []string{"api", "mailpit"} {
		if !strings.Contains(h.stdout.String(), want) {
			t.Errorf("config output missing %s with no profile selection", want)
		}
	}
}

func TestProfilesExcludeWhenNotActive(t *testing.T) {
	t.Parallel()
	h := newHarnessWithConfig(t, profileFixture, "config", "--json", "--profile", "search")
	h.sup.statuses = readyStatuses()
	if code := h.run(); code != exitOK {
		t.Fatalf("exit = %d, stderr %s", code, h.stderr)
	}
	if out := h.stdout.String(); strings.Contains(out, "mailpit") {
		t.Errorf("mailpit leaked past --profile search: %q", out)
	}
	if !strings.Contains(h.stdout.String(), "api") {
		t.Errorf("always-on service api was filtered out too: %q", h.stdout)
	}
}

func TestProfilesIncludeOnOverlap(t *testing.T) {
	t.Parallel()
	h := newHarnessWithConfig(t, profileFixture, "config", "--json", "--profile", "search,mail")
	h.sup.statuses = readyStatuses()
	if code := h.run(); code != exitOK {
		t.Fatalf("exit = %d, stderr %s", code, h.stderr)
	}
	if out := h.stdout.String(); !strings.Contains(out, "mailpit") || !strings.Contains(out, "api") {
		t.Errorf("overlap run should keep both services: %q", out)
	}
}

func TestProfilesExcludingEverythingIsAConfigError(t *testing.T) {
	t.Parallel()
	h := newHarnessWithConfig(t, `
services:
  - name: mailpit
    cmd: [sleep, 100]
    profiles: [mail]
  - name: worker
    cmd: [sleep, 100]
    profiles: [jobs]
`, "status", "--profile", "search")
	h.sup.statuses = readyStatuses()
	if code := h.run(); code != exitConfig {
		t.Fatalf("exit = %d, want %d for a run with nothing left", code, exitConfig)
	}
	if !strings.Contains(h.stderr.String(), "mail") {
		t.Errorf("error should name the declared profiles; stderr: %s", h.stderr)
	}
}

func TestProfilesFromEnvWhenFlagAbsent(t *testing.T) {
	// t.Setenv forbids Parallel.
	t.Setenv("MABO_PROFILE", "mail")
	h := newHarnessWithConfig(t, profileFixture, "config", "--json")
	h.sup.statuses = readyStatuses()
	if code := h.run(); code != exitOK {
		t.Fatalf("exit = %d, stderr %s", code, h.stderr)
	}
	if out := h.stdout.String(); !strings.Contains(out, "mailpit") || !strings.Contains(out, "api") {
		t.Errorf("MABO_PROFILE=mail should keep both services: %q", out)
	}
}

func TestProfilesFlagBeatsEnv(t *testing.T) {
	t.Setenv("MABO_PROFILE", "mail")
	h := newHarnessWithConfig(t, profileFixture, "config", "--json", "--profile", "search")
	h.sup.statuses = readyStatuses()
	if code := h.run(); code != exitOK {
		t.Fatalf("exit = %d, stderr %s", code, h.stderr)
	}
	if out := h.stdout.String(); strings.Contains(out, "mailpit") {
		t.Errorf("flag should have won over env and dropped mailpit: %q", out)
	}
}
