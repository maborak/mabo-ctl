package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newDir builds a state directory under a throwaway root. Nothing in this
// package's tests may touch a real .dev/.
func newDir(t *testing.T) *Dir {
	t.Helper()
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func mode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

func TestNewCreatesTreeWithRestrictivePermissions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	d, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if want := filepath.Join(root, ".dev"); d.Path() != want {
		t.Errorf("Path() = %q, want %q", d.Path(), want)
	}
	for _, p := range []string{d.Path(), d.LogsDir(), d.PIDsDir()} {
		if got := mode(t, p); got != 0o700 {
			t.Errorf("mode(%s) = %04o, want 0700", p, got)
		}
	}
}

func TestNewIsIdempotentAndTightensLoosePermissions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if _, err := New(root); err != nil {
		t.Fatalf("New: %v", err)
	}
	// Simulate a tree created by an older version with a permissive umask.
	loose := filepath.Join(root, ".dev", "logs")
	if err := os.Chmod(loose, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	d, err := New(root)
	if err != nil {
		t.Fatalf("New (second call): %v", err)
	}
	if got := mode(t, d.LogsDir()); got != 0o700 {
		t.Errorf("mode(logs) = %04o after re-New, want 0700", got)
	}
}

func TestNewRejectsEmptyRoot(t *testing.T) {
	t.Parallel()
	if _, err := New("  "); err == nil {
		t.Fatal("New(\"  \") = nil error, want an error")
	}
}

func TestNewMakesRootAbsolute(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	rel, err := filepath.Rel(wd, root)
	if err != nil {
		t.Skipf("no relative path from %s to %s", wd, root)
	}
	d, err := New(rel)
	if err != nil {
		t.Fatalf("New(%q): %v", rel, err)
	}
	if !filepath.IsAbs(d.Root) {
		t.Errorf("Root = %q, want an absolute path", d.Root)
	}
}

func TestPathHelpers(t *testing.T) {
	t.Parallel()
	d := &Dir{Root: "/repo"}
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"Path", d.Path(), "/repo/.dev"},
		{"LogsDir", d.LogsDir(), "/repo/.dev/logs"},
		{"PIDsDir", d.PIDsDir(), "/repo/.dev/pids"},
		{"RunEnvPath", d.RunEnvPath(), "/repo/.dev/run.env"},
		{"LogPath", d.LogPath("backend"), "/repo/.dev/logs/backend.log"},
		{"PIDPath", d.PIDPath("backend"), "/repo/.dev/pids/backend.pid"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestPIDRoundTrip(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	if err := d.WritePID("backend", 4242); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	pid, err := d.ReadPID("backend")
	if err != nil {
		t.Fatalf("ReadPID: %v", err)
	}
	if pid != 4242 {
		t.Errorf("ReadPID = %d, want 4242", pid)
	}
	if got := mode(t, d.PIDPath("backend")); got != 0o600 {
		t.Errorf("pid file mode = %04o, want 0600", got)
	}
	// A rewrite must replace, not append.
	if err := d.WritePID("backend", 99); err != nil {
		t.Fatalf("WritePID (rewrite): %v", err)
	}
	if pid, err = d.ReadPID("backend"); err != nil || pid != 99 {
		t.Fatalf("ReadPID after rewrite = (%d, %v), want (99, nil)", pid, err)
	}
	if err := d.RemovePID("backend"); err != nil {
		t.Fatalf("RemovePID: %v", err)
	}
	if pid, err = d.ReadPID("backend"); err != nil || pid != 0 {
		t.Fatalf("ReadPID after remove = (%d, %v), want (0, nil)", pid, err)
	}
}

func TestReadPIDAbsentIsNotAnError(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	pid, err := d.ReadPID("never-started")
	if err != nil {
		t.Fatalf("ReadPID of absent file: %v, want nil (absent means not running)", err)
	}
	if pid != 0 {
		t.Errorf("ReadPID of absent file = %d, want 0", pid)
	}
}

func TestReadPIDMalformed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		contents string
	}{
		{"empty", ""},
		{"whitespace", "   \n\t "},
		{"not a number", "not-a-pid\n"},
		{"trailing junk", "123 456\n"},
		{"zero", "0\n"},
		{"negative", "-9\n"},
		{"float", "12.5\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := newDir(t)
			if err := os.WriteFile(d.PIDPath("backend"), []byte(c.contents), 0o600); err != nil {
				t.Fatalf("seed pid file: %v", err)
			}
			pid, err := d.ReadPID("backend")
			if err == nil {
				t.Fatalf("ReadPID(%q) = (%d, nil), want ErrMalformedPID", c.contents, pid)
			}
			if !errors.Is(err, ErrMalformedPID) {
				t.Errorf("ReadPID(%q) error = %v, want it to wrap ErrMalformedPID", c.contents, err)
			}
			if pid != 0 {
				t.Errorf("ReadPID(%q) = %d, want 0 alongside the error", c.contents, pid)
			}
		})
	}
}

func TestReadPIDAcceptsTrailingNewlineAndSpaces(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	if err := os.WriteFile(d.PIDPath("backend"), []byte(" 777 \n"), 0o600); err != nil {
		t.Fatalf("seed pid file: %v", err)
	}
	pid, err := d.ReadPID("backend")
	if err != nil || pid != 777 {
		t.Fatalf("ReadPID = (%d, %v), want (777, nil)", pid, err)
	}
}

func TestWritePIDRejectsNonPositive(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	for _, pid := range []int{0, -1} {
		if err := d.WritePID("backend", pid); err == nil {
			t.Errorf("WritePID(backend, %d) = nil, want an error", pid)
		}
	}
	if _, err := os.Stat(d.PIDPath("backend")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a rejected WritePID created %s", d.PIDPath("backend"))
	}
}

func TestRemovePIDAbsentIsNotAnError(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	if err := d.RemovePID("backend"); err != nil {
		t.Fatalf("RemovePID of absent file: %v, want nil", err)
	}
}

func TestUnsafeServiceNamesAreRejected(t *testing.T) {
	t.Parallel()
	names := []string{
		"",
		"../evil",
		"..",
		".",
		"a/b",
		`a\b`,
		"-leading-dash",
		"_leading_underscore",
		"has space",
		"has.dot",
		"semi;colon",
		"nul\x00byte",
	}
	for _, name := range names {
		t.Run(strings.ReplaceAll(name, "/", "_"), func(t *testing.T) {
			t.Parallel()
			d := newDir(t)
			if err := d.WritePID(name, 5); !errors.Is(err, ErrInvalidService) {
				t.Errorf("WritePID(%q) error = %v, want ErrInvalidService", name, err)
			}
			if _, err := d.ReadPID(name); !errors.Is(err, ErrInvalidService) {
				t.Errorf("ReadPID(%q) error = %v, want ErrInvalidService", name, err)
			}
			if err := d.RemovePID(name); !errors.Is(err, ErrInvalidService) {
				t.Errorf("RemovePID(%q) error = %v, want ErrInvalidService", name, err)
			}
			f, err := d.TruncateLog(name)
			if !errors.Is(err, ErrInvalidService) {
				t.Errorf("TruncateLog(%q) error = %v, want ErrInvalidService", name, err)
			}
			if f != nil {
				t.Errorf("TruncateLog(%q) returned a file", name)
				_ = f.Close()
			}
			// Nothing may have escaped .dev/ — the whole point of the rule.
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

func TestValidServiceNamesAreAccepted(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	for _, name := range []string{"a", "backend", "browser-service", "svc_2", "Web1"} {
		if err := d.WritePID(name, 11); err != nil {
			t.Errorf("WritePID(%q) = %v, want nil", name, err)
		}
	}
}

func TestTruncateLog(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	p := d.LogPath("backend")

	f, err := d.TruncateLog("backend")
	if err != nil {
		t.Fatalf("TruncateLog: %v", err)
	}
	if _, err := f.WriteString("first run\n"); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	if got := mode(t, p); got != 0o600 {
		t.Errorf("log mode = %04o, want 0600", got)
	}

	f2, err := d.TruncateLog("backend")
	if err != nil {
		t.Fatalf("TruncateLog (second): %v", err)
	}
	if _, err := f2.WriteString("second\n"); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if err := f2.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(b) != "second\n" {
		t.Errorf("log = %q, want %q (the previous run must be truncated)", b, "second\n")
	}
}

func TestTruncateLogTightensExistingPermissions(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	p := d.LogPath("backend")
	if err := os.WriteFile(p, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	f, err := d.TruncateLog("backend")
	if err != nil {
		t.Fatalf("TruncateLog: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := mode(t, p); got != 0o600 {
		t.Errorf("log mode = %04o, want 0600 (a service may print a token here)", got)
	}
}

func TestOpenLogAppendAddsToTheRunLogWithoutRotating(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	seed := d.LogPath("backend")
	if err := os.WriteFile(seed, []byte("svc line\n"), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	f, err := d.OpenLogAppend("backend")
	if err != nil {
		t.Fatalf("OpenLogAppend: %v", err)
	}
	if _, err := f.WriteString("hook line\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	body, err := os.ReadFile(seed)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(body) != "svc line\nhook line\n" {
		t.Errorf("log = %q, want the service line kept and the hook line appended", body)
	}
	if got := mode(t, seed); got != 0o600 {
		t.Errorf("log mode = %04o, want 0600 after append", got)
	}
	// The rotation sibling must be untouched: an append is not a start.
	if _, err := os.Stat(seed + ".1"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("OpenLogAppend rotated the log; want the previous run left alone")
	}
}

func TestOpenLogAppendRejectsAnInvalidServiceName(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	if f, err := d.OpenLogAppend("../escape"); err == nil {
		_ = f.Close()
		t.Errorf("OpenLogAppend accepted an invalid service name")
	}
}

func TestReset(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	d, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.WritePID("backend", 7); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	f, err := d.TruncateLog("backend")
	if err != nil {
		t.Fatalf("TruncateLog: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := d.WriteRunEnv(&RunEnv{Ports: map[string]int{"backend": 7102}}); err != nil {
		t.Fatalf("WriteRunEnv: %v", err)
	}

	if err := d.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if _, err := os.Stat(d.Path()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat(%s) after Reset = %v, want ErrNotExist", d.Path(), err)
	}
	// Reset must not take the repository with it.
	if _, err := os.Stat(root); err != nil {
		t.Errorf("stat(root) after Reset = %v, want the root to survive", err)
	}
	// Resetting twice is not an error.
	if err := d.Reset(); err != nil {
		t.Errorf("Reset (second call) = %v, want nil", err)
	}
}

// TestTruncateLogKeepsThePreviousRun: the run before this one survives as
// <log>.1 — the only evidence a crash leaves once the spawning process is
// gone — and the generation before THAT is overwritten, so disk use stays
// bounded at two files per service.
func TestTruncateLogKeepsThePreviousRun(t *testing.T) {
	root := t.TempDir()
	st, err := New(root)
	if err != nil {
		t.Fatal(err)
	}

	write := func(content string) {
		t.Helper()
		f, err := st.TruncateLog("svc")
		if err != nil {
			t.Fatalf("TruncateLog: %v", err)
		}
		if _, err := f.WriteString(content); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}

	write("first run\n")
	write("second run\n")

	// The fresh log holds this run; the rotated one holds the run before it.
	got := readLog(t, filepath.Join(root, ".dev", "logs", "svc.log"))
	if got != "second run\n" {
		t.Errorf("current log = %q, want this run's output", got)
	}
	prev := readLog(t, filepath.Join(root, ".dev", "logs", "svc.log.1"))
	if prev != "first run\n" {
		t.Errorf("rotated log = %q, want the previous run's output", prev)
	}

	// A third start overwrites the generation before it rather than growing.
	write("third run\n")
	prev = readLog(t, filepath.Join(root, ".dev", "logs", "svc.log.1"))
	if prev != "second run\n" {
		t.Errorf("rotated log = %q, want the overwritten generation", prev)
	}
	entries, err := os.ReadDir(filepath.Join(root, ".dev", "logs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("logs dir holds %d entries (%v), want exactly the two generations", len(entries), names)
	}
}

// TestTruncateLogFirstRunHasNoGeneration: rotating a log that does not exist
// yet is not an error — the first start of a service has no previous run.
func TestTruncateLogFirstRunHasNoGeneration(t *testing.T) {
	root := t.TempDir()
	st, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	f, err := st.TruncateLog("fresh")
	if err != nil {
		t.Fatalf("TruncateLog on a service with no previous log: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".dev", "logs", "fresh.log.1")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a rotated generation exists after a first start: %v", err)
	}
}

// readLog reads a file, failing the test on any error.
func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// start claims — the cross-process double-spawn lock

// TestClaimPIDExclusiveCreate: one claim wins, the second is ErrClaimed, and
// the file on disk is 0600 like everything else under .dev.
func TestClaimPIDExclusiveCreate(t *testing.T) {
	d := newDir(t)
	now := time.Now()

	if _, err := d.ClaimPID("svc", os.Getpid(), now); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	_, err := d.ClaimPID("svc", os.Getpid(), now.Add(time.Second))
	if !errors.Is(err, ErrClaimed) {
		t.Fatalf("second claim = %v, want ErrClaimed", err)
	}
	fi, statErr := os.Stat(d.PIDClaimPath("svc"))
	if statErr != nil {
		t.Fatal(statErr)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("claim mode = %o, want 600", fi.Mode().Perm())
	}
}

// TestClaimPIDStalenessRules: a claim by a DEAD owner, an ancient claim and an
// UNPARSEABLE claim are wreckage to be replaced; only a live, fresh, readable
// claim is somebody else's work in progress.
func TestClaimPIDStalenessRules(t *testing.T) {
	d := newDir(t)

	dead := deadPID(t)
	if _, err := d.ClaimPID("svc", dead, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimPID("svc", os.Getpid(), time.Now()); err != nil {
		t.Errorf("a dead owner's claim blocked a new one: %v", err)
	}

	if err := os.WriteFile(d.PIDClaimPath("ancient"), []byte(
		fmt.Sprintf(`{"pid":%d,"at":"2006-01-02T15:04:05Z"}`, os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimPID("ancient", os.Getpid(), time.Now()); err != nil {
		t.Errorf("an ancient claim blocked a new one: %v", err)
	}

	if err := os.WriteFile(d.PIDClaimPath("garbage"), []byte("}{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimPID("garbage", os.Getpid(), time.Now()); err != nil {
		t.Errorf("an unparseable claim must be stale, not fatal: %v", err)
	}
}

// TestReleaseClaimIsIdempotent: releasing twice, or releasing what was never
// claimed, is fine — callers race each other here by design.
func TestReleaseClaimIsIdempotent(t *testing.T) {
	d := newDir(t)
	for i := 0; i < 2; i++ {
		if err := d.ReleaseClaim("svc"); err != nil {
			t.Fatalf("release #%d: %v", i+1, err)
		}
	}
}

// deadPID returns a pid that provably does not exist, so tests never depend on
// a hardcoded number being free.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a process to reap a real dead pid from: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait() // zombie reaped; the pid is now genuinely unallocated
	return pid
}
