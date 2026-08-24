package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
