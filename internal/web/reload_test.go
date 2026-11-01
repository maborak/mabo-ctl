package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// reloadServer is a recorder server whose Options.Reload is fn.
func reloadServer(t *testing.T, ctrl Controller, fn func(context.Context, chan<- supervisor.Event) (*Reloaded, error)) *Server {
	t.Helper()
	s, err := NewWith(ctrl, Options{Addr: recorderAddr, Reload: fn})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}
	return s
}

// postPath sends an authenticated POST to path on s.
func postPath(s *Server, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+path, nil)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	return rec
}

// liveConfig writes body to a temp mabo-ctl.yaml, loads it, and returns the
// config with its path.
func liveConfig(t *testing.T, body string) (*config.Config, string) {
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

const watchedYAML = "services:\n  - name: backend\n    cmd: [echo]\n"

// 422 means nothing changed: the configuration in force is the old one.
func TestReloadThatAppliesNothingIs422AndKeepsTheConfigurationInForce(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	s := reloadServer(t, ctrl, func(context.Context, chan<- supervisor.Event) (*Reloaded, error) {
		return nil, errors.New("services[0] \"backend\": cmd is empty")
	})
	rec := postPath(s, "/api/config/reload")
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "cmd is empty") {
		t.Fatalf("POST reload = %d %s, want 422 with the reason", rec.Code, rec.Body.String())
	}
	if s.controller() != Controller(ctrl) {
		t.Error("a refused reload replaced the controller")
	}
}

// An applied reload is in force even when a service failed to come back, and
// says so in a 200 body — the configuration WAS applied, so a 422 claiming the
// old one stayed in force would be a lie.
func TestAppliedReloadWithAFailureIs200AndAdoptsTheNewController(t *testing.T) {
	t.Parallel()
	next := &fakeCtrl{insts: []service.Instance{{Name: "worker"}}}
	s := reloadServer(t, twoServices(), func(_ context.Context, ev chan<- supervisor.Event) (*Reloaded, error) {
		ev <- supervisor.Event{Service: "worker", Phase: supervisor.PhaseFailed, Msg: "failed: process died"}
		return &Reloaded{Controller: next, Restarted: []string{"worker"}, Stopped: []string{"frontend"}},
			errors.New("services did not start: worker")
	})
	rec := postPath(s, "/api/config/reload")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST reload = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var resp reloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || !strings.Contains(resp.Error, "worker") ||
		len(resp.Restarted) != 1 || len(resp.Stopped) != 1 || len(resp.Events) != 1 {
		t.Errorf("response = %+v, want ok=false naming worker, with its event", resp)
	}
	if s.controller() != Controller(next) {
		t.Fatal("the applied configuration's controller was not adopted")
	}
	if names, _ := s.serviceNames(); len(names) != 1 || names[0] != "worker" {
		t.Errorf("declared names = %v, want the new configuration's [worker]", names)
	}
	found := false
	for _, e := range s.events.history() {
		found = found || e.Msg == "configuration reloaded"
	}
	if !found {
		t.Error("the outcome never reached /api/events")
	}
}

// A reload and a start, stop or restart never overlap, and the loser is told
// with 409 rather than queued behind an operation that can take minutes.
func TestReloadAndLifecycleOperationsRefuseEachOtherWith409(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	s := reloadServer(t, ctrl, func(context.Context, chan<- supervisor.Event) (*Reloaded, error) {
		t.Error("the reload ran while an operation held the lifecycle")
		return nil, errors.New("unreachable")
	})

	s.lifecycleMu.RLock()
	rec := postPath(s, "/api/config/reload")
	s.lifecycleMu.RUnlock()
	if rec.Code != http.StatusConflict {
		t.Errorf("reload during an operation = %d, want 409", rec.Code)
	}

	s.lifecycleMu.Lock()
	rec = postPath(s, "/api/backend/start")
	s.lifecycleMu.Unlock()
	if rec.Code != http.StatusConflict {
		t.Errorf("start during a reload = %d, want 409", rec.Code)
	}
	if ctrl.callCount() != 0 {
		t.Error("the start reached the supervisor during a reload")
	}
}

func TestReloadIsRefusedWhereItIsUnavailable(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())
	if rec := postPath(s, "/api/config/reload"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("reload without Options.Reload = %d, want 422", rec.Code)
	}
}

// The watcher compares with the configuration in force, so an edit is offered
// and its revert withdraws the offer.
func TestConfigWatcherAnnouncesAnEditAndWithdrawsItOnRevert(t *testing.T) {
	t.Parallel()
	cfg, path := liveConfig(t, watchedYAML)
	ctrl := twoServices()
	ctrl.cfg = cfg
	s := reloadServer(t, ctrl, func(context.Context, chan<- supervisor.Event) (*Reloaded, error) { return nil, nil })
	s.configPoll = 5 * time.Millisecond
	events, release := s.events.subscribe()
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.watchConfig(ctx)

	next := func(want string) {
		t.Helper()
		select {
		case e := <-events:
			if e.Msg != want {
				t.Fatalf("event %q, want %q", e.Msg, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no %q event", want)
		}
	}
	if err := os.WriteFile(path, []byte(watchedYAML+"# edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	next(configChangedMsg)
	if err := os.WriteFile(path, []byte(watchedYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	next(configCurrentMsg)
}

// A console that cannot reload must not announce edits with a button that
// then refuses.
func TestNoConfigWatchWithoutReload(t *testing.T) {
	cfg, path := liveConfig(t, watchedYAML)
	ctrl := twoServices()
	ctrl.cfg = cfg
	s, err := NewWith(ctrl, Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	s.configPoll = 5 * time.Millisecond
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("ListenAndServe shutdown: %v", err)
		}
	}()

	if err := os.WriteFile(path, []byte(watchedYAML+"# edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	for _, e := range s.events.history() {
		if e.Msg == configChangedMsg {
			t.Fatal("a console without Reload announced a config change")
		}
	}
}

// A page that connects after the edit was announced still offers the reload.
func TestEventsReplayAPendingConfigChange(t *testing.T) {
	ctrl := twoServices()
	s, base := newLiveServer(t, ctrl)
	s.mu.Lock()
	s.configPending = true
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := openStream(t, ctx, sse(s, base, "/api/events"))
	defer resp.Body.Close()
	var ev eventJSON
	if err := json.Unmarshal(readSSEData(t, bufio.NewReader(resp.Body)), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Msg != configChangedMsg {
		t.Errorf("first event = %q, want the pending announcement", ev.Msg)
	}
}

// Rotating the key must lock out everything the old key could do: new
// requests, and the streams it had already opened.
func TestSetAccessKeyLocksOutTheOldKeyAndItsStreams(t *testing.T) {
	ctrl := twoServices()
	s, base := newLiveServer(t, ctrl)
	old := s.Token()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := openStream(t, ctx, sse(s, base, "/api/events"))
	defer resp.Body.Close()
	waitFor(t, "the stream to subscribe", func() bool { return s.events.subscribers() == 1 })

	if err := s.SetAccessKey("bad key!"); err == nil || s.Token() != old {
		t.Fatalf("an invalid key was accepted (err %v)", err)
	}
	if err := s.SetAccessKey("rotated-key-0123"); err != nil {
		t.Fatalf("SetAccessKey: %v", err)
	}
	waitFor(t, "the old key's stream to close", func() bool { return s.streams.Load() == 0 })
	_, _ = io.Copy(io.Discard, resp.Body)

	post := func(token string) int {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/backend/start", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(tokenHeader, token)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		return r.StatusCode
	}
	if code := post(old); code != http.StatusForbidden {
		t.Errorf("old key after rotation = %d, want 403", code)
	}
	if code := post("rotated-key-0123"); code != http.StatusOK {
		t.Errorf("new key = %d, want 200", code)
	}
}

// The spec is only byte-compared against its docs copy elsewhere; this parses
// it, so a malformed edit fails here rather than as a blank API page, and pins
// the reload contract the page and the docs describe.
func TestOpenAPISpecParsesAndDocumentsTheReloadContract(t *testing.T) {
	t.Parallel()
	var spec map[string]any
	if err := yaml.Unmarshal([]byte(openapiSpec), &spec); err != nil {
		t.Fatalf("the embedded OpenAPI spec does not parse: %v", err)
	}
	at := func(m map[string]any, keys ...string) map[string]any {
		t.Helper()
		for _, k := range keys {
			next, ok := m[k].(map[string]any)
			if !ok {
				t.Fatalf("spec has no %q under %v", k, keys)
			}
			m = next
		}
		return m
	}
	responses := at(spec, "paths", "/api/config/reload", "post", "responses")
	for _, code := range []string{"200", "409", "422"} {
		if _, ok := responses[code]; !ok {
			t.Errorf("POST /api/config/reload documents no %s", code)
		}
	}
	at(spec, "components", "schemas", "ReloadResponse")
	for _, path := range []string{"/api/start-all", "/api/stop-all", "/api/restart-all", "/api/{svc}/start", "/api/{svc}/stop", "/api/{svc}/restart"} {
		if _, ok := at(spec, "paths", path, "post", "responses")["409"]; !ok {
			t.Errorf("POST %s does not document the 409 a reload in progress returns", path)
		}
	}
}
