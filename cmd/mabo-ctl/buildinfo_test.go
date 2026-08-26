package main

import (
	"runtime/debug"
	"strings"
	"testing"
)

// vcsgo builds a *debug.BuildInfo the way a Go 1.18+ toolchain embeds one for
// a git checkout: VCS state, cgo mode, one dependency and one replaced
// dependency so the report's rendering rules are all exercised at once.
func vcsBI(revision, modified, when, cgo string) *debug.BuildInfo {
	return &debug.BuildInfo{
		Main: debug.Module{Path: "github.com/maborak/mabo-ctl"},
		Deps: []*debug.Module{
			{Path: "github.com/spf13/cobra", Version: "v1.9.1"},
			{Path: "fork.local/lib", Version: "v2.0.0", Replace: &debug.Module{Path: "./local/lib", Version: "v0.3.0"}},
		},
		Settings: []debug.BuildSetting{
			{Key: "-buildmode", Value: "exe"},
			{Key: "-compiler", Value: "gc"},
			{Key: "CGO_ENABLED", Value: cgo},
			{Key: "GOARCH", Value: "arm64"},
			{Key: "GOOS", Value: "darwin"},
			{Key: "vcs.modified", Value: modified},
			{Key: "vcs.revision", Value: revision},
			{Key: "vcs.time", Value: when},
		},
	}
}

func TestResolveBuildFillsFromVCS(t *testing.T) {
	t.Parallel()
	const rev = "0123456789abcdef0123456789abcdef01234567"

	b := resolveBuild("dev", "unknown", "unknown",
		vcsBI(rev, "true", "2026-08-26T00:00:00Z", "0"))

	if b.Version != "dev" {
		// The version must never become sha-shaped: upgrade compares release
		// tags only, and a source build is deliberately not one of those.
		t.Fatalf("version = %q, want the unstamped \"dev\" to stand", b.Version)
	}
	if b.Commit != "0123456789ab" {
		t.Errorf("commit = %q, want the abbreviated revision %q", b.Commit, rev[:12])
	}
	if !b.Dirty {
		t.Error("dirty = false, want true from vcs.modified=true")
	}
	if b.BuiltAt != "2026-08-26T00:00:00Z" {
		t.Errorf("builtAt = %q, want the embedded vcs.time", b.BuiltAt)
	}
	if b.CGO != "disabled" {
		t.Errorf("cgo = %q, want \"disabled\"", b.CGO)
	}
	if b.Module != "github.com/maborak/mabo-ctl (devel)" {
		t.Errorf("module = %q, want path with (devel)", b.Module)
	}
	if len(b.Settings) == 0 || len(b.Deps) != 2 {
		t.Fatalf("settings = %d entries, deps = %d entries; want both preserved", len(b.Settings), len(b.Deps))
	}
	wantDep := "fork.local/lib@v2.0.0 (replaced by ./local/lib@v0.3.0)"
	if got := b.Deps[1]; got != wantDep {
		t.Errorf("dep line = %q, want %q", got, wantDep)
	}
}

func TestResolveBuildStampsWinOverVCS(t *testing.T) {
	t.Parallel()
	b := resolveBuild("v1.2.3", "fedcba987654", "2026-01-01T09:30:00Z",
		vcsBI("00000000000000000000000000000000000000ff", "false", "2025-01-01T00:00:00Z", "1"))

	if b.Version != "v1.2.3" || b.Commit != "fedcba987654" || b.BuiltAt != "2026-01-01T09:30:00Z" {
		t.Fatalf("resolved %+v; the Makefile stamps must outrank the embedded VCS metadata", b)
	}
	if b.Dirty {
		t.Error("dirty = true from an old vcs record, want false; stamps mean this binary came from elsewhere")
	}
	if b.CGO != "enabled" {
		t.Errorf("cgo = %q, want \"enabled\"", b.CGO)
	}
}

func TestResolveBuildWithoutEmbeddedInfo(t *testing.T) {
	t.Parallel()
	b := resolveBuild("", "", "", nil)

	// Nothing anywhere knows anything: sentinels survive, never panic on the
	// way to a readable summary.
	if b.Commit != "unknown" || b.BuiltAt != "unknown" || b.CGO != "unknown" {
		t.Fatalf("resolved %+v, want unknown sentinels preserved", b)
	}
	if s := b.Summary(); !strings.Contains(s, "commit unknown") || !strings.Contains(s, "built unknown") {
		t.Fatalf("summary without any provenance = %q", s)
	}
}

func TestShortRevision(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"", ""},
		{"abc", "abc"},
		{"0123456789abcdef", "0123456789ab"}, // twelve characters, git style
	}
	for _, tc := range tests {
		if got := shortRevision(tc.in); got != tc.want {
			t.Errorf("shortRevision(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestVersionFlagPrintsFullReport covers the wiring end to end: --version in
// a directory with no mabo-ctl.yaml prints the complete self-report, which is
// what SECURITY.md asks a bug reporter to paste.
func TestVersionFlagPrintsFullReport(t *testing.T) {
	h := newHarnessAt(t, t.TempDir(), "--version")
	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}

	out := h.stdout.String()
	for _, want := range []string{
		"(commit ", ", built ", "platform: ", "cgo: ", "\nbuild settings:\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("--version output is missing %q:\n%s", want, out)
		}
	}
	if h.console != 0 {
		t.Fatal("--version opened the console instead of reporting")
	}
}
