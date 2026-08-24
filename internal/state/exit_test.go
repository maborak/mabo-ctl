package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sampleExit is a fully populated record: every field carries a distinguishable
// value so a round trip that drops or transposes one is visible.
func sampleExit() ExitRecord {
	return ExitRecord{
		PID:       4242,
		ExitCode:  -1,
		Signal:    "SIGKILL",
		StartedAt: time.Date(2026, 8, 16, 9, 12, 0, 0, time.UTC),
		EndedAt:   time.Date(2026, 8, 16, 9, 16, 30, 0, time.UTC),
		LogTail:   "Traceback (most recent call last):\n  RuntimeError: boom\n",
		Stopped:   false,
	}
}

func TestExitRoundTrip(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	want := sampleExit()

	if err := d.WriteExit("backend", want); err != nil {
		t.Fatalf("WriteExit: %v", err)
	}
	got, ok, err := d.ReadExit("backend")
	if err != nil {
		t.Fatalf("ReadExit: %v", err)
	}
	if !ok {
		t.Fatal("ReadExit reported no record after WriteExit")
	}
	if got.PID != want.PID || got.ExitCode != want.ExitCode || got.Signal != want.Signal {
		t.Errorf("ReadExit identity = %+v, want %+v", got, want)
	}
	if !got.StartedAt.Equal(want.StartedAt) || !got.EndedAt.Equal(want.EndedAt) {
		t.Errorf("ReadExit times = (%s, %s), want (%s, %s)",
			got.StartedAt, got.EndedAt, want.StartedAt, want.EndedAt)
	}
	if got.LogTail != want.LogTail {
		t.Errorf("ReadExit LogTail = %q, want %q", got.LogTail, want.LogTail)
	}
	if got.Stopped != want.Stopped {
		t.Errorf("ReadExit Stopped = %v, want %v", got.Stopped, want.Stopped)
	}

	// A rewrite replaces the record; there is only ever one run described.
	second := ExitRecord{PID: 99, ExitCode: 0, Stopped: true}
	if err := d.WriteExit("backend", second); err != nil {
		t.Fatalf("WriteExit (rewrite): %v", err)
	}
	got, ok, err = d.ReadExit("backend")
	if err != nil || !ok || got.PID != 99 || !got.Stopped {
		t.Fatalf("ReadExit after rewrite = (%+v, %v, %v), want the second record", got, ok, err)
	}

	if err := d.RemoveExit("backend"); err != nil {
		t.Fatalf("RemoveExit: %v", err)
	}
	if _, ok, err = d.ReadExit("backend"); err != nil || ok {
		t.Fatalf("ReadExit after remove = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestReadExitAbsentIsNotAnError(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	rec, ok, err := d.ReadExit("never-crashed")
	if err != nil {
		t.Fatalf("ReadExit of absent record: %v, want nil (absent means no observed death)", err)
	}
	if ok {
		t.Error("ReadExit of absent record reported ok = true")
	}
	if rec != (ExitRecord{}) {
		t.Errorf("ReadExit of absent record = %+v, want the zero record", rec)
	}
}

func TestReadExitCorrupt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		contents string
	}{
		{"empty", ""},
		{"whitespace", "  \n\t"},
		{"truncated object", `{"pid":42,`},
		{"not json", "SIGKILL at 09:16\n"},
		{"wrong type for pid", `{"pid":"forty-two"}`},
		{"json array", `[{"pid":42}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := newDir(t)
			if err := os.WriteFile(d.ExitPath("backend"), []byte(c.contents), 0o600); err != nil {
				t.Fatalf("seed exit record: %v", err)
			}
			rec, ok, err := d.ReadExit("backend")
			if err == nil {
				t.Fatalf("ReadExit(%q) = (%+v, %v, nil), want ErrMalformedExit", c.contents, rec, ok)
			}
			if !errors.Is(err, ErrMalformedExit) {
				t.Errorf("ReadExit(%q) error = %v, want it to wrap ErrMalformedExit", c.contents, err)
			}
			// A corrupt record must not be reported as "present" either: the
			// caller would render fields it never read.
			if ok {
				t.Errorf("ReadExit(%q) reported ok = true alongside the error", c.contents)
			}
			if rec != (ExitRecord{}) {
				t.Errorf("ReadExit(%q) = %+v, want the zero record alongside the error", c.contents, rec)
			}
		})
	}
}

func TestExitFileIsJSONWithTheDocumentedKeys(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	if err := d.WriteExit("backend", sampleExit()); err != nil {
		t.Fatalf("WriteExit: %v", err)
	}
	b, err := os.ReadFile(d.ExitPath("backend"))
	if err != nil {
		t.Fatalf("read exit record: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("exit record is not JSON: %v (%q)", err, b)
	}
	for _, k := range []string{"pid", "exit_code", "signal", "started_at", "ended_at", "log_tail", "stopped"} {
		if _, present := raw[k]; !present {
			t.Errorf("exit record has no %q key: %q", k, b)
		}
	}
}

func TestExitFilesAndDirectoryAreNotReadableByOthers(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	if got := mode(t, d.ExitsDir()); got != 0o700 {
		t.Errorf("mode(exits) = %04o, want 0700", got)
	}
	if err := d.WriteExit("backend", sampleExit()); err != nil {
		t.Fatalf("WriteExit: %v", err)
	}
	// The record embeds a log tail, so a group-readable file discloses whatever
	// the service printed on its way out.
	if got := mode(t, d.ExitPath("backend")); got != 0o600 {
		t.Errorf("exit record mode = %04o, want 0600", got)
	}
}

func TestWriteExitTightensExistingPermissions(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	p := d.ExitPath("backend")
	if err := os.WriteFile(p, []byte(`{"pid":1}`), 0o644); err != nil {
		t.Fatalf("seed exit record: %v", err)
	}
	if err := d.WriteExit("backend", sampleExit()); err != nil {
		t.Fatalf("WriteExit: %v", err)
	}
	if got := mode(t, p); got != 0o600 {
		t.Errorf("exit record mode = %04o after rewrite, want 0600", got)
	}
}

func TestRemoveExitAbsentIsNotAnError(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	if err := d.RemoveExit("backend"); err != nil {
		t.Fatalf("RemoveExit of absent record: %v, want nil", err)
	}
}

func TestExitPathHelpers(t *testing.T) {
	t.Parallel()
	d := &Dir{Root: "/repo"}
	if got, want := d.ExitsDir(), "/repo/.dev/exits"; got != want {
		t.Errorf("ExitsDir() = %q, want %q", got, want)
	}
	if got, want := d.ExitPath("backend"), "/repo/.dev/exits/backend.json"; got != want {
		t.Errorf("ExitPath() = %q, want %q", got, want)
	}
}

func TestUnsafeServiceNamesAreRejectedByTheExitAPI(t *testing.T) {
	t.Parallel()
	names := []string{"", "../evil", "..", ".", "a/b", `a\b`, "-leading-dash", "has space", "nul\x00byte"}
	for _, name := range names {
		t.Run(strings.ReplaceAll(name, "/", "_"), func(t *testing.T) {
			t.Parallel()
			d := newDir(t)
			if err := d.WriteExit(name, sampleExit()); !errors.Is(err, ErrInvalidService) {
				t.Errorf("WriteExit(%q) error = %v, want ErrInvalidService", name, err)
			}
			if _, _, err := d.ReadExit(name); !errors.Is(err, ErrInvalidService) {
				t.Errorf("ReadExit(%q) error = %v, want ErrInvalidService", name, err)
			}
			if err := d.RemoveExit(name); !errors.Is(err, ErrInvalidService) {
				t.Errorf("RemoveExit(%q) error = %v, want ErrInvalidService", name, err)
			}
			// The name composes a path; nothing may have escaped `.dev/`.
			entries, err := os.ReadDir(d.Root)
			if err != nil {
				t.Fatalf("read root: %v", err)
			}
			if len(entries) != 1 || entries[0].Name() != ".dev" {
				t.Errorf("root contains %v, want only .dev", entries)
			}
		})
	}
}

func TestResetClearsExitRecords(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	d, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.WriteExit("backend", sampleExit()); err != nil {
		t.Fatalf("WriteExit: %v", err)
	}
	if err := d.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if _, err := os.Stat(d.ExitsDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat(%s) after Reset = %v, want ErrNotExist", d.ExitsDir(), err)
	}
	// And the record is genuinely gone as far as the API is concerned, not just
	// its directory.
	if _, err := New(root); err != nil {
		t.Fatalf("New after Reset: %v", err)
	}
	if _, ok, err := d.ReadExit("backend"); err != nil || ok {
		t.Errorf("ReadExit after Reset = (%v, %v), want (false, nil)", ok, err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, ".dev", exitsDirName)); err != nil {
		t.Fatalf("read exits dir: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("exits dir contains %v after Reset, want it empty", entries)
	}
}
