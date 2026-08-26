package surface

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDriftGateIsTheSyncMechanism compares the committed surfaces.json against
// a freshly enumerated map from the CURRENTLY BUILT binary. Any CLI command or
// flag, config schema field, or status --json key added, removed or renamed
// fails here until `go run ./tools/surfacemap` refreshes the committed map —
// that failure-to-refresh IS the synchronization this package exists for.
func TestDriftGateIsTheSyncMechanism(t *testing.T) {
	if testing.Short() {
		t.Skip("drift gate builds the binary; skipped under -short")
	}

	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "mabo-ctl")
	if out, err := ExecGoBuild(bin); err != nil {
		t.Fatalf("build mabo-ctl: %v\n%s", err, out)
	}

	live, err := Enumerate(bin)
	if err != nil {
		t.Fatalf("enumerate live surfaces: %v", err)
	}
	diskPath := filepath.Join(root, "internal", "surface", "surfaces.json")
	disk, err := LoadCanonical(diskPath)
	if err != nil {
		t.Fatalf("load committed map: %v", err)
	}

	if diff := Diff(disk, live); len(diff) > 0 {
		t.Fatalf("SURFACE DRIFT between the binary and the committed map (%d).\n"+
			"Refresh with: go run ./tools/surfacemap\nthen review and commit %s\n\n%s",
			len(diff), diskPath, strings.Join(diff, "\n"))
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for dir != "/" && dir != "." {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("go.mod not found above this package")
	return ""
}
