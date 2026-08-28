package health

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recorder collects what the test server actually observed, so assertions can
// be made on the wire behaviour rather than on the probe's own report.
type recorder struct {
	mu      sync.Mutex
	methods []string
	hosts   []string
}

func (r *recorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.methods = append(r.methods, req.Method)
	r.hosts = append(r.hosts, req.Host)
}

func (r *recorder) seenMethods() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.methods...)
}

func (r *recorder) seenHosts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hosts...)
}

// newServer starts an httptest server that is closed when the test ends.
func newServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

// testCtx bounds every test so a hung probe fails loudly instead of hanging
// the suite.
func testCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestMaxBodyBytesIsFourKiB(t *testing.T) {
	if MaxBodyBytes != 4096 {
		t.Fatalf("MaxBodyBytes = %d, want 4096", MaxBodyBytes)
	}
}

// A server that answers HEAD must never see a GET: HEAD is the cheap request
// and the fallback is only for servers that refuse it.
func TestProbeUsesHeadAndNeverFallsBack(t *testing.T) {
	rec := &recorder{}
	ts := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("User-agent: *\n"))
	})

	res := Probe(testCtx(t, 5*time.Second), ts.URL+"/robots.txt")
	if !res.OK || res.Err != nil {
		t.Fatalf("Probe = %+v, want OK with no error", res)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("Status = %d, want 200", res.Status)
	}
	if got := rec.seenMethods(); len(got) != 1 || got[0] != http.MethodHead {
		t.Fatalf("server saw %v, want exactly one HEAD", got)
	}
	if res.Elapsed <= 0 {
		t.Fatalf("Elapsed = %v, want a positive duration", res.Elapsed)
	}
}

// 405 and 501 are the two statuses that mean "I speak HTTP but not HEAD".
func TestProbeFallsBackToGET(t *testing.T) {
	for _, tc := range []struct {
		name      string
		headReply int
	}{
		{"method not allowed", http.StatusMethodNotAllowed},
		{"not implemented", http.StatusNotImplemented},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			ts := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				rec.record(r)
				if r.Method == http.MethodHead {
					w.WriteHeader(tc.headReply)
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			})

			res := Probe(testCtx(t, 5*time.Second), ts.URL+"/health")
			if !res.OK || res.Err != nil {
				t.Fatalf("Probe = %+v, want OK with no error", res)
			}
			if res.Status != http.StatusOK {
				t.Fatalf("Status = %d, want the GET's 200", res.Status)
			}
			want := []string{http.MethodHead, http.MethodGet}
			if got := rec.seenMethods(); !equalStrings(got, want) {
				t.Fatalf("server saw %v, want %v", got, want)
			}
		})
	}
}

// A method the server rejects with 405 for reasons of its own must not be
// retried forever: exactly one fallback, then done.
func TestProbeFallbackIsRecordedEvenWhenGETAlsoFails(t *testing.T) {
	rec := &recorder{}
	ts := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusMethodNotAllowed)
	})

	res := Probe(testCtx(t, 5*time.Second), ts.URL+"/")
	if !res.OK {
		t.Fatalf("Probe = %+v, want OK: a 405 is still an answering server", res)
	}
	if res.Status != http.StatusMethodNotAllowed {
		t.Fatalf("Status = %d, want 405", res.Status)
	}
	if got := rec.seenMethods(); len(got) != 2 {
		t.Fatalf("server saw %v, want exactly HEAD then GET", got)
	}
}

// dev.sh bug #3: the predecessor drained the body and a 2 ms server looked
// "slow" forever. The probe must return on headers and must not read the body.
func TestProbeDoesNotReadTheBody(t *testing.T) {
	const (
		chunkSize = 64 << 10
		bodySize  = 10 << 20
	)

	var sent atomic.Int64
	observed := make(chan string, 1)
	var once sync.Once
	notify := func(reason string) {
		once.Do(func() { observed <- reason })
	}

	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			// Force the GET fallback so there is a body to (not) read.
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		chunk := bytes.Repeat([]byte("x"), chunkSize)
		for sent.Load() < bodySize {
			n, err := w.Write(chunk)
			sent.Add(int64(n))
			if err != nil {
				notify("write failed: " + err.Error())
				return
			}
			if r.Context().Err() != nil {
				notify("request context cancelled: " + r.Context().Err().Error())
				return
			}
		}
		notify("wrote the whole body")
	}))
	// Safety net: without it, a handler blocked in Write on a peer that never
	// reads would wedge the test instead of failing it.
	ts.Config.WriteTimeout = 5 * time.Second
	ts.Start()
	t.Cleanup(ts.Close)

	res := Probe(testCtx(t, 30*time.Second), ts.URL+"/")
	if !res.OK {
		t.Fatalf("Probe = %+v, want OK", res)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("Status = %d, want 200", res.Status)
	}
	if res.Elapsed > time.Second {
		t.Fatalf("Elapsed = %v, want well under a second: the probe drained the body", res.Elapsed)
	}

	select {
	case reason := <-observed:
		if reason == "wrote the whole body" {
			t.Fatalf("server delivered all %d bytes: the probe read the body", bodySize)
		}
		t.Logf("server observed the early close: %s", reason)
	case <-time.After(15 * time.Second):
		t.Fatal("server never observed the probe closing the connection")
	}

	if got := sent.Load(); got >= bodySize {
		t.Fatalf("server wrote %d bytes, want far fewer than the %d byte body", got, bodySize)
	}
}

// "Is the server answering?" — not "is the route happy?".
func TestProbeTreatsAnyStatusAsReady(t *testing.T) {
	for _, status := range []int{
		http.StatusOK,
		http.StatusNoContent,
		http.StatusUnauthorized,
		http.StatusNotFound,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ts := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			})
			res := Probe(testCtx(t, 5*time.Second), ts.URL+"/health")
			if !res.OK || res.Err != nil {
				t.Fatalf("Probe = %+v, want OK with no error for status %d", res, status)
			}
			if res.Status != status {
				t.Fatalf("Status = %d, want %d", res.Status, status)
			}
		})
	}
}

// A 3xx proves the server answered; chasing it would probe a different target
// and read another body.
func TestProbeDoesNotFollowRedirects(t *testing.T) {
	var target atomic.Int64
	ts := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/elsewhere") {
			target.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})

	res := Probe(testCtx(t, 5*time.Second), ts.URL+"/health")
	if !res.OK || res.Status != http.StatusFound {
		t.Fatalf("Probe = %+v, want OK with status 302", res)
	}
	if n := target.Load(); n != 0 {
		t.Fatalf("redirect target was fetched %d times, want 0", n)
	}
}

func TestProbeConnectionRefused(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := ts.URL + "/health"
	ts.Close() // the port is now free; nothing is listening

	res := Probe(testCtx(t, 5*time.Second), url)
	if res.OK {
		t.Fatalf("Probe = %+v, want not OK against a closed port", res)
	}
	if res.Err == nil {
		t.Fatal("Err = nil, want a dial error")
	}
	if res.Status != 0 {
		t.Fatalf("Status = %d, want 0", res.Status)
	}
	if !strings.Contains(res.Err.Error(), url) {
		t.Fatalf("Err = %v, want it to name the URL %q", res.Err, url)
	}
}

func TestProbeContextDeadline(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	ts := newServer(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := Probe(ctx, ts.URL+"/health")
	elapsed := time.Since(start)

	if res.OK {
		t.Fatalf("Probe = %+v, want not OK", res)
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("Err = %v, want it to wrap context.DeadlineExceeded", res.Err)
	}
	if elapsed > ProbeTimeout {
		t.Fatalf("Probe took %v, want it to honour the caller's shorter deadline", elapsed)
	}
}

func TestProbeAlreadyCancelledContext(t *testing.T) {
	ts := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := Probe(ctx, ts.URL+"/health")
	if res.OK {
		t.Fatalf("Probe = %+v, want not OK", res)
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("Err = %v, want it to wrap context.Canceled", res.Err)
	}
}

func TestProbeRejectsBadURLs(t *testing.T) {
	for _, tc := range []struct{ name, url string }{
		{"empty", ""},
		{"blank", "   "},
		{"no scheme", "localhost:7100/health"},
		{"wrong scheme", "ftp://localhost:7100/health"},
		{"no host", "http:///health"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := Probe(testCtx(t, time.Second), tc.url)
			if res.OK {
				t.Fatalf("Probe(%q) = %+v, want not OK", tc.url, res)
			}
			if !errors.Is(res.Err, ErrBadURL) {
				t.Fatalf("Err = %v, want it to wrap ErrBadURL", res.Err)
			}
		})
	}
}

func TestWaitRejectsBadURL(t *testing.T) {
	res := Wait(testCtx(t, time.Second), "", func() bool { return true })
	if res.OK || !errors.Is(res.Err, ErrBadURL) {
		t.Fatalf("Wait = %+v, want an ErrBadURL failure", res)
	}
}

// Probes must not reuse a connection between polls: a connection the server
// half-closed while the service was starting would otherwise look like a
// failure on the next attempt.
func TestProbeDoesNotReuseConnections(t *testing.T) {
	var opened atomic.Int64
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	ts.Start()
	t.Cleanup(ts.Close)

	ctx := testCtx(t, 10*time.Second)
	for i := range 3 {
		if res := Probe(ctx, ts.URL+"/health"); !res.OK {
			t.Fatalf("probe %d = %+v, want OK", i, res)
		}
	}
	if got := opened.Load(); got != 3 {
		t.Fatalf("server accepted %d connections for 3 probes, want 3 (no keep-alive reuse)", got)
	}
}

// dev.sh bug #4: a dev server bound localhost to ::1 only, so probing
// 127.0.0.1 failed while localhost succeeded. Probing by hostname over "tcp"
// must reach an IPv6-only listener.
func TestProbeReachesIPv6OnlyListener(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback on this machine: %v", err)
	}
	if !localhostResolvesTo(t, func(ip net.IP) bool { return ip.To4() == nil }) {
		_ = ln.Close()
		t.Skip("localhost does not resolve to ::1 on this machine")
	}
	assertProbeByHostname(t, ln)
}

// The mirror image of the IPv6 case: forcing tcp6 would be just as wrong as
// forcing tcp4.
func TestProbeReachesIPv4OnlyListener(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no IPv4 loopback on this machine: %v", err)
	}
	if !localhostResolvesTo(t, func(ip net.IP) bool { return ip.To4() != nil }) {
		_ = ln.Close()
		t.Skip("localhost does not resolve to 127.0.0.1 on this machine")
	}
	assertProbeByHostname(t, ln)
}

// assertProbeByHostname serves ln and probes it through the name "localhost",
// asserting both that the probe connects and that the Host header was never
// rewritten to a literal address.
func assertProbeByHostname(t *testing.T, ln net.Listener) {
	t.Helper()

	rec := &recorder{}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	_ = ts.Listener.Close()
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", ln.Addr(), err)
	}
	url := "http://localhost:" + port + "/health"

	res := Probe(testCtx(t, 10*time.Second), url)
	if !res.OK {
		t.Fatalf("Probe(%s) = %+v, want OK against %s", url, res, ln.Addr())
	}
	wantHost := "localhost:" + port
	if got := rec.seenHosts(); len(got) != 1 || got[0] != wantHost {
		t.Fatalf("server saw Host %v, want [%s]: the probe rewrote the hostname", got, wantHost)
	}
}

// localhostResolvesTo reports whether any address behind "localhost" satisfies
// want, so an address-family test can skip on a machine that cannot express it.
func localhostResolvesTo(t *testing.T, want func(net.IP) bool) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, "localhost")
	if err != nil {
		t.Logf("resolving localhost: %v", err)
		return false
	}
	for _, a := range addrs {
		if want(a.IP) {
			return true
		}
	}
	return false
}

func TestWaitPollsUntilTheServerAnswers(t *testing.T) {
	var attempts atomic.Int32
	ts := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= 2 {
			// Not listening yet, as far as the client can tell: drop the
			// connection without a response.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("test server does not support hijacking")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	start := time.Now()
	res := Wait(testCtx(t, 20*time.Second), ts.URL+"/health", func() bool { return true })
	elapsed := time.Since(start)

	if !res.OK || res.Err != nil {
		t.Fatalf("Wait = %+v, want OK with no error", res)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("Status = %d, want 200", res.Status)
	}
	if n := attempts.Load(); n < 3 {
		t.Fatalf("server saw %d attempts, want at least 3", n)
	}
	if elapsed < PollInterval {
		t.Fatalf("Wait returned in %v without honouring the %v poll interval", elapsed, PollInterval)
	}
	if res.Elapsed < PollInterval {
		t.Fatalf("Elapsed = %v, want the total wait, not one request", res.Elapsed)
	}
}

// A dead process must produce a failure immediately, not sit until the probe
// or readiness timeout expires.
func TestWaitReturnsAsSoonAsTheProcessDies(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	ts := newServer(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	})

	var live atomic.Bool
	live.Store(true)
	timer := time.AfterFunc(200*time.Millisecond, func() { live.Store(false) })
	t.Cleanup(func() { timer.Stop() })

	start := time.Now()
	res := Wait(testCtx(t, 30*time.Second), ts.URL+"/health", live.Load)
	elapsed := time.Since(start)

	if res.OK {
		t.Fatalf("Wait = %+v, want not OK", res)
	}
	if !errors.Is(res.Err, ErrProcessGone) {
		t.Fatalf("Err = %v, want it to wrap ErrProcessGone", res.Err)
	}
	// The request is still hanging: anything near ProbeTimeout means Wait sat
	// through the in-flight probe instead of abandoning it.
	if elapsed > ProbeTimeout {
		t.Fatalf("Wait took %v, want it to return promptly after alive() went false", elapsed)
	}
}

func TestWaitReturnsImmediatelyWhenAlreadyDead(t *testing.T) {
	var requests atomic.Int64
	ts := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	start := time.Now()
	res := Wait(testCtx(t, 5*time.Second), ts.URL+"/health", func() bool { return false })
	elapsed := time.Since(start)

	if res.OK || !errors.Is(res.Err, ErrProcessGone) {
		t.Fatalf("Wait = %+v, want an ErrProcessGone failure", res)
	}
	if elapsed > time.Second {
		t.Fatalf("Wait took %v, want an immediate return", elapsed)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("server saw %d requests, want 0: a dead process must not be probed", n)
	}
}

func TestWaitHonoursContextDeadline(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	ts := newServer(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := Wait(ctx, ts.URL+"/health", func() bool { return true })
	elapsed := time.Since(start)

	if res.OK {
		t.Fatalf("Wait = %+v, want not OK", res)
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("Err = %v, want it to wrap context.DeadlineExceeded", res.Err)
	}
	if errors.Is(res.Err, ErrProcessGone) {
		t.Fatalf("Err = %v, want a timeout, not a dead process", res.Err)
	}
	if elapsed < 300*time.Millisecond {
		t.Fatalf("Wait returned after %v, before its deadline", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Wait took %v, want it to stop at the deadline", elapsed)
	}
}

// A nil alive callback means "liveness is not tracked" and must not panic.
func TestWaitAcceptsNilAliveFunc(t *testing.T) {
	ts := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	res := Wait(testCtx(t, 5*time.Second), ts.URL+"/health", nil)
	if !res.OK || res.Status != http.StatusServiceUnavailable {
		t.Fatalf("Wait = %+v, want OK with status 503", res)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// tcp and exec probes

// freePort asks the kernel for a listening TCP port, for tests that must own
// the exact socket they probe.
func freePort(t *testing.T) (string, net.Listener) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().String(), l
}

func TestProbeTCPConnectsAndCloses(t *testing.T) {
	addr, l := freePort(t)

	res := ProbeTCP(context.Background(), addr)
	if !res.OK {
		t.Fatalf("OK = false, err = %v; want a successful dial against a live listener", res.Err)
	}
	if res.Status != 0 {
		t.Errorf("Status = %d, want 0: a dial has no HTTP status", res.Status)
	}

	// A refused port is the negative case.
	_ = l.Close() //nolint:errcheck — nothing depends on this listener surviving
	if res2 := ProbeTCP(context.Background(), "127.0.0.1:1"); res2.OK {
		t.Error("dialing a refused port reported OK")
	}
}

func TestProbeTCPEmptyAddr(t *testing.T) {
	if res := ProbeTCP(context.Background(), ""); res.OK || res.Err == nil {
		t.Errorf("empty address: OK = %v, err = %v; want a rejection", res.OK, res.Err)
	}
}

func TestWaitTCPReadyThenReportsAliveDeath(t *testing.T) {
	addr, _ := freePort(t)
	done := make(chan Result, 1)
	go func() { done <- WaitTCP(context.Background(), "tcp:"+addr, addr, nil) }()

	select {
	case res := <-done:
		if !res.OK {
			t.Fatalf("OK = false, err = %v", res.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitTCP never returned against an open port")
	}
}

func TestExecProbeExitCodes(t *testing.T) {
	dir := t.TempDir()

	ok := ProbeExec(context.Background(), dir, nil, []string{"true"})
	if !ok.OK {
		t.Errorf("`true`: OK = false, err = %v", ok.Err)
	}

	fail := ProbeExec(context.Background(), dir, nil, []string{"false"})
	if fail.OK {
		t.Error("`false` reported OK")
	}
	if fail.Err == nil || !strings.Contains(fail.Err.Error(), "exit status 1") {
		t.Errorf("`false` err = %v, want exit status 1", fail.Err)
	}

	missing := ProbeExec(context.Background(), dir, nil, []string{"mabo-ctl-no-such-binary-xyz"})
	if missing.OK || missing.Err == nil {
		t.Error("a binary that cannot start must report not-ready with an error")
	}
}

func TestExecProbeCapturesTruncatedOutputOnFailure(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fail.sh")
	body := "#!/bin/sh\necho before-failure-marker\nexit 3\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	// A bounded retry, because the first assertion races the machine, not the
	// code: under a full-suite -race run with many packages forking at once, a
	// freshly spawned /bin/sh can miss the probe's hard 2s budget entirely —
	// killed at the deadline before `echo` ever ran, an honest timeout with
	// empty output. On any machine healthy enough to trust the suite, at least
	// one of three attempts gets scheduled promptly.
	var res Result
	for attempt := 1; attempt <= 3; attempt++ {
		res = ProbeExec(context.Background(), dir, nil, []string{script})
		if !res.OK && res.Err != nil && strings.Contains(res.Err.Error(), "before-failure-marker") {
			return // the assertion below, already proven
		}
		t.Logf("attempt %d: res = %v (fork starvation under load?)", attempt, res.Err)
	}
	if res.OK {
		t.Fatal("an exiting-3 script reported OK")
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "before-failure-marker") {
		t.Errorf("err = %v, want the captured diagnostic line", res.Err)
	}
}

func TestExecProbeHardTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("sleeps for the full probe timeout")
	}
	dir := t.TempDir()
	start := time.Now()
	res := ProbeExec(context.Background(), dir, nil, []string{"sleep", "60"})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("exec probe took %s; the hard timeout did not hold", elapsed)
	}
	if res.OK {
		t.Error("a sleeping probe reported OK")
	}
}

func TestExecProbeRejectsEmptyArgv(t *testing.T) {
	if res := ProbeExec(context.Background(), t.TempDir(), nil, nil); res.OK || res.Err == nil {
		t.Errorf("empty argv: OK = %v, err = %v; want a rejection", res.OK, res.Err)
	}
}

func TestWaitExecRunsInTheGivenDirAndEnv(t *testing.T) {
	dir := t.TempDir()
	probeFile := filepath.Join(dir, "seen-by-probe")
	script := filepath.Join(dir, "touch.sh")
	body := "#!/bin/sh\n[ -n \"$MABO_PROBE_VAR\" ] && touch seen-by-probe\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	env := []string{"PATH=" + os.Getenv("PATH"), "MABO_PROBE_VAR=1"}
	res := WaitExec(context.Background(), "exec: "+script, dir, env, []string{script}, nil)
	if !res.OK {
		t.Fatalf("OK = false, err = %v — the probe must run in dir with env", res.Err)
	}
	if _, err := os.Stat(probeFile); err != nil {
		t.Errorf("the probe's side effect is missing: %v — wrong dir or env", err)
	}
}
