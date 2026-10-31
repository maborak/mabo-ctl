package console

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// configuredFake is a fakeCtrl that also says which configuration it was built
// from, as *supervisor.Supervisor does.
type configuredFake struct {
	*fakeCtrl
	cfg *config.Config
}

func (c *configuredFake) Config() *config.Config { return c.cfg }

// loadConfig writes body to a temp mabo-ctl.yaml and loads it.
func loadConfig(t *testing.T, body string) (*config.Config, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), config.FileName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, path
}

const reloadYAML = "services:\n  - name: website\n    cmd: [echo]\n"

// reloadModel boots a console whose controller knows its configuration and
// whose Reload is reload.
func reloadModel(t *testing.T, reload func(context.Context, chan<- supervisor.Event) (*Reloaded, error)) (Model, string) {
	t.Helper()
	cfg, path := loadConfig(t, reloadYAML)
	f := &fakeCtrl{statuses: threeServices()}
	m := New(&configuredFake{fakeCtrl: f, cfg: cfg}, Options{Root: "/repo", Reload: reload})
	t.Cleanup(m.Shutdown)
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 140, Height: 24})
	m, _ = send(t, m, statusMsg{statuses: f.statuses})
	return m, path
}

// The comparison is with the configuration in force, so an edit shows and a
// revert withdraws it.
func TestConfigEditShowsInTheTitleAndARevertWithdrawsIt(t *testing.T) {
	m, path := reloadModel(t, func(context.Context, chan<- supervisor.Event) (*Reloaded, error) { return nil, nil })
	cfg := configOf(m.ctrl)

	if err := os.WriteFile(path, []byte(reloadYAML+"# edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, _ = send(t, m, runCmd(t, checkConfig(cfg)))
	if title := m.titleView(); !strings.Contains(title, "YAML changed · L reloads") {
		t.Fatalf("title does not announce the edit: %q", title)
	}

	if err := os.WriteFile(path, []byte(reloadYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	m, _ = send(t, m, runCmd(t, checkConfig(cfg)))
	if strings.Contains(m.titleView(), "YAML changed") {
		t.Fatal("reverting the edit did not withdraw the offer")
	}
}

// L restarts services, and is one Shift away from l: it asks first, and only y
// reloads.
func TestReloadKeyAsksBeforeRestartingAnything(t *testing.T) {
	calls := 0
	m, _ := reloadModel(t, func(context.Context, chan<- supervisor.Event) (*Reloaded, error) {
		calls++
		return nil, errors.New("not reached")
	})

	m, _ = send(t, m, key("L"))
	if !m.confirmReload || !strings.Contains(m.View(), "Reload mabo-ctl.yaml?") {
		t.Fatalf("L did not ask first:\n%s", m.View())
	}
	m, _ = send(t, m, key("n"))
	if m.confirmReload || m.reload != nil {
		t.Fatal("a key other than y started the reload")
	}

	m, _ = send(t, m, key("L"))
	m, cmd := send(t, m, key("y"))
	if m.reload == nil {
		t.Fatal("y did not start the reload")
	}
	m, _ = send(t, m, runCmd(t, cmd))
	if calls != 1 {
		t.Fatalf("Reload called %d times, want 1", calls)
	}
	if m.reloadErr == nil || !strings.Contains(m.View(), "configuration not reloaded") {
		t.Errorf("a refused reload was not reported:\n%s", m.View())
	}
}

// A key that silently does nothing reads as a broken key.
func TestReloadAndOperationKeysSayWhyTheyCannotRun(t *testing.T) {
	release := make(chan struct{})
	m, _ := reloadModel(t, func(context.Context, chan<- supervisor.Event) (*Reloaded, error) {
		<-release
		return nil, errors.New("refused")
	})
	t.Cleanup(func() { close(release) })

	busy := m
	busy.activeOps = 1
	busy, _ = send(t, busy, key("L"))
	if busy.confirmReload || !strings.Contains(busy.notice, "wait for the running start/stop") {
		t.Errorf("L during an operation: confirm=%v notice=%q", busy.confirmReload, busy.notice)
	}

	m, _ = send(t, m, key("L"))
	m, _ = send(t, m, key("y"))
	for _, k := range []string{"s", "x", "r", "a", "S"} {
		got, _ := send(t, m, key(k))
		if !strings.Contains(got.notice, "reload is still running") {
			t.Errorf("%s during a reload: notice %q, want it to say why nothing happened", k, got.notice)
		}
	}
}

// Quitting ends the process, and with it a reload that may have stopped
// services it has not started again: q waits, a second q quits at once.
func TestQuitWaitsForAReloadInFlight(t *testing.T) {
	release := make(chan struct{})
	m, _ := reloadModel(t, func(context.Context, chan<- supervisor.Event) (*Reloaded, error) {
		<-release
		return nil, errors.New("refused")
	})
	m, _ = send(t, m, key("L"))
	m, cmd := send(t, m, key("y"))

	waiting, _ := send(t, m, key("q"))
	if waiting.quit || !waiting.quitAfterReload {
		t.Fatal("q quit while a reload was applying")
	}
	forced, _ := send(t, waiting, key("q"))
	if !forced.quit {
		t.Error("a second q did not quit at once")
	}

	close(release)
	done, _ := send(t, waiting, runCmd(t, cmd))
	if !done.quit {
		t.Error("the deferred quit did not happen when the reload finished")
	}
}

// An applied reload swaps the controller, and a status read from the old one
// must not overwrite the new configuration's list.
func TestAppliedReloadAdoptsTheControllerAndDropsStaleStatus(t *testing.T) {
	next := &fakeCtrl{statuses: threeServices()[:1]}
	m, _ := reloadModel(t, func(_ context.Context, ev chan<- supervisor.Event) (*Reloaded, error) {
		ev <- supervisor.Event{Service: "website", Msg: "stopping…"}
		return &Reloaded{Controller: next, Restarted: []string{"website"}}, errors.New("services did not start: website")
	})
	m, _ = send(t, m, key("L"))
	m, cmd := send(t, m, key("y"))
	msg := runCmd(t, cmd)
	if _, ok := msg.(reloadEventMsg); !ok {
		t.Fatalf("first reload message is %T, want the supervisor event", msg)
	}
	m, cmd = send(t, m, msg)
	m, refreshCmd := send(t, m, runCmd(t, cmd))

	if m.ctrl != Controller(next) || m.ctrlGen != 1 {
		t.Fatal("the reloaded controller was not adopted")
	}
	if !strings.Contains(m.View(), "configuration reloaded, but services did not start: website") {
		t.Errorf("a partial failure was not reported:\n%s", m.View())
	}
	m, _ = send(t, m, runCmd(t, refreshCmd))
	if len(m.statuses) != 1 {
		t.Fatalf("the new controller's status was not applied: %d rows", len(m.statuses))
	}
	m, _ = send(t, m, statusMsg{statuses: threeServices(), gen: 0})
	if len(m.statuses) != 1 {
		t.Error("a status read from the replaced controller was applied")
	}
}
