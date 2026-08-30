package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/redact"
	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/state"
	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/ui"
)

// The tests in this file are the security proof for a package that can start
// and stop processes over HTTP. Every control described in the package
// documentation has a test here that shows it refusing something, and the two
// leak tests exist because a stream handler that outlives its request would
// quietly accumulate one supervisor.Tail per opened browser tab.
//
// Nothing here spawns a process: a fake Controller stands in for the
// supervisor, which is also what lets the tests assert that a request was
// refused BEFORE it reached the supervisor at all.

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeTail records one Tail call and reports when that call returned.
type fakeTail struct {
	svc    string
	n      int
	follow bool
	done   chan struct{}
}

// finished reports whether the tail has returned.
func (t *fakeTail) finished() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// opCall records one start/stop/restart request.
type opCall struct {
	kind  opKind
	names []string
}

// fakeCtrl is a Controller that records what it was asked to do and never
// touches a process.
type fakeCtrl struct {
	mu       sync.Mutex
	insts    []service.Instance
	cfg      *config.Config
	statuses []supervisor.Status
	lines    []string
	events   []supervisor.Event
	opErr    error
	opDelay  time.Duration

	tails []*fakeTail
	calls []opCall
}

func (f *fakeCtrl) Instances() []service.Instance {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]service.Instance(nil), f.insts...)
}

func (f *fakeCtrl) Config() *config.Config { return f.cfg }

func (f *fakeCtrl) Status(context.Context) []supervisor.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]supervisor.Status(nil), f.statuses...)
}

func (f *fakeCtrl) Start(ctx context.Context, names []string, ev chan<- supervisor.Event) error {
	return f.op(ctx, opStart, names, ev)
}

func (f *fakeCtrl) Stop(ctx context.Context, names []string, ev chan<- supervisor.Event) error {
	return f.op(ctx, opStop, names, ev)
}

func (f *fakeCtrl) Restart(ctx context.Context, names []string, ev chan<- supervisor.Event) error {
	return f.op(ctx, opRestart, names, ev)
}

func (f *fakeCtrl) op(ctx context.Context, kind opKind, names []string, ev chan<- supervisor.Event) error {
	f.mu.Lock()
	f.calls = append(f.calls, opCall{kind: kind, names: append([]string(nil), names...)})
	events, err, delay := f.events, f.opErr, f.opDelay
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, e := range events {
		select {
		case ev <- e:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (f *fakeCtrl) Tail(ctx context.Context, svc string, n int, follow bool, out chan<- string) error {
	t := &fakeTail{svc: svc, n: n, follow: follow, done: make(chan struct{})}
	f.mu.Lock()
	f.tails = append(f.tails, t)
	lines := append([]string(nil), f.lines...)
	f.mu.Unlock()

	defer close(t.done)
	defer close(out)

	for _, l := range lines {
		select {
		case out <- l:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if !follow {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

// callCount reports how many operations reached the controller.
// callsOf returns the recorded operations of one kind, so a test can assert on
// the NAMES a route selected rather than only on how many calls it made.
func (f *fakeCtrl) callsOf(kind opKind) []opCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []opCall
	for _, c := range f.calls {
		if c.kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeCtrl) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// tailCount reports how many tails were started.
func (f *fakeCtrl) tailCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tails)
}

// tailsFinished reports whether every tail started so far has returned.
func (f *fakeCtrl) tailsFinished() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tails {
		if !t.finished() {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

const (
	declaredSecret  = "declared-token-must-not-appear"
	inheritedSecret = "inherited-token-must-not-appear"
	inheritedKey    = "AWS_SECRET_ACCESS_KEY"
	recorderAddr    = "127.0.0.1:7999"
)

// twoServices is the fixture every test shares: one service whose declared
// environment holds a credential and one plain one.
func twoServices() *fakeCtrl {
	return &fakeCtrl{
		insts: []service.Instance{
			{
				Name:    "backend",
				Dir:     "/repo/backend",
				Port:    7100,
				Health:  "http://localhost:7100/health?ready=1&deep=0",
				Cmd:     []string{"/usr/bin/python3", "-m", "uvicorn", "app:main", "--port", "7100"},
				Runtime: "conda:api",
				Color:   "cyan",
				// The RESOLVED environment: the caller's whole environment,
				// forwarded to the child. None of it may ever be rendered.
				Env: []string{
					inheritedKey + "=" + inheritedSecret,
					"HOME=/Users/dev",
					"API_TOKEN=" + declaredSecret,
				},
				DependsOn: []string{},
			},
			{
				Name: "frontend",
				Dir:  "/repo/frontend",
				Port: 7200,
				Cmd:  []string{"/usr/bin/npm", "run", "dev"},
				Env:  []string{"HOME=/Users/dev"},
				// A dependency, so the field is exercised.
				DependsOn: []string{"backend"},
			},
		},
		cfg: &config.Config{
			Root: "/repo",
			Path: "/repo/mabo-ctl.yaml",
			Services: []config.Spec{
				{
					Name: "backend",
					Env: map[string]string{
						"API_TOKEN":    declaredSecret,
						"DATABASE_URL": "postgres://app:hunter2@localhost:5432/app",
						"LOG_LEVEL":    "debug",
					},
				},
				{Name: "frontend"},
			},
			StopGrace:    10 * time.Second,
			ReadyTimeout: 30 * time.Second,
		},
		statuses: []supervisor.Status{
			{
				Name:    "backend",
				Phase:   supervisor.PhaseReady,
				PID:     4242,
				Port:    7100,
				Health:  "http://localhost:7100/health?ready=1&deep=0",
				HTTP:    200,
				LogPath: "/repo/.dev/logs/backend.log",
				Elapsed: 1500 * time.Millisecond,
			},
			{Name: "frontend", Phase: supervisor.PhaseStopped, LogPath: "/repo/.dev/logs/frontend.log"},
		},
		lines: []string{"listening on 7100", "ready"},
	}
}

// newRecorderServer returns a Server that is never bound, for tests that forge
// Host and Origin headers against a known address.
func newRecorderServer(t *testing.T, ctrl Controller) *Server {
	t.Helper()
	s, err := NewWith(ctrl, Options{Addr: recorderAddr})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}
	s.heartbeat = 20 * time.Millisecond
	return s
}

// newLiveServer binds a real socket on an ephemeral port and serves until the
// test ends, so the SSE tests exercise real connection teardown.
func newLiveServer(t *testing.T, ctrl Controller) (*Server, string) {
	t.Helper()
	s, err := NewWith(ctrl, Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}
	s.heartbeat = 20 * time.Millisecond
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("ListenAndServe: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("ListenAndServe did not return after ctx cancellation")
		}
	})
	return s, "http://" + s.Addr()
}

// waitFor polls cond until it holds or the test fails.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// mutatingPaths is every route that can change the state of a process.
var mutatingPaths = []string{
	"/api/backend/start",
	"/api/backend/stop",
	"/api/backend/restart",
	"/api/start-all",
	"/api/stop-all",
}

// ---------------------------------------------------------------------------
// bind-address safety
// ---------------------------------------------------------------------------

func TestNewRefusesNonLoopbackWithoutForce(t *testing.T) {
	t.Parallel()
	cases := []struct {
		addr     string
		loopback bool
	}{
		{"127.0.0.1:7999", true},
		{"127.0.0.2:0", true},
		{"[::1]:7999", true},
		{"0.0.0.0:7999", false},
		{":7999", false},
		{"[::]:7999", false},
		{"192.168.1.10:7999", false},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			_, err := NewWith(twoServices(), Options{Addr: tc.addr})
			switch {
			case tc.loopback && err != nil:
				t.Fatalf("NewWith(%q) refused a loopback address: %v", tc.addr, err)
			case !tc.loopback && err == nil:
				t.Fatalf("NewWith(%q) accepted a non-loopback address without Force", tc.addr)
			case !tc.loopback:
				if !errors.Is(err, ErrUnsafeAddr) {
					t.Fatalf("NewWith(%q) error = %v, want one wrapping ErrUnsafeAddr", tc.addr, err)
				}
				if !strings.Contains(err.Error(), "--i-know-this-is-dangerous") {
					t.Errorf("refusal does not say how to override: %v", err)
				}
			}

			// Force accepts every one of them.
			if _, err := NewWith(twoServices(), Options{Addr: tc.addr, Force: true}); err != nil {
				t.Fatalf("NewWith(%q, Force) = %v, want nil", tc.addr, err)
			}
		})
	}
}

func TestNewRejectsMalformedAddrAndNilController(t *testing.T) {
	t.Parallel()
	if _, err := NewWith(twoServices(), Options{Addr: "no-port-here"}); err == nil {
		t.Error("NewWith accepted an address with no port")
	}
	if _, err := NewWith(twoServices(), Options{Addr: "127.0.0.1:not-a-port"}); err == nil {
		t.Error("NewWith accepted a non-numeric port")
	}
	if _, err := New(nil, Options{}); !errors.Is(err, ErrNoController) {
		t.Errorf("New(nil) = %v, want ErrNoController", err)
	}
	if _, err := NewWith(nil, Options{}); !errors.Is(err, ErrNoController) {
		t.Errorf("NewWith(nil) = %v, want ErrNoController", err)
	}
}

func TestDefaultAddrIsLoopback(t *testing.T) {
	t.Parallel()
	s, err := NewWith(twoServices(), Options{})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}
	if s.Addr() != DefaultAddr {
		t.Errorf("Addr() = %q, want %q", s.Addr(), DefaultAddr)
	}
	loopback, err := isLoopbackAddr(DefaultAddr)
	if err != nil || !loopback {
		t.Errorf("DefaultAddr %q is not loopback (err=%v)", DefaultAddr, err)
	}
	if s.heartbeat != 15*time.Second {
		t.Errorf("heartbeat = %v, want the 15s the contract fixes", s.heartbeat)
	}
}

func TestURLCarriesTokenAndAnOpenableHost(t *testing.T) {
	t.Parallel()
	s, err := NewWith(twoServices(), Options{Addr: "0.0.0.0:7999", Force: true})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}
	url := s.URL()
	if !strings.Contains(url, "token="+s.Token()) {
		t.Errorf("URL() = %q, want it to carry the session token", url)
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:7999/") {
		t.Errorf("URL() = %q, want a wildcard bind reported as loopback", url)
	}
	if len(s.Token()) != 64 {
		t.Errorf("token is %d chars, want 64 hex chars (32 bytes)", len(s.Token()))
	}
}

func TestTokensDifferBetweenServers(t *testing.T) {
	t.Parallel()
	a, err := NewWith(twoServices(), Options{})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}
	b, err := NewWith(twoServices(), Options{})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}
	if a.Token() == b.Token() {
		t.Fatal("two servers generated the same session token")
	}
}

// ---------------------------------------------------------------------------
// CSRF: token, method, Origin, Host
// ---------------------------------------------------------------------------

func TestMutationWithoutTokenIsForbidden(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	s := newRecorderServer(t, ctrl)

	for _, path := range mutatingPaths {
		req := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+path, nil)
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s without a token = %d, want 403", path, rec.Code)
		}
	}
	if n := ctrl.callCount(); n != 0 {
		t.Fatalf("%d untokened mutations reached the supervisor, want 0", n)
	}
}

func TestMutationWithWrongTokenIsForbidden(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	s := newRecorderServer(t, ctrl)

	wrong := []string{
		"",
		"nope",
		strings.Repeat("0", len(s.Token())), // right length, wrong value
		s.Token()[:len(s.Token())-1],        // prefix
		s.Token() + "x",                     // extension
		strings.ToUpper(s.Token()),          // case-flipped
	}
	for _, tok := range wrong {
		for _, path := range mutatingPaths {
			req := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+path, nil)
			req.Header.Set(tokenHeader, tok)
			rec := httptest.NewRecorder()
			s.h.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("POST %s with token %q = %d, want 403", path, tok, rec.Code)
			}
		}
	}
	if n := ctrl.callCount(); n != 0 {
		t.Fatalf("%d mutations with a bad token reached the supervisor, want 0", n)
	}
}

func TestTokenInAQueryParameterIsNotAccepted(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	s := newRecorderServer(t, ctrl)

	// A cross-origin form POST can carry a query string but cannot set a
	// header, so accepting the token here would undo the whole defence.
	req := httptest.NewRequest(http.MethodPost,
		"http://"+recorderAddr+"/api/backend/start?token="+s.Token(), nil)
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST with the token in the query = %d, want 403", rec.Code)
	}
	if n := ctrl.callCount(); n != 0 {
		t.Fatalf("the request reached the supervisor")
	}
}

func TestGetToAMutatingPathIsMethodNotAllowed(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	s := newRecorderServer(t, ctrl)

	for _, path := range mutatingPaths {
		req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+path, nil)
		// Even holding a valid token, a GET must not mutate: a GET is
		// reachable from an <img> tag on any page in the world.
		req.Header.Set(tokenHeader, s.Token())
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", path, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); !strings.Contains(allow, http.MethodPost) {
			t.Errorf("GET %s: Allow = %q, want it to name POST", path, allow)
		}
	}
	if n := ctrl.callCount(); n != 0 {
		t.Fatalf("%d GETs reached the supervisor, want 0", n)
	}
}

func TestForeignOriginIsRejected(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	s := newRecorderServer(t, ctrl)

	foreign := []string{
		"http://evil.example",
		"https://evil.example",
		"http://evil.example:7999",
		"http://127.0.0.1:8080",
		"http://localhost:8080",
		"null",
		"file://",
	}
	for _, origin := range foreign {
		req := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+"/api/backend/start", nil)
		req.Header.Set(tokenHeader, s.Token())
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("POST with Origin %q = %d, want 403", origin, rec.Code)
		}
	}
	if n := ctrl.callCount(); n != 0 {
		t.Fatalf("%d cross-origin mutations reached the supervisor, want 0", n)
	}

	// A read is refused just as firmly, so a cross-origin page cannot even
	// enumerate the services.
	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/services", nil)
	req.Header.Set(tokenHeader, s.Token())
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("GET /api/services with a foreign Origin = %d, want 403", rec.Code)
	}
}

func TestSameOriginIsAccepted(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	s := newRecorderServer(t, ctrl)

	for _, origin := range []string{"http://" + recorderAddr, "http://localhost:7999"} {
		req := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+"/api/backend/start", nil)
		req.Header.Set(tokenHeader, s.Token())
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("POST with same Origin %q = %d, want 200 (body %s)",
				origin, rec.Code, rec.Body.String())
		}
	}
}

func TestMismatchedHostIsRejected(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	s := newRecorderServer(t, ctrl)

	// Every one of these is what a DNS-rebinding attack looks like on the
	// wire: the connection arrives on loopback, and the Host header is the
	// attacker's own name or the wrong port.
	bad := []string{
		"attacker.example:7999",
		"mabo-ctl.attacker.example:7999",
		"127.0.0.1.nip.io:7999",
		"127.0.0.1:8080",
		"localhost:8080",
		"192.168.1.10:7999",
		"",
	}
	for _, host := range bad {
		for _, target := range []string{"/", "/api/services", "/api/status", "/api/backend/start"} {
			req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+target, nil)
			req.Host = host
			req.Header.Set(tokenHeader, s.Token())
			rec := httptest.NewRecorder()
			s.h.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("Host %q on %s = %d, want 403", host, target, rec.Code)
			}
		}
	}
	if n := ctrl.callCount(); n != 0 {
		t.Fatalf("%d rebinding requests reached the supervisor, want 0", n)
	}
}

func TestMatchingHostIsAccepted(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())

	for _, host := range []string{"127.0.0.1:7999", "localhost:7999", "[::1]:7999"} {
		req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/services", nil)
		req.Header.Set(tokenHeader, s.Token())
		req.Host = host
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Host %q = %d, want 200", host, rec.Code)
		}
	}
}

func TestNoCORSHeadersAreEverSent(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())

	for _, target := range []string{"/", "/api/services", "/api/status"} {
		req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+target, nil)
		req.Header.Set(tokenHeader, s.Token())
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, req)

		for h := range rec.Header() {
			if strings.HasPrefix(strings.ToLower(h), "access-control-") {
				t.Errorf("%s carries CORS header %s, which would let a foreign page read it", target, h)
			}
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", target, got)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q, want DENY", target, got)
		}
	}
}

func TestMutationWithTheTokenReachesTheSupervisor(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	ctrl.events = []supervisor.Event{
		{Service: "backend", Phase: supervisor.PhaseRunning, Msg: "spawned"},
		{Service: "backend", Phase: supervisor.PhaseReady, Msg: "ready in 1.2s"},
	}
	s := newRecorderServer(t, ctrl)

	req := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+"/api/backend/restart", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST restart = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp mutationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Operation != string(opRestart) || !resp.OK {
		t.Errorf("response = %+v, want a successful restart", resp)
	}
	if len(resp.Events) != 2 {
		t.Errorf("got %d events, want the 2 the supervisor emitted", len(resp.Events))
	}

	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if len(ctrl.calls) != 1 || ctrl.calls[0].kind != opRestart ||
		len(ctrl.calls[0].names) != 1 || ctrl.calls[0].names[0] != "backend" {
		t.Fatalf("supervisor calls = %+v, want one restart of backend", ctrl.calls)
	}
}

func TestBulkMutationTargetsEveryService(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	s := newRecorderServer(t, ctrl)

	// start-all and stop-all now spell "everything" differently, and the
	// difference is deliberate. stop-all passes the empty default selection,
	// which for a stop already means every service. start-all NAMES them,
	// because the empty selection is what `autostart: false` narrows — a
	// "Start all" button that a per-service default can turn into a no-op is a
	// broken control.
	for path, want := range map[string]opKind{"/api/start-all": opStart, "/api/stop-all": opStop} {
		req := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+path, nil)
		req.Header.Set(tokenHeader, s.Token())
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s = %d, want 200", path, rec.Code)
		}
		var resp mutationResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
		if resp.Operation != string(want) {
			t.Errorf("POST %s ran %q, want %q", path, resp.Operation, want)
		}
		switch want {
		case opStart:
			if len(resp.Services) != len(ctrl.insts) {
				t.Errorf("POST %s named %v, want every declared service named explicitly",
					path, resp.Services)
			}
		default:
			if len(resp.Services) != 0 {
				t.Errorf("POST %s named services %v, want the empty set that means all",
					path, resp.Services)
			}
		}
	}
}

func TestFailedOperationIsReportedInABody(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	ctrl.opErr = fmt.Errorf("backend: %w", supervisor.ErrNotStarted)
	s := newRecorderServer(t, ctrl)

	req := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+"/api/backend/start", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("a service that failed to start = %d, want 200 with ok:false", rec.Code)
	}
	var resp mutationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.OK || !strings.Contains(resp.Error, "did not start") {
		t.Errorf("response = %+v, want ok:false naming the failure", resp)
	}
}

// ---------------------------------------------------------------------------
// {svc} validation
// ---------------------------------------------------------------------------

func TestUnknownServiceIs404AndNeverReachesTheSupervisor(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	s := newRecorderServer(t, ctrl)

	type probe struct {
		method string
		path   string
	}
	probes := []probe{
		{http.MethodGet, "/api/logs/nope"},
		{http.MethodGet, "/api/stream/nope"},
		{http.MethodPost, "/api/nope/start"},
		{http.MethodPost, "/api/nope/stop"},
		{http.MethodPost, "/api/nope/restart"},
		// A name that would traverse a path if it were ever used to compose
		// one. config already rejects such a name at load time; this is the
		// front end refusing to forward it regardless.
		{http.MethodGet, "/api/logs/..%2f..%2fetc%2fpasswd"},
		{http.MethodPost, "/api/..%2f..%2fetc/start"},
	}
	for _, p := range probes {
		req := httptest.NewRequest(p.method, "http://"+recorderAddr+p.path, nil)
		req.Header.Set(tokenHeader, s.Token())
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", p.method, p.path, rec.Code)
			continue
		}
		var resp errorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			continue // a mux-level 404 has no JSON body, which is fine
		}
		if len(resp.Valid) != 2 || resp.Valid[0] != "backend" {
			t.Errorf("%s %s: 404 body = %+v, want it to name the valid services",
				p.method, p.path, resp)
		}
	}

	if n := ctrl.callCount(); n != 0 {
		t.Fatalf("%d operations on an unknown service reached the supervisor, want 0", n)
	}
	if n := ctrl.tailCount(); n != 0 {
		t.Fatalf("%d tails of an unknown service reached the supervisor, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// /api/services — the declared view, and nothing more
// ---------------------------------------------------------------------------

func TestServicesRedactsDeclaredSecretsButKeepsKeys(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())

	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/services", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/services = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, declaredSecret) {
		t.Fatalf("the declared API_TOKEN value appears in /api/services:\n%s", body)
	}
	if strings.Contains(body, "hunter2") {
		t.Fatalf("a password embedded in DATABASE_URL appears in /api/services:\n%s", body)
	}

	var got []serviceInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d services, want 2", len(got))
	}

	env := map[string]redact.Var{}
	for _, e := range got[0].Env {
		env[e.Key] = e
	}
	for _, key := range []string{"API_TOKEN", "DATABASE_URL", "LOG_LEVEL"} {
		if _, ok := env[key]; !ok {
			t.Errorf("declared key %s is missing; a developer needs to know the variable is set", key)
		}
	}
	if e := env["API_TOKEN"]; !e.Redacted || e.Value != redact.Mark {
		t.Errorf("API_TOKEN = %+v, want a redacted value", e)
	}
	if e := env["DATABASE_URL"]; !e.Redacted {
		t.Errorf("DATABASE_URL = %+v, want a redacted value", e)
	}
	if e := env["LOG_LEVEL"]; e.Redacted || e.Value != "debug" {
		t.Errorf("LOG_LEVEL = %+v, want the real value: it is not a credential", e)
	}
}

func TestServicesNeverRendersTheInheritedEnvironment(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())

	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/services", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	body := rec.Body.String()

	// Instance.Env is the caller's whole environment forwarded to the child.
	// Neither its values nor its keys may appear: the key list alone is a map
	// of what the developer has configured on their machine.
	for _, forbidden := range []string{inheritedSecret, inheritedKey, "HOME"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("%q from the inherited environment appears in /api/services:\n%s", forbidden, body)
		}
	}
}

func TestServicesShowsTheCommandAndWhereItRuns(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())

	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/services", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	var got []serviceInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	be := got[0]
	if be.Dir != "/repo/backend" || be.Port != 7100 || be.Runtime != "conda:api" {
		t.Errorf("backend = %+v, want dir, port and runtime rendered", be)
	}
	if len(be.Cmd) != 6 || be.Cmd[0] != "/usr/bin/python3" {
		t.Errorf("cmd = %v, want the resolved argv", be.Cmd)
	}
	want := "/usr/bin/python3 -m uvicorn app:main --port 7100"
	if be.CmdLine != want {
		t.Errorf("cmd_line = %q, want %q", be.CmdLine, want)
	}
	if be.Health == "" || !strings.Contains(be.Health, "&deep=0") {
		t.Errorf("health = %q, want the expanded URL intact", be.Health)
	}
	if got[1].DependsOn[0] != "backend" {
		t.Errorf("frontend depends_on = %v, want [backend]", got[1].DependsOn)
	}
}

func TestServicesReportsAnUnresolvableCommand(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	ctrl.insts[0].CmdErr = errors.New("conda:api: /opt/conda/envs/api/bin/uvicorn does not exist")
	s := newRecorderServer(t, ctrl)

	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/services", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	var got []serviceInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if !strings.Contains(got[0].CmdError, "does not exist") {
		t.Errorf("cmd_error = %q, want the resolution failure", got[0].CmdError)
	}
}

// ---------------------------------------------------------------------------
// /api/status — the same bytes as `mabo-ctl status --json`
// ---------------------------------------------------------------------------

func TestStatusIsByteIdenticalToStatusJSON(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	s := newRecorderServer(t, ctrl)

	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/status", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/status = %d, want 200", rec.Code)
	}
	want, err := ui.StatusJSON(ctrl.statuses)
	if err != nil {
		t.Fatalf("ui.StatusJSON: %v", err)
	}
	if got := rec.Body.Bytes(); string(got) != string(want) {
		t.Fatalf("/api/status is not what `mabo-ctl status --json` emits\n got: %s\nwant: %s", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// ---------------------------------------------------------------------------
// logs
// ---------------------------------------------------------------------------

func TestLogsReturnsTheTail(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	s := newRecorderServer(t, ctrl)

	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/logs/backend?tail=2", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/logs/backend = %d, want 200", rec.Code)
	}
	var resp logsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp.Service != "backend" || len(resp.Lines) != 2 || resp.Lines[0] != "listening on 7100" {
		t.Fatalf("logs = %+v, want the two fixture lines", resp)
	}
	if !ctrl.tailsFinished() {
		t.Error("a non-following tail was left running after the response")
	}
}

func TestTailCountIsClamped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want int
	}{
		{"", defaultLogTail},
		{"abc", defaultLogTail},
		{"0", defaultLogTail},
		{"-5", defaultLogTail},
		{"50", 50},
		{"999999999", maxLogTail},
	}
	for _, tc := range cases {
		if got := tailCount(tc.raw); got != tc.want {
			t.Errorf("tailCount(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// SSE — the leak this package exists to not have
// ---------------------------------------------------------------------------

func TestStreamEndsAndStopsTheTailWhenTheClientDisconnects(t *testing.T) {
	ctrl := twoServices()
	s, base := newLiveServer(t, ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	resp := openStream(t, ctx, sse(s, base, "/api/stream/backend"))

	br := bufio.NewReader(resp.Body)
	readSSEData(t, br) // the first log line proves the stream is live

	if got := s.streams.Load(); got != 1 {
		t.Fatalf("live streams = %d, want 1", got)
	}
	waitFor(t, "the tail to start", func() bool { return ctrl.tailCount() == 1 })

	cancel()
	_ = resp.Body.Close()

	waitFor(t, "the tail to stop when the tab closed", ctrl.tailsFinished)
	waitFor(t, "the stream handler to return", func() bool { return s.streams.Load() == 0 })
}

func TestStreamEndsEvenWithTheProducerMidSend(t *testing.T) {
	ctrl := twoServices()
	// Far more output than the channel between the tail and the handler can
	// hold, so the tail is blocked on a send when the tab closes. A handler
	// that returned without cancelling and draining would strand it there.
	ctrl.lines = make([]string, streamBuffer*8)
	for i := range ctrl.lines {
		ctrl.lines[i] = fmt.Sprintf("chatty line %d", i)
	}
	s, base := newLiveServer(t, ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	resp := openStream(t, ctx, sse(s, base, "/api/stream/backend"))
	readSSEData(t, bufio.NewReader(resp.Body))

	cancel()
	_ = resp.Body.Close()

	waitFor(t, "the blocked tail to return", ctrl.tailsFinished)
	waitFor(t, "the stream handler to return", func() bool { return s.streams.Load() == 0 })
}

func TestManyOpenedAndClosedStreamsLeakNothing(t *testing.T) {
	ctrl := twoServices()
	s, base := newLiveServer(t, ctrl)

	// One warm-up round so the transport's own goroutines exist before the
	// baseline is taken.
	openAndCloseStream(t, sse(s, base, "/api/stream/backend"))
	waitFor(t, "the warm-up stream to end", func() bool { return s.streams.Load() == 0 })
	runtime.GC()
	baseline := runtime.NumGoroutine()

	const rounds = 20
	for i := 0; i < rounds; i++ {
		openAndCloseStream(t, sse(s, base, "/api/stream/backend"))
	}

	waitFor(t, "every stream handler to return", func() bool { return s.streams.Load() == 0 })
	waitFor(t, "every tail to return", ctrl.tailsFinished)
	if n := ctrl.tailCount(); n != rounds+1 {
		t.Fatalf("started %d tails, want %d", n, rounds+1)
	}
	waitFor(t, "the goroutine count to come back down", func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= baseline+5
	})
}

func TestStreamHeadersAndHeartbeat(t *testing.T) {
	ctrl := twoServices()
	ctrl.lines = nil // an idle stream: only the heartbeat should arrive
	s, base := newLiveServer(t, ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := openStream(t, ctx, sse(s, base, "/api/stream/backend"))
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}

	// The heartbeat is 20ms in tests. Reading it at all proves both that it is
	// sent and that every write is flushed rather than buffered.
	br := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the stream: %v", err)
		}
		if strings.HasPrefix(line, ": heartbeat") {
			return
		}
	}
	t.Fatal("no heartbeat comment arrived on an idle stream")
}

func TestEventsStreamReachesEveryOpenConsole(t *testing.T) {
	ctrl := twoServices()
	ctrl.events = []supervisor.Event{
		{Service: "backend", Phase: supervisor.PhaseRunning, Msg: "spawned"},
		{Service: "backend", Phase: supervisor.PhaseReady, Msg: "ready"},
	}
	s, base := newLiveServer(t, ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Two consoles, as in two browser tabs. A single supervisor channel read
	// by one handler would starve the second one entirely.
	const viewers = 2
	readers := make([]*bufio.Reader, viewers)
	for i := range readers {
		resp := openStream(t, ctx, sse(s, base, "/api/events"))
		defer resp.Body.Close() //nolint:revive // one per viewer, released at test end
		readers[i] = bufio.NewReader(resp.Body)
	}
	waitFor(t, "both consoles to subscribe", func() bool { return s.events.subscribers() == viewers })

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/backend/start", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set(tokenHeader, s.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST start: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	for i, br := range readers {
		var ev eventJSON
		if err := json.Unmarshal(readSSEData(t, br), &ev); err != nil {
			t.Fatalf("viewer %d: decoding event: %v", i, err)
		}
		if ev.Service != "backend" || ev.Msg != "spawned" {
			t.Errorf("viewer %d got %+v, want the first supervisor event", i, ev)
		}
	}
}

func TestEventsStreamEndsWhenTheClientDisconnects(t *testing.T) {
	ctrl := twoServices()
	s, base := newLiveServer(t, ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	resp := openStream(t, ctx, sse(s, base, "/api/events"))
	waitFor(t, "the console to subscribe", func() bool { return s.events.subscribers() == 1 })

	cancel()
	_ = resp.Body.Close()

	waitFor(t, "the subscription to be released", func() bool { return s.events.subscribers() == 0 })
	waitFor(t, "the events handler to return", func() bool { return s.streams.Load() == 0 })
}

// ---------------------------------------------------------------------------
// the broker
// ---------------------------------------------------------------------------

func TestBrokerFansOutAndDropsOnlyForTheSlowSubscriber(t *testing.T) {
	t.Parallel()
	b := newBroker()

	fastA, releaseA := b.subscribe()
	fastB, releaseB := b.subscribe()
	slow, releaseSlow := b.subscribe()
	defer releaseA()
	defer releaseB()
	defer releaseSlow()

	// The slow subscriber never reads, so its buffer fills and stays full.
	const n = subscriberBuffer * 2
	for i := 0; i < n; i++ {
		e := supervisor.Event{Service: "backend", Msg: fmt.Sprintf("event %d", i)}
		done := make(chan struct{})
		go func() { defer close(done); b.publish(e) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("publish blocked on the slow subscriber at event %d", i)
		}
		// Drain the fast ones as we go, which is what a live SSE handler does.
		if got := <-fastA; got.Msg != e.Msg {
			t.Fatalf("fast subscriber A got %q, want %q", got.Msg, e.Msg)
		}
		if got := <-fastB; got.Msg != e.Msg {
			t.Fatalf("fast subscriber B got %q, want %q", got.Msg, e.Msg)
		}
	}

	if len(slow) != subscriberBuffer {
		t.Errorf("the slow subscriber holds %d events, want its buffer full at %d",
			len(slow), subscriberBuffer)
	}
	if b.dropped == 0 {
		t.Error("no drop was recorded for a subscriber that never read")
	}
}

func TestBrokerReleaseAndCloseAreIdempotent(t *testing.T) {
	t.Parallel()
	b := newBroker()

	ch, release := b.subscribe()
	if b.subscribers() != 1 {
		t.Fatalf("subscribers = %d, want 1", b.subscribers())
	}
	release()
	release()
	if b.subscribers() != 0 {
		t.Fatalf("subscribers = %d after release, want 0", b.subscribers())
	}
	if _, ok := <-ch; ok {
		t.Error("a released subscription still delivers events")
	}

	// Publishing to nobody, and after close, must not panic.
	b.publish(supervisor.Event{Msg: "nobody is listening"})
	b.close()
	b.close()
	b.publish(supervisor.Event{Msg: "still nobody"})

	// A subscription taken after close is already closed, so a racing handler
	// ends immediately instead of waiting forever.
	after, releaseAfter := b.subscribe()
	defer releaseAfter()
	if _, ok := <-after; ok {
		t.Error("subscribing after close yielded a live channel")
	}
}

// ---------------------------------------------------------------------------
// the page
// ---------------------------------------------------------------------------

func TestIndexCarriesTheTokenAndALockedDownCSP(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())

	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), s.Token()) {
		t.Error("the console page does not carry the session token, so every button would 403")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "connect-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q is missing %q", csp, want)
		}
	}
}

// TestUnauthenticatedRequestsNeverSeeTheToken is the regression test for the
// hole that made every other control in this package decorative.
//
// The five mutating routes were token-guarded, and then GET / handed that same
// token to anybody who asked, in a meta tag. A process running as another uid on
// a shared host — an attacker this project's threat model names explicitly —
// could scrape it with one curl and drive start/stop/restart as the developer.
// The token guarded the door and was posted on the door.
//
// The read routes are covered by the same loop because they are the other half:
// /api/status runs health probes as a side effect, so leaving it open let any
// page the developer visited use mabo-ctl as a request beacon.
func TestUnauthenticatedRequestsNeverSeeTheToken(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())

	for _, target := range []string{
		"/api/services", "/api/config", "/api/status",
		"/api/logs/backend", "/api/stream/backend", "/api/events",
	} {
		// The two SSE targets carry a deadline. A refused request returns long
		// before it expires, but an ACCEPTED one streams until its context ends —
		// so without this the regression shows up as a hung test rather than a
		// failing one, which is a much worse way to learn the guard is gone.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+target, nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, req)
		cancel()

		if rec.Code != http.StatusForbidden {
			t.Errorf("unauthenticated GET %s = %d, want 403", target, rec.Code)
		}
		if strings.Contains(rec.Body.String(), s.Token()) {
			t.Errorf("unauthenticated GET %s disclosed the session token", target)
		}
	}
}

// TestTheUnlockPageCarriesNoToken covers the index, which answers an
// unauthenticated browser differently from the API routes: a person who opened a
// bookmark, or reloaded after the page stripped the token out of the address
// bar, gets a box to paste it into. The property that matters is unchanged and
// is asserted here — that response must not contain the credential.
func TestTheUnlockPageCarriesNoToken(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())

	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/", nil)
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET / = %d, want 401", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, s.Token()) {
		t.Fatal("the unlock page disclosed the session token")
	}
	// It has to be usable, or it is just a 403 with extra steps.
	for _, want := range []string{`name="token"`, `method="get"`, `action="/"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the unlock page has no %s, so there is no way to submit a token", want)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp == "" {
		t.Error("the unlock page carries no CSP")
	}
}

// TestDocsPageServesRapidocWithTokenWiredIn checks that the API reference page
// is the RapiDoc API Reference, with the session token wired into the api-key
// (X-Mabo-Ctl-Token in the header) so try-it requests are authenticated exactly
// like the console buttons, and the server pinned to the address actually
// bound. Like the console page, this is delivered only to a caller who has
// already proven possession of the token.
func TestDocsPageServesRapidocWithTokenWiredIn(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())

	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api-docs", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api-docs = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// The page is driven by the vendored RapiDoc web component.
	for _, want := range []string{
		`id="api-reference"`,
		`<rapi-doc`,
		`spec-url="/api/openapi.yaml"`,
		`<script src="/api-docs/rapidoc.js">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the docs page does not contain %s", want)
		}
	}
	// The token is wired in as the api-key value: try-it is authenticated.
	if !strings.Contains(body, `api-key-value="`+s.Token()) {
		t.Errorf("the docs page does not contain the wired-in token")
	}
	// The server is pinned to the address actually bound, not the spec's
	// 127.0.0.1:7999 default, so try-it reaches this console on any port.
	if !strings.Contains(body, `server-url="http://`+recorderAddr) {
		t.Errorf("the docs page does not contain the bound server address")
	}

	// The docs page uses the relaxed CSP that allows its same-origin script,
	// and nothing else loosened.
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'unsafe-inline' 'self'") {
		t.Errorf("CSP %q is missing script-src 'unsafe-inline' 'self'", csp)
	}
	if !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("CSP %q is missing connect-src 'self'", csp)
	}
}

// TestRapidocBundleRouteServed pins the vendored RapiDoc bundle's route: the
// page points at it with a script tag, so the same origin must serve it,
// session-gated like the rest of the docs surface.
func TestRapidocBundleRouteServed(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())

	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api-docs/rapidoc.js", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api-docs/rapidoc.js = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("Content-Type = %q, want application/javascript", ct)
	}
	for _, want := range []string{"RapiDoc", "rapi-doc"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the served bundle does not contain %q", want)
		}
	}

	// Unauthenticated, the bundle is refused like every docs route.
	req2 := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api-docs/rapidoc.js", nil)
	rec2 := httptest.NewRecorder()
	s.h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /api-docs/rapidoc.js = %d, want 401", rec2.Code)
	}
}

// TestUnauthenticatedDocsPageLeaksNeitherTokenNorSpec pins the security
// boundary of /api-docs: exactly one of the two responses carries the token
// and the page, and it is not this one.
func TestUnauthenticatedDocsPageLeaksNeitherTokenNorSpec(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())

	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api-docs", nil)
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api-docs = %d, want 401", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, s.Token()) {
		t.Fatal("an unauthenticated /api-docs response disclosed the session token")
	}
	if strings.Contains(body, "<rapi-doc") {
		t.Error("an unauthenticated /api-docs response disclosed the API reference page")
	}
}

// TestEmbeddedOpenAPIMatchesTheDocsCopy pins the two copies of the spec — the
// one embedded in this package and the one under docs/ that humans and code
// generators read — as byte-identical. Serving a spec that disagrees with the
// published one would drift the machine surface from its documentation, which
// is the failure the drift gate exists for. The docs copy resolves relative to
// the package directory, which `go test` always uses as the working directory.
func TestEmbeddedOpenAPIMatchesTheDocsCopy(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatalf("reading docs/openapi.yaml: %v", err)
	}
	if got := openapiSpec; got != string(doc) {
		t.Fatalf("internal/web/openapi.yaml differs from docs/openapi.yaml.\n" +
			"Regenerate with: cp docs/openapi.yaml internal/web/openapi.yaml\n")
	}
}

// TestEveryRouteIsDocumentedInTheAPIReference gates docs/API.md against the
// live route table, the same way surfaces.json gates the machine surfaces: a
// route added to [consoleRoutes] without a matching `### \`METHOD /path\“
// heading in the human-readable reference fails here, instead of quietly
// shipping documentation that enumerates fewer endpoints than the server
// serves.
func TestEveryRouteIsDocumentedInTheAPIReference(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "API.md"))
	if err != nil {
		t.Fatalf("reading docs/API.md: %v", err)
	}
	text := string(doc)

	for _, r := range Routes() {
		path := r.Path
		if path == "/{$}" {
			path = "/"
		}
		heading := "### `" + r.Method + " " + path + "`"
		if !strings.Contains(text, heading) {
			t.Errorf("route %s %s is not documented in docs/API.md; add a %s section",
				r.Method, r.Path, heading)
		}
	}
}

// TestAWrongTokenIsRefusedEverywhere pins that the check is a comparison and not
// a presence test.
func TestAWrongTokenIsRefusedEverywhere(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())
	wrong := strings.Repeat("0", len(s.Token()))

	// The index answers 401 with the unlock page and the API routes answer 403;
	// what both must never do is serve the console or the token.
	for _, tc := range []struct {
		target string
		want   int
	}{{"/", http.StatusUnauthorized}, {"/api/status", http.StatusForbidden}} {
		for _, req := range []*http.Request{
			httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+tc.target+"?token="+wrong, nil),
			httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+tc.target, nil),
		} {
			req.Header.Set(tokenHeader, wrong)
			req.AddCookie(&http.Cookie{Name: s.cookieName(), Value: wrong})
			rec := httptest.NewRecorder()
			s.h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("GET %s with a wrong token = %d, want %d", tc.target, rec.Code, tc.want)
			}
			if strings.Contains(rec.Body.String(), s.Token()) {
				t.Errorf("GET %s with a wrong token disclosed the token", tc.target)
			}
		}
	}
}

// TestTheQueryTokenMintsACookieSoAReloadWorks covers the reason the cookie
// exists at all. The page strips the token from the address bar on load, so
// without a cookie the next F5 would 403 the developer out of their own console.
func TestTheQueryTokenMintsACookieSoAReloadWorks(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())

	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/?token="+s.Token(), nil)
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /?token= = %d, want 200", rec.Code)
	}

	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == s.cookieName() {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie was set, so reloading the console page would 403")
	}
	if !cookie.HttpOnly {
		t.Error("the session cookie is readable by script; it carries the token that starts services")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Error("the session cookie is not SameSite=Strict, so a foreign page could ride it")
	}

	// The reload: address bar stripped, cookie only.
	reload := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/", nil)
	reload.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	s.h.ServeHTTP(rec2, reload)
	if rec2.Code != http.StatusOK {
		t.Fatalf("reload with the session cookie = %d, want 200", rec2.Code)
	}
}

// TestTheSessionCookieNeverAuthorisesAMutation is the line between the two
// guards. The cookie makes reloads work; it must never make a POST work, because
// a cookie is exactly the credential a cross-origin request can carry.
func TestTheSessionCookieNeverAuthorisesAMutation(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	s := newRecorderServer(t, ctrl)

	req := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+"/api/backend/start", nil)
	req.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.Token()})
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("POST authorised by a cookie = %d, want 403", rec.Code)
	}
	if n := ctrl.callCount(); n != 0 {
		t.Errorf("%d cookie-authorised mutations reached the supervisor, want 0", n)
	}
}

// TestTheQueryTokenNeverAuthorisesAMutation closes the other half: the query is
// accepted on reads because EventSource cannot send a header, and a mutation
// must not inherit that concession — a query string rides an <img> tag.
func TestTheQueryTokenNeverAuthorisesAMutation(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	s := newRecorderServer(t, ctrl)

	req := httptest.NewRequest(http.MethodPost,
		"http://"+recorderAddr+"/api/backend/start?token="+s.Token(), nil)
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("POST authorised by a query token = %d, want 403", rec.Code)
	}
	if n := ctrl.callCount(); n != 0 {
		t.Errorf("%d query-authorised mutations reached the supervisor, want 0", n)
	}
}

// TestAStaleCookieDoesNotVetoAGoodQueryToken covers the multi-console case.
// Cookies are not port-scoped, so a browser holding console A's cookie sends it
// to console B as well; if the cookie were consulted first and treated as
// decisive, opening a second console would 403 with a perfectly good token in
// the URL.
func TestAStaleCookieDoesNotVetoAGoodQueryToken(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())

	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/?token="+s.Token(), nil)
	req.AddCookie(&http.Cookie{Name: s.cookieName(), Value: strings.Repeat("f", 64)})
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /?token= with a stale cookie = %d, want 200", rec.Code)
	}
}

func TestInjectTokenPlacesItInsideHead(t *testing.T) {
	t.Parallel()
	out := string(injectToken([]byte("<html><head><title>x</title></head><body></body></html>"), "abc123"))
	if !strings.Contains(out, `content="abc123"`) {
		t.Fatalf("token was not injected: %s", out)
	}
	if strings.Index(out, "abc123") > strings.Index(out, "</head>") {
		t.Errorf("token was injected outside <head>: %s", out)
	}

	// A page with no head at all still gets the token, at the top.
	bare := string(injectToken([]byte("<p>hello</p>"), "abc123"))
	if !strings.HasPrefix(bare, `<meta name="mabo-ctl-token"`) {
		t.Errorf("a page without a head did not get the token first: %s", bare)
	}
}

func TestPageTemplateFillsInTheToken(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())
	tmpl, err := templateFor(`<html><head><meta name="t" content="{{.Token}}"></head></html>`)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	s.tmpl = tmpl

	page := string(s.renderPage())
	if strings.Count(page, s.Token()) != 1 {
		t.Errorf("want the token rendered exactly once by the template, got:\n%s", page)
	}
}

// ---------------------------------------------------------------------------
// the real supervisor
// ---------------------------------------------------------------------------

// TestOverARealSupervisor wires the exported constructor to a real
// *supervisor.Supervisor. It spawns nothing: it proves the concrete type
// satisfies Controller, that the declared environment reaches the page through
// supervisor.Config, and that the resolved instance environment does not.
func TestOverARealSupervisor(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	st, err := state.New(root)
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	cfg := &config.Config{
		Root: root,
		Services: []config.Spec{{
			Name: "backend",
			Env:  map[string]string{"API_TOKEN": declaredSecret, "LOG_LEVEL": "debug"},
		}},
		StopGrace:    time.Second,
		ReadyTimeout: time.Second,
	}
	insts := []service.Instance{{
		Name: "backend",
		Dir:  root,
		Cmd:  []string{"/bin/echo", "hi"},
		Env:  []string{inheritedKey + "=" + inheritedSecret},
	}}
	sup := supervisor.New(cfg, st, insts)

	s, err := New(sup, Options{Addr: recorderAddr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/services", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/services = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "API_TOKEN") || !strings.Contains(body, "LOG_LEVEL") {
		t.Errorf("declared keys are missing from %s", body)
	}
	if strings.Contains(body, declaredSecret) || strings.Contains(body, inheritedSecret) {
		t.Fatalf("a secret reached the page: %s", body)
	}

	// And the real supervisor's status serialises through ui.StatusJSON.
	req = httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/status", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec = httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/status = %d, want 200", rec.Code)
	}
	want, err := ui.StatusJSON(sup.Status(context.Background()))
	if err != nil {
		t.Fatalf("ui.StatusJSON: %v", err)
	}
	if rec.Body.String() != string(want) {
		t.Errorf("/api/status = %s, want %s", rec.Body.String(), want)
	}
}

// ---------------------------------------------------------------------------
// lifecycle
// ---------------------------------------------------------------------------

func TestShutdownEndsInFlightStreams(t *testing.T) {
	ctrl := twoServices()
	s, err := NewWith(ctrl, Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}
	s.heartbeat = 20 * time.Millisecond
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	base := "http://" + s.Addr()

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.ListenAndServe(ctx) }()

	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	resp := openStream(t, streamCtx, sse(s, base, "/api/stream/backend"))
	defer resp.Body.Close()
	waitFor(t, "the stream to be live", func() bool { return s.streams.Load() == 1 })

	// Shutdown must not wait for an SSE response that never ends on its own.
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("ListenAndServe: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ListenAndServe did not return: an SSE handler is holding shutdown open")
	}
	waitFor(t, "the stream handler to return", func() bool { return s.streams.Load() == 0 })
	waitFor(t, "the tail to return", ctrl.tailsFinished)
}

func TestListenIsIdempotentAndReportsTheRealPort(t *testing.T) {
	t.Parallel()
	s, err := NewWith(twoServices(), Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	first := s.Addr()
	if err := s.Listen(); err != nil {
		t.Fatalf("second Listen: %v", err)
	}
	if s.Addr() != first {
		t.Errorf("Addr changed on the second Listen: %q then %q", first, s.Addr())
	}
	if strings.HasSuffix(first, ":0") {
		t.Errorf("Addr() = %q, want the port the kernel actually assigned", first)
	}
	if !strings.Contains(s.URL(), first) {
		t.Errorf("URL() = %q, want it built from the bound address %q", s.URL(), first)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ListenAndServe did not return")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// sse builds the URL an EventSource actually opens: the path with the session
// token in the QUERY. A browser's EventSource cannot set a request header, so
// the query is the only credential the real console can present on a stream —
// which is exactly why requireSession accepts it there.
func sse(s *Server, base, path string) string {
	return base + path + "?token=" + s.Token()
}

// openStream issues a GET and returns the live response, failing the test if
// the stream did not open.
func openStream(t *testing.T, ctx context.Context, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("GET %s = %d, want 200", url, resp.StatusCode)
	}
	return resp
}

// openAndCloseStream opens a stream, reads one event and abandons it the way a
// closed browser tab does.
func openAndCloseStream(t *testing.T, url string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := openStream(t, ctx, url)
	readSSEData(t, bufio.NewReader(resp.Body))
	cancel()
	_ = resp.Body.Close()
}

// readSSEData reads until the next "data:" line and returns its payload.
func readSSEData(t *testing.T, br *bufio.Reader) []byte {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the stream: %v", err)
		}
		if payload, ok := strings.CutPrefix(line, "data: "); ok {
			return []byte(strings.TrimRight(payload, "\r\n"))
		}
	}
	t.Fatal("no data event arrived")
	return nil
}

// ---------------------------------------------------------------------------
// the page and the phase vocabulary
// ---------------------------------------------------------------------------

// TestConsolePageKnowsEveryPhase is the drift guard between the browser and the
// terminal. The page carries its own copy of the phase table because it is
// plain JavaScript and cannot import internal/ui; that copy has to be a MIRROR,
// not a fork.
//
// This is the same class of divergence the page's deleted reconcileFailed()
// belonged to: the console used to relabel the server's phase on the client, so
// the browser said "failed" while the terminal said "stopped" for one service at
// one instant. A phase the server can report and the page has no row for renders
// as "?" — the same bug wearing a different coat.
func TestConsolePageKnowsEveryPhase(t *testing.T) {
	t.Parallel()
	for _, phase := range supervisor.Phases() {
		glyph, word := ui.PhaseLabel(phase)

		if !strings.Contains(consoleHTML, string(phase)+":") {
			t.Errorf("console.html has no PHASES entry for %q", phase)
		}
		if !strings.Contains(consoleHTML, `"`+word+`"`) {
			t.Errorf("console.html does not carry the word %q for phase %q", word, phase)
		}
		if !strings.Contains(consoleHTML, `"`+glyph+`"`) {
			t.Errorf("console.html does not carry the glyph %q for phase %q — the browser and "+
				"the terminal would draw the same state differently", glyph, phase)
		}
		// Colour is never the only signal, but each phase still needs its own
		// class: two phases sharing one would make the tile row unreadable.
		if !strings.Contains(consoleHTML, ".p-"+string(phase)+" ") {
			t.Errorf("console.html has no .p-%s rule", phase)
		}
		if !strings.Contains(consoleHTML, ".tile.t-"+string(phase)+" ") {
			t.Errorf("console.html has no .tile.t-%s rule", phase)
		}
		// Every phase is a tile, so a stack with one exited service shows a
		// count rather than nothing.
		if !strings.Contains(consoleHTML, `"`+string(phase)+`"`) {
			t.Errorf("console.html does not list %q in TILE_ORDER", phase)
		}
	}
}

// TestConsolePageSortsTheAlarmingPhasesFirst checks the rank the page sorts by.
// A five-service stack with one dead service must not bury it fourth, and the
// three phases that mean "go and look" must outrank every healthy one.
func TestConsolePageKeepsDeclarationOrder(t *testing.T) {
	t.Parallel()
	// The list is painted in declaration order and never re-sorted: a service
	// that fails or comes ready keeps its row where mabo-ctl.yaml put it.
	// Rows that jumped on every start/stop made the list unreadable by muscle
	// memory, and position was always a redundant attention signal — the glyph,
	// the colour and the summary tiles carry it. This pins the decision
	// structurally: no phase ranks exist to sort by, and paintList pushes rows
	// straight off the services array with no comparator.
	if strings.Contains(consoleHTML, "rank: ") || strings.Contains(consoleHTML, ".rank()") {
		t.Fatal("console.html still carries a phase rank; the list must not be sortable by phase")
	}
	from := strings.Index(consoleHTML, "function paintList()")
	if from < 0 {
		t.Fatal("console.html no longer declares paintList")
	}
	body := consoleHTML[from:]
	body = body[:strings.Index(body, "\n  }\n")]
	if strings.Contains(body, ".sort(") {
		t.Fatalf("paintList re-sorts its rows:\n%s", body)
	}
	if !strings.Contains(body, "for (var i = 0; i < services.length; i++)") {
		t.Fatalf("paintList no longer walks the services array in declaration order:\n%s", body)
	}
}

// TestConsolePageHasNoClientSidePhaseReconciliation checks that the workaround
// deleted alongside this feature stays deleted. The server now persists the
// death it observes, so the page has no reason to remember one — and a page
// that remembers is a second read implementation of a question the server
// already answers.
func TestConsolePageHasNoClientSidePhaseReconciliation(t *testing.T) {
	t.Parallel()
	for _, gone := range []string{"reconcileFailed", "failedSince"} {
		if strings.Contains(consoleHTML, gone) {
			t.Errorf("console.html still mentions %s; the server answers this question now", gone)
		}
	}
}

// TestConsolePageClassifiesAndFiltersLogLevels is the drift guard for the log
// pane's severity filter. The classification lives entirely in the page's
// JavaScript because the stream carries raw child output — the server never
// parses a line it only relays — so nothing in Go can vouch that the feature
// survived a page rewrite. These assertions pin the pieces that make it work:
// the detector, one CSS rule per bucket, the pressed-state chip grammar and
// the match gate that lets a level filter compose with the text filter.
func TestConsolePageClassifiesAndFiltersLogLevels(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"function detectLevel(", // the classifier itself
		`"log.level"`,           // ECS dotted key first: the common case
		".log-line.lvl-error ",  // one tint per bucket …
		".log-line.lvl-warn ",
		".log-line.lvl-debug ",
		".log-line.ok ",                           // … success folded into info keeps its own ink
		`aria-label", "filter log lines by level`, // the chip group is announced
		`"All"`, `"Err"`, `"Warn"`, `"Info"`, `"Dbg"`, // the whole chip set
		"setLevelFilter",               // chips actually drive the pane …
		"rec.lvl === this.levelFilter", // … through the same matches() gate as the text filter
	} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("console.html lost %q; the log level filter is not wired", want)
		}
	}
}

// TestStatusRedactsCredentialsInTheProbeFailure covers the channel that opened
// when the supervisor started quoting the dial error in Detail: a failed probe
// names the URL it dialled VERBATIM, so redacting the health field alone would
// hand the same credential out through the field beside it.
func TestStatusRedactsCredentialsInTheProbeFailure(t *testing.T) {
	t.Parallel()
	const raw = "http://admin:hunter2@127.0.0.1:1/health"
	ctrl := twoServices()
	ctrl.statuses = []supervisor.Status{{
		Name:   "backend",
		Phase:  supervisor.PhaseDegraded,
		Health: raw,
		Detail: "health: HEAD " + raw + ": dial tcp 127.0.0.1:1: connect: connection refused",
	}}
	s := newRecorderServer(t, ctrl)

	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/status", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "hunter2") {
		t.Fatalf("the probe failure leaked the credential:\n%s", body)
	}
	if !strings.Contains(body, "connection refused") {
		t.Fatalf("redaction destroyed the diagnosis, which is the whole reason to show it:\n%s", body)
	}
}

// TestEveryPhaseDeclaresWhetherItIsAlive guards the table the action buttons
// now read.
//
// The console used to offer Start on a service it was showing as ready and Stop
// on one it called stopped, because the buttons only knew whether a request was
// in flight. They now key off PHASES[…].alive, which makes that flag part of the
// contract: a phase added to supervisor.Phases() without one would silently come
// out as "not alive", and its Stop button would be dead on a running service.
//
// The expectation is restated here rather than read from Go because the
// supervisor has no aliveness helper — adding an exported one used only by this
// test would be worse than the restatement. This test is the enforcement point;
// if it fails, decide the new phase's aliveness in console.html deliberately.
func TestEveryPhaseDeclaresWhetherItIsAlive(t *testing.T) {
	t.Parallel()

	// Alive means a PROCESS is there. slow and degraded are alive but not
	// answering their probe; failed and exited describe one that is gone.
	wantAlive := map[supervisor.Phase]bool{
		supervisor.PhaseFailed:   false,
		supervisor.PhaseExited:   false,
		supervisor.PhaseStopped:  false,
		supervisor.PhaseSlow:     true,
		supervisor.PhaseDegraded: true,
		supervisor.PhaseRunning:  true,
		supervisor.PhaseReady:    true,
	}

	from := strings.Index(consoleHTML, "var PHASES = {")
	if from < 0 {
		t.Fatal("console.html has no PHASES table")
	}
	table := consoleHTML[from:]
	table = table[:strings.Index(table, "};")]

	for _, phase := range supervisor.Phases() {
		want, known := wantAlive[phase]
		if !known {
			t.Fatalf("supervisor.Phases() gained %q; decide its aliveness here and in console.html", phase)
		}
		at := strings.Index(table, string(phase)+":")
		if at < 0 {
			t.Fatalf("console.html has no PHASES entry for %q", phase)
		}
		row := table[at:]
		row = row[:strings.Index(row, "\n")]

		switch {
		case strings.Contains(row, "alive: true"):
			if !want {
				t.Errorf("%q is marked alive in console.html, but a %s service has no process", phase, phase)
			}
		case strings.Contains(row, "alive: false"):
			if want {
				t.Errorf("%q is marked not alive in console.html, so its Stop button would be dead "+
					"on a service that is running", phase)
			}
		default:
			t.Errorf("the PHASES entry for %q declares no aliveness, so its buttons would guess: %s", phase, row)
		}
	}
}

// TestStartAllStartsEvenServicesThatOptOutOfAutostart is the regression test
// for a broken button.
//
// "Start all" passed nil, which means "the default selection" — and
// autostart: false is exactly what narrows that default. On a stack where every
// service opts out (a realistic shape: a dozen workers you start by name) the
// button did nothing and reported "no service has autostart enabled". A true
// sentence, and a control that does not do what it is labelled. Clicking it is
// as explicit an instruction as typing the names.
func TestStartAllStartsEvenServicesThatOptOutOfAutostart(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	for i := range ctrl.insts {
		ctrl.insts[i].NoAutostart = true
	}
	s := newRecorderServer(t, ctrl)

	req := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+"/api/start-all", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/start-all = %d, want 200", rec.Code)
	}
	calls := ctrl.callsOf(opStart)
	if len(calls) != 1 {
		t.Fatalf("supervisor start calls = %d, want 1", len(calls))
	}
	if got := calls[0].names; len(got) != len(ctrl.insts) {
		t.Errorf("start-all selected %v, want every declared service — a button that says "+
			"\"all\" must not be narrowed by a per-service default", got)
	}
}

// ---------------------------------------------------------------------------
// the merged stream
// ---------------------------------------------------------------------------

// TestStreamAllFansEveryServiceIntoOneStream: /api/stream/all starts one tail
// per declared service, labels every line with its service on the wire, and
// leaves no tail or handler goroutine behind when the client leaves.
func TestStreamAllFansEveryServiceIntoOneStream(t *testing.T) {
	ctrl := twoServices()
	s, base := newLiveServer(t, ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	resp := openStream(t, ctx, sse(s, base, "/api/stream/all"))

	// Read until BOTH services have spoken — the interleaving is the
	// scheduler's business, not the test's. The fake tail replays the same
	// fixture lines for every service, so the assertion is the LABELLING and
	// the fan-in, not the content.
	seen := map[string]string{}
	br := bufio.NewReader(resp.Body)
	for len(seen) < 2 {
		var line logLine
		if err := json.Unmarshal(readSSEData(t, br), &line); err != nil {
			t.Fatalf("decoding a merged line: %v", err)
		}
		if line.Service == "" {
			t.Fatalf("a merged line arrived with no service label: %+v", line)
		}
		if _, dup := seen[line.Service]; !dup {
			seen[line.Service] = line.Line
		}
	}
	if seen["backend"] == "" || seen["frontend"] == "" {
		t.Fatalf("merged lines = %v, want a labelled line from each service", seen)
	}

	// One connection, one tails-per-service count, nothing left behind.
	if got := s.streams.Load(); got != 1 {
		t.Fatalf("live streams = %d, want 1 for the merged connection", got)
	}
	waitFor(t, "both tails to start", func() bool { return ctrl.tailCount() == 2 })

	cancel()
	_ = resp.Body.Close()
	waitFor(t, "both tails to stop when the tab closed", ctrl.tailsFinished)
	waitFor(t, "the stream handler to return", func() bool { return s.streams.Load() == 0 })
}

// TestStreamAllRequiresASession: the merged stream can read every log at once,
// which makes the session guard MORE important, not less.
func TestStreamAllRequiresASession(t *testing.T) {
	ctrl := twoServices()
	s := newRecorderServer(t, ctrl)

	req := httptest.NewRequest(http.MethodGet, "/api/stream/all", nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /api/stream/all without a session = %d, want 403", rec.Code)
	}
}

// TestConsolePageHasAMergedLogMode is the page-content drift guard for merged
// mode: the toggle, the sentinel, the labelling and the endpoint it opens.
func TestConsolePageHasAMergedLogMode(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		`"All services"`,             // the toggle button
		"bindAll",                    // entering merged mode
		`this.name = "all"`,          // the sentinel that keeps stale-frame guards working
		"all services",               // title and aria strings
		"lineText",                   // the labelling render
		`rec.svc + " · " + rec.text`, // the label format
	} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("console.html lost %q; merged log mode is not wired", want)
		}
	}
}
