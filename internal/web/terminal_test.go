package web

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// newTerminalServer is a recorder server with the Terminal tab on.
func newTerminalServer(t *testing.T, opt Options) *Server {
	t.Helper()
	t.Setenv("SHELL", "/bin/sh")
	terms := supervisor.NewTerminals(t.TempDir())
	t.Cleanup(terms.CloseAll)
	if opt.Addr == "" {
		opt.Addr = recorderAddr
	}
	opt.Terminals = terms
	s, err := NewWith(twoServices(), opt)
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}
	s.heartbeat = 20 * time.Millisecond
	return s
}

func terminalRequest(s *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://"+recorderAddr+path, strings.NewReader(body))
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	return rec
}

// TestTerminalRoutesAreOffUnlessEnabled pins the opt-in: without Terminals
// every terminal route is a 404, and the page says how to turn it on.
func TestTerminalRoutesAreOffUnlessEnabled(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/terminals"},
		{http.MethodPost, "/api/terminals"},
		{http.MethodPost, "/api/terminals/1/input"},
		{http.MethodGet, "/api/terminals/1/stream"},
	} {
		if rec := terminalRequest(s, c.method, c.path, `{}`); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s with the terminal off = %d, want 404", c.method, c.path, rec.Code)
		}
	}
	if page := string(s.renderPage()); !strings.Contains(page, "Terminal is off") {
		t.Error("the page does not explain how to enable the terminal")
	}
}

// TestTerminalDisablesOnNonLoopbackBind keeps the console available without
// granting shells through the ordinary dangerous-bind permission.
func TestTerminalDisablesOnNonLoopbackBind(t *testing.T) {
	t.Parallel()
	const addr = "192.168.0.27:7999"
	s, err := NewWith(twoServices(), Options{
		Addr:      addr,
		Force:     true,
		Terminals: supervisor.NewTerminals(t.TempDir()),
	})
	if err != nil {
		t.Fatalf("console should keep serving: %v", err)
	}
	if s.terminals != nil || s.opts.Terminals != nil {
		t.Fatal("remote terminals remain enabled without permission")
	}
	if page := string(s.renderPage()); !strings.Contains(page, "Terminal is off") {
		t.Fatal("the page does not show the disabled terminal box")
	}
	for _, route := range Routes() {
		if !strings.HasPrefix(route.Path, "/api/terminals") && route.Path != "/api/services" {
			continue
		}
		path := strings.ReplaceAll(route.Path, "{id}", "missing")
		r := httptest.NewRequest(route.Method, "http://"+addr+path, strings.NewReader(`{}`))
		r.Header.Set(tokenHeader, s.Token())
		r.Header.Set("Origin", "http://"+addr)
		w := httptest.NewRecorder()
		s.h.ServeHTTP(w, r)
		want := http.StatusNotFound
		if route.Path == "/api/services" {
			want = http.StatusOK
		}
		if w.Code != want {
			t.Errorf("%s %s = %d, want %d: %s", route.Method, path, w.Code, want, w.Body.String())
		}
	}
}

func TestRemoteTerminalBindRequiresBothPermissions(t *testing.T) {
	t.Parallel()
	for _, force := range []bool{false, true} {
		terms := supervisor.NewTerminals(t.TempDir())
		t.Cleanup(terms.CloseAll)
		_, err := NewWith(twoServices(), Options{
			Addr: "192.168.0.29:7999", Force: force,
			Terminals: terms, AllowRemoteTerminal: true,
		})
		if force && err != nil {
			t.Fatalf("explicit remote terminal permissions: %v", err)
		}
		if !force && !errors.Is(err, ErrUnsafeAddr) {
			t.Fatalf("without bind permission: %v, want ErrUnsafeAddr", err)
		}
	}
}

func TestRemoteTerminalKeepsHostOriginProxyAndAuthGuards(t *testing.T) {
	const addr = "192.168.0.29:7999"
	s := newTerminalServer(t, Options{Addr: addr, Force: true, AllowRemoteTerminal: true,
		AllowedOrigins: []string{"https://dev.tunnel.example"}})
	for _, c := range []struct {
		name, host, origin, forwarded, key string
		want                               int
	}{
		{"direct LAN", addr, "http://" + addr, "", s.Token(), http.StatusOK},
		{"no origin read", addr, "", "", s.Token(), http.StatusOK},
		{"wrong host", "192.168.0.30:7999", "", "", s.Token(), http.StatusForbidden},
		{"wrong port", "192.168.0.29:8000", "", "", s.Token(), http.StatusForbidden},
		{"wrong origin", addr, "http://192.168.0.30:7999", "", s.Token(), http.StatusForbidden},
		{"trusted origin", addr, "https://dev.tunnel.example", "", s.Token(), http.StatusOK},
		{"trusted host", "dev.tunnel.example", "https://dev.tunnel.example", "", s.Token(), http.StatusOK},
		{"proxy", addr, "http://" + addr, "for=192.168.0.20", s.Token(), http.StatusOK},
		{"untrusted host", "evil.example", "https://dev.tunnel.example", "", s.Token(), http.StatusForbidden},
		{"untrusted origin", "dev.tunnel.example", "https://evil.example", "", s.Token(), http.StatusForbidden},
		{"proxy cannot authorize", "evil.example", "https://evil.example", "host=dev.tunnel.example", s.Token(), http.StatusForbidden},
		{"invalid key", addr, "http://" + addr, "", "invalid", http.StatusForbidden},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://"+c.host+"/api/terminals", nil)
			r.Header.Set("Origin", c.origin)
			r.Header.Set("Forwarded", c.forwarded)
			r.Header.Set(tokenHeader, c.key)
			w := httptest.NewRecorder()
			s.h.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, c.want, w.Body.String())
			}
		})
	}
	// A direct authenticated LAN mutation really can open and close a shell.
	post := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://"+addr+path, strings.NewReader(body))
		r.Header.Set(tokenHeader, s.Token())
		r.Header.Set("Origin", "http://"+addr)
		w := httptest.NewRecorder()
		s.h.ServeHTTP(w, r)
		return w
	}
	w := post("/api/terminals", `{"cols":80,"rows":24}`)
	if w.Code != http.StatusOK {
		t.Fatalf("LAN open: %d %s", w.Code, w.Body.String())
	}
	var info terminalInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if w := post("/api/terminals/"+info.ID+"/close", ""); w.Code != http.StatusNoContent {
		t.Fatalf("LAN close: %d %s", w.Code, w.Body.String())
	}
	// Exercise every terminal route with the feature enabled, not merely the
	// closed permission matrix of a console whose terminals are off.
	for _, role := range []string{"read", "view", "write"} {
		_, key := createRemoteTestShare(t, s, addr, role)
		for _, route := range Routes() {
			if !strings.HasPrefix(route.Path, "/api/terminals") {
				continue
			}
			path := strings.ReplaceAll(route.Path, "{id}", "missing")
			r := httptest.NewRequest(route.Method, "http://"+addr+path, strings.NewReader(`{}`))
			r.Header.Set(tokenHeader, key)
			r.Header.Set("Origin", "http://"+addr)
			w := httptest.NewRecorder()
			s.h.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s %s = %d, want 403", role, route.Method, path, w.Code)
			}
		}
	}
}

func createRemoteTestShare(t *testing.T, s *Server, addr, role string) (string, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://"+addr+"/api/access/shares",
		strings.NewReader(`{"name":"remote guest","role":"`+role+`"}`))
	r.Header.Set(tokenHeader, s.Token())
	r.Header.Set("Origin", "http://"+addr)
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create share: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Share sharedAccess `json:"share"`
		Token string       `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Share.ID, out.Token
}

func TestRemoteTerminalThroughTrustedTunnel(t *testing.T) {
	s := newTerminalServer(t, Options{AllowRemoteTerminal: true,
		AllowedOrigins: []string{"https://dev.tunnel.example"}})
	request := func(method, path, key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://dev.tunnel.example"+path, strings.NewReader(body))
		r.Header.Set(tokenHeader, key)
		r.Header.Set("Origin", "https://dev.tunnel.example")
		r.Header.Set("X-Forwarded-For", "192.168.0.20")
		w := httptest.NewRecorder()
		s.h.ServeHTTP(w, r)
		return w
	}
	w := request(http.MethodPost, "/api/terminals", s.Token(), `{"cols":80,"rows":24}`)
	if w.Code != http.StatusOK {
		t.Fatalf("tunnel open: %d %s", w.Code, w.Body.String())
	}
	var info terminalInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if w := request(http.MethodGet, "/api/terminals", s.Token(), ""); w.Code != http.StatusOK {
		t.Fatalf("tunnel list: %d %s", w.Code, w.Body.String())
	}
	for _, role := range []string{"read", "view", "write"} {
		_, key := createTestShare(t, s, role)
		for _, route := range Routes() {
			if !strings.HasPrefix(route.Path, "/api/terminals") {
				continue
			}
			path := strings.ReplaceAll(route.Path, "{id}", info.ID)
			if w := request(route.Method, path, key, `{}`); w.Code != http.StatusForbidden {
				t.Errorf("%s tunnel %s %s = %d, want 403", role, route.Method, path, w.Code)
			}
		}
	}
	if w := request(http.MethodPost, "/api/terminals/"+info.ID+"/close", s.Token(), ""); w.Code != http.StatusNoContent {
		t.Fatalf("tunnel close: %d %s", w.Code, w.Body.String())
	}
}

// TestTerminalRefusesATrustedRemoteOrigin checks the stricter rule: an origin
// the operator trusted for a tunnel may drive the rest of the console, but not
// a shell.
func TestTerminalRefusesATrustedRemoteOrigin(t *testing.T) {
	s := newTerminalServer(t, Options{AllowedOrigins: []string{"https://dev.tunnel.example"}})

	// The same request is accepted by an ordinary mutation, so the 403 below is
	// the terminal's own rule and not the global guard.
	for _, c := range []struct {
		host, origin, path string
		want               int
	}{
		{"dev.tunnel.example", "https://dev.tunnel.example", "/api/backend/start", http.StatusOK},
		{"dev.tunnel.example", "https://dev.tunnel.example", "/api/terminals", http.StatusForbidden},
		{recorderAddr, "https://dev.tunnel.example", "/api/terminals", http.StatusForbidden},
	} {
		req := httptest.NewRequest(http.MethodPost, "http://"+c.host+c.path, strings.NewReader(`{"cols":80,"rows":24}`))
		req.Host = c.host
		req.Header.Set("Origin", c.origin)
		req.Header.Set(tokenHeader, s.Token())
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("POST %s (Host %s, Origin %s) = %d, want %d: %s",
				c.path, c.host, c.origin, rec.Code, c.want, rec.Body)
		}
	}
	if n := len(s.terminals.List()); n != 0 {
		t.Errorf("%d terminals were opened through a remote origin", n)
	}
}

// TestTerminalRoundTrip opens a shell, types into it, reads the answer off the
// SSE stream of a real listener, and closes it.
func TestTerminalRoundTrip(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	terms := supervisor.NewTerminals(t.TempDir())
	t.Cleanup(terms.CloseAll)
	s, err := NewWith(twoServices(), Options{Addr: "127.0.0.1:0", Terminals: terms})
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
		if err := <-done; err != nil {
			t.Errorf("ListenAndServe shutdown: %v", err)
		}
	})
	base := "http://" + s.Addr()

	post := func(path, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
		req.Header.Set(tokenHeader, s.Token())
		req.Header.Set("Origin", base)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}

	resp := post("/api/terminals", `{"cols":80,"rows":24}`)
	var info terminalInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("open = %d, %v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()

	sreq, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/terminals/"+info.ID+"/stream", nil)
	sreq.Header.Set(tokenHeader, s.Token())
	stream, err := http.DefaultClient.Do(sreq)
	if err != nil || stream.StatusCode != http.StatusOK {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = stream.Body.Close() }()

	if r := post("/api/terminals/"+info.ID+"/input", `{"data":"echo mark-$((40+2))\n"}`); r.StatusCode != http.StatusNoContent {
		t.Fatalf("input = %d", r.StatusCode)
	}

	var got strings.Builder
	found := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stream.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var c terminalChunk
			if json.Unmarshal([]byte(line), &c) != nil {
				continue
			}
			b, _ := base64.StdEncoding.DecodeString(c.Data)
			got.Write(b)
			if strings.Contains(got.String(), "mark-42") {
				close(found)
				return
			}
		}
	}()
	select {
	case <-found:
	case <-time.After(10 * time.Second):
		t.Fatalf("no marker on the stream after 10s")
	}

	if r := post("/api/terminals/"+info.ID+"/close", ``); r.StatusCode != http.StatusNoContent {
		t.Fatalf("close = %d", r.StatusCode)
	}
	if n := len(terms.List()); n != 0 {
		// Close waits for the shell; the map entry follows a moment later.
		deadline := time.Now().Add(5 * time.Second)
		for len(terms.List()) != 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if n := len(terms.List()); n != 0 {
			t.Errorf("%d terminals still open after close", n)
		}
	}
}

// TestTerminalStreamMarksTheScrollbackAsReplay is the server half of
// LANDMINES §21: the scrollback frame says it is a replay, so the page can
// refuse to answer the terminal queries inside it.
func TestTerminalStreamMarksTheScrollbackAsReplay(t *testing.T) {
	s := newTerminalServer(t, Options{})
	info, err := s.terminals.Open(80, 24)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	term, _ := s.terminals.Get(info.ID)
	if err := term.Write([]byte("echo mark-$((40+2))\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		backlog, _, detach := term.Attach()
		detach()
		if strings.Contains(string(backlog), "mark-42") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the shell never echoed the marker")
		}
		time.Sleep(20 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/terminals/"+info.ID+"/stream", nil).WithContext(ctx)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	for _, line := range strings.Split(rec.Body.String(), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var c terminalChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("frame %q: %v", data, err)
		}
		if !c.Replay {
			t.Errorf("the first frame is not marked as a replay: %s", data)
		}
		b, _ := base64.StdEncoding.DecodeString(c.Data)
		if !strings.Contains(string(b), "mark-42") {
			t.Errorf("the replay frame lacks the scrollback: %q", b)
		}
		return
	}
	t.Fatalf("no frame on the stream: %q", rec.Body.String())
}

// TestTerminalRefusesARelayedRequest checks the defence against a tunnel that
// rewrites Host to the loopback address: the headers a relaying proxy adds are
// refused even when Host and Origin look local.
func TestTerminalRefusesARelayedRequest(t *testing.T) {
	s := newTerminalServer(t, Options{})
	for _, h := range forwardedHeaders {
		req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/terminals", nil)
		req.Header.Set(tokenHeader, s.Token())
		req.Header.Set(h, "203.0.113.7")
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET /api/terminals with %s = %d, want 403", h, rec.Code)
		}
	}
}

// TestTerminalOpenPastTheCapIs429 checks the cap surfaces as a retryable
// refusal rather than a server error.
func TestTerminalOpenPastTheCapIs429(t *testing.T) {
	s := newTerminalServer(t, Options{})
	for i := 0; i < supervisor.MaxTerminals; i++ {
		if _, err := s.terminals.Open(80, 24); err != nil {
			t.Fatalf("Open #%d: %v", i+1, err)
		}
	}
	if rec := terminalRequest(s, http.MethodPost, "/api/terminals", `{"cols":80,"rows":24}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("POST /api/terminals past the cap = %d, want 429: %s", rec.Code, rec.Body)
	}
}
