package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/state"
)

// reloadApp bootstraps an app over the repo at h and resolves it the way the
// resident front ends do before they serve, ports persisted included. Nothing
// is started, so a reload has nothing to stop or restart and these tests are
// about what it resolves and adopts.
func reloadApp(t *testing.T, h *harness) *app {
	t.Helper()
	a := serveApp(t, h)
	if _, err := a.realSupervisor(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := service.Persist(a.st, a.insts); err != nil {
		t.Fatalf("persist: %v", err)
	}
	return a
}

// writeConfig replaces the harness repo's mabo-ctl.yaml.
func writeConfig(t *testing.T, h *harness, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.root, config.FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func apiConfig(port, color string) string {
	return "services:\n  - name: api\n    port: " + port + "\n    cmd: [sleep, \"600\"]\n    color: " + color + "\n"
}

// docs/LANDMINES.md §7: a reload that resolved without the drift step kept
// run.env's port against the edit that changed it, and then persisted the old
// port back. The edit is the instruction; an unrelated edit moves nothing.
func TestReloadAdoptsAnEditedPortAndKeepsAPinnedOne(t *testing.T) {
	h := newHarnessWithConfig(t, apiConfig("4711", "green"))
	a := reloadApp(t, h)

	writeConfig(t, h, apiConfig("4799", "green"))
	if r, err := a.reloadConfig(context.Background(), nil); r == nil || err != nil {
		t.Fatalf("reloadConfig = %v, %v", r, err)
	}
	if got := a.insts[0].Port; got != 4799 {
		t.Errorf("api port after editing it to 4799 = %d", got)
	}
	re, err := a.st.ReadRunEnv()
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := re.Port("api"); p != 4799 {
		t.Errorf("run.env api port = %d, want the adopted 4799", p)
	}

	// A port pinned by an earlier --port survives an edit that does not touch
	// the declared port.
	pinned := &state.RunEnv{Ports: map[string]int{}}
	pinned.SetPort("api", 5000)
	if err := a.st.WriteRunEnv(pinned); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, h, apiConfig("4799", "red"))
	if r, err := a.reloadConfig(context.Background(), nil); r == nil || err != nil {
		t.Fatalf("reloadConfig = %v, %v", r, err)
	}
	if got := a.insts[0].Port; got != 5000 {
		t.Errorf("a colour edit moved api from its pinned 5000 to %d", got)
	}
}

// Discovery asks the operator to rename devctl.yaml; re-reading the old path
// made every reload after the rename fail.
func TestReloadFollowsTheRenameDiscoveryAskedFor(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, config.LegacyFileName)
	if err := os.WriteFile(legacy, []byte(apiConfig("4711", "green")), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarnessAt(t, root)
	a := reloadApp(t, h)

	if err := os.Rename(legacy, filepath.Join(root, config.FileName)); err != nil {
		t.Fatal(err)
	}
	if r, err := a.reloadConfig(context.Background(), nil); r == nil || err != nil {
		t.Fatalf("reloadConfig after the rename = %v, %v", r, err)
	}
	if filepath.Base(a.cfg.Path) != config.FileName {
		t.Errorf("config in force is %s, want the renamed file", a.cfg.Path)
	}
}

// --ports assigns by position; an edit that changes which services declare a
// port would silently hand each slot to a different service.
func TestReloadRefusesWhenPositionalPortsWouldShift(t *testing.T) {
	h := newHarnessWithConfig(t, apiConfig("4711", "green"))
	a := reloadApp(t, h)
	a.ports.values = []int{4800}

	writeConfig(t, h, "services:\n  - name: db\n    port: 5432\n    cmd: [sleep, \"600\"]\n"+
		"  - name: api\n    port: 4711\n    cmd: [sleep, \"600\"]\n")
	r, err := a.reloadConfig(context.Background(), nil)
	if r != nil || err == nil || !strings.Contains(err.Error(), "--ports") {
		t.Fatalf("reloadConfig = %v, %v; want a refusal naming --ports", r, err)
	}
}

// A watcher started before a reload must read the configuration in force after
// it, not the one it was started with.
func TestReloadMovesTheResidentSupervisor(t *testing.T) {
	h := newHarnessWithConfig(t, apiConfig("4711", "green"))
	a := reloadApp(t, h)
	a.resident.Store(a.sup)
	old := a.sup

	writeConfig(t, h, apiConfig("4711", "red"))
	if r, err := a.reloadConfig(context.Background(), nil); r == nil || err != nil {
		t.Fatalf("reloadConfig = %v, %v", r, err)
	}
	if got := a.resident.Load(); got == old || got != a.sup {
		t.Error("the resident supervisor did not follow the reload")
	}
	if sts := (residentStatus{a}).Status(context.Background()); len(sts) != 1 || sts[0].Name != "api" {
		t.Errorf("resident status = %+v", sts)
	}
}
