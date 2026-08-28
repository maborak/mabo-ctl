package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// waitHealthFixture is a two-service repo where only one declares a health
// URL; --wait must never wait on the service that was never asked anything.
const waitHealthFixture = `
services:
  - name: api
    cmd: [sleep, 100]
    port: 7310
    health: http://127.0.0.1:7310/healthz
  - name: worker
    cmd: [sleep, 100]
`

// flipProbed moves every probed service into ready, as a probe turning green
// does while a caller watches. Safe next to fakeSup.Status because both hold
// the same mutex.
func flipProbed(f *fakeSup) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.statuses {
		if f.statuses[i].Health != "" {
			f.statuses[i].Phase = supervisor.PhaseReady
			f.statuses[i].HTTP = 200
		}
	}
}

func newWaitingHarness(t *testing.T, args ...string) (*harness, *fakeSup) {
	t.Helper()
	h := newHarnessWithConfig(t, waitHealthFixture, args...)
	sup := &fakeSup{statuses: []supervisor.Status{
		{Name: "api", Phase: supervisor.PhaseRunning, Port: 7310,
			Health: "http://127.0.0.1:7310/healthz"},
		{Name: "worker", Phase: supervisor.PhaseReady},
	}}
	h.sup = sup
	return h, sup
}

func TestHealthWaitReportsUsageWithoutWait(t *testing.T) {
	t.Parallel()
	h, _ := newWaitingHarness(t, "health", "--timeout", "5s")
	code := h.run()
	if code != exitUsage {
		t.Fatalf("exit code = %d, want %d (--timeout without --wait)", code, exitUsage)
	}
}

func TestHealthWaitSucceedsOnceReady(t *testing.T) {
	t.Parallel()
	h, sup := newWaitingHarness(t, "health", "--wait", "--timeout", "30s")
	go func() {
		time.Sleep(50 * time.Millisecond)
		flipProbed(sup)
	}()
	code := h.run()
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d; stderr=%s stdout=%s",
			code, exitOK, h.stderr.String(), h.stdout.String())
	}
}

func TestHealthWaitTimesOutAndNamesTheLaggard(t *testing.T) {
	t.Parallel()
	h, _ := newWaitingHarness(t, "health", "--wait", "--timeout", "150ms")
	code := h.run()
	if code != exitNotReady {
		t.Fatalf("exit code = %d, want %d", code, exitNotReady)
	}
	if !strings.Contains(h.stderr.String(), "api") {
		t.Errorf("stderr %q does not name the still-down service", h.stderr.String())
	}
}

func TestHealthWaitNeverWaitsOnUndeclaredServices(t *testing.T) {
	t.Parallel()
	// worker declares no health URL: even when it sits in running forever, the
	// wait ends as soon as the probed service is failed. A stuck portless
	// service must not hold --wait open.
	start := time.Now()
	h := newHarnessWithConfig(t, waitHealthFixture, "health", "--wait")
	h.sup = &fakeSup{statuses: []supervisor.Status{
		{Name: "api", Phase: supervisor.PhaseFailed,
			Health: "http://127.0.0.1:7310/healthz"},
		{Name: "worker", Phase: supervisor.PhaseRunning},
	}}
	code := h.run()
	if code != exitNotReady {
		t.Fatalf("exit code = %d, want %d", code, exitNotReady)
	}
	if d := time.Since(start); d > 2*healthWaitInterval {
		t.Errorf("wait held open %s by a service with no health URL", d)
	}
}

func TestHealthWaitNoDeclaredHealthReturnsImmediately(t *testing.T) {
	t.Parallel()
	start := time.Now()
	h := newHarnessWithConfig(t, waitHealthFixture, "health", "--wait", "--timeout", "1m")
	h.sup = &fakeSup{statuses: readyStatuses()}
	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d", code, exitOK)
	}
	if d := time.Since(start); d > 2*healthWaitInterval {
		t.Errorf("wait with nothing declared took %s; want immediate", d)
	}
}

// logsWithPaths wires each status with LogPath and materialises those files so
// --since's mtime gate has something honest to read.
func logsWithPaths(t *testing.T, h *harness, modAge time.Duration) {
	t.Helper()
	paths := map[string]string{
		"alpha": filepath.Join(h.root, ".dev", "alpha.log"),
		"beta":  filepath.Join(h.root, ".dev", "beta.log"),
	}
	for name, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir .dev: %v", err)
		}
		if err := os.WriteFile(p, []byte("seed "+name+"\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	if modAge > 0 {
		old := time.Now().Add(-modAge)
		for _, p := range paths {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatalf("chtimes %s: %v", p, err)
			}
		}
	}
	sts := h.sup.statuses
	h.sup.statuses = append([]supervisor.Status(nil), sts...)
	for i := range h.sup.statuses {
		if p, ok := paths[h.sup.statuses[i].Name]; ok {
			h.sup.statuses[i].LogPath = p
		}
	}
}

func TestLogsGrepFiltersEveryStream(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "logs", "alpha", "--grep", "needle")
	h.sup.lines = map[string][]string{"alpha": {"noisy", "has needle here", "quiet"}}
	if code := h.run(); code != exitOK {
		t.Fatalf("exit = %d, stderr %s", code, h.stderr)
	}
	if got := strings.TrimSpace(h.stdout.String()); got != "has needle here" {
		t.Fatalf("grep output = %q, want only the needle line", got)
	}
}

func TestLogsGrepKeepsPrefixInMultiServiceMode(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "logs", "alpha", "beta", "--grep", "hit")
	h.sup.lines = map[string][]string{"alpha": {"hit"}, "beta": {"miss", "another hit"}}
	if code := h.run(); code != exitOK {
		t.Fatalf("exit = %d, stderr %s", code, h.stderr)
	}
	out := h.stdout.String()
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") ||
		strings.Contains(out, "miss") {
		t.Fatalf("output %q must carry prefixed hits from both and no miss", out)
	}
}

func TestLogsSinceKeepsFreshAndSkipsStale(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "logs", "alpha", "beta", "--since", "30m")
	logsWithPaths(t, h, time.Hour) // both files aged an hour back
	freshPath := h.sup.statuses[0].LogPath
	if err := os.WriteFile(freshPath, []byte("just now\n"), 0o600); err != nil {
		t.Fatalf("rewrite fresh log: %v", err)
	}
	h.sup.lines = map[string][]string{"alpha": {"line-a"}}
	if code := h.run(); code != exitOK {
		t.Fatalf("exit = %d, stderr %s", code, h.stderr)
	}
	if out := h.stdout.String(); !strings.Contains(out, "line-a") {
		t.Fatalf("fresh service's line missing from %q", out)
	}
	if !strings.Contains(h.stderr.String(), "beta") {
		t.Errorf("stderr %q should name the skipped service", h.stderr.String())
	}
}

func TestLogsSinceAllStalePrintsNotesOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "logs", "alpha", "--since", "30m")
	logsWithPaths(t, h, time.Hour)
	h.sup.lines = map[string][]string{"alpha": {"old line"}}
	if code := h.run(); code != exitOK {
		t.Fatalf("exit = %d, stderr %s", code, h.stderr)
	}
	if out := h.stdout.String(); strings.Contains(out, "old line") {
		t.Fatalf("stale service's output leaked past --since: %q", out)
	}
	if !strings.Contains(h.stderr.String(), "alpha") {
		t.Errorf("stderr %q should name the skipped service", h.stderr.String())
	}
}

func TestLogsSinceRejectsFollow(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "logs", "alpha", "-f", "--since", "5m")
	h.cancel() // in case the run somehow starts following
	if code := h.run(); code != exitUsage {
		t.Fatalf("exit = %d, want %d for --since with -f", code, exitUsage)
	}
}
