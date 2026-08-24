package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// serveApp returns an app wired to h, bootstrapped exactly as run would
// bootstrap it, so a test can call the serve body without going through cobra
// and therefore without needing a signal to end it.
func serveApp(t *testing.T, h *harness) *app {
	t.Helper()
	normalize(h.env)
	a := newApp(h.env)
	a.bootstrap()
	return a
}

// TestServeRefusesNonLoopbackAddress locks in the control the whole command
// hangs on: an address other machines can reach exposes routes that start and
// stop the commands in mabo-ctl.yaml, so it is refused, it is a usage error
// rather than a runtime one, and nothing is bound or printed.
func TestServeRefusesNonLoopbackAddress(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "serve", "--addr", "0.0.0.0:7999")

	if code := h.run(); code != exitUsage {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, h.stderr)
	}
	msg := h.stderr.String()
	for _, want := range []string{"0.0.0.0:7999", "--i-know-this-is-dangerous"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, msg)
		}
	}
	if got := h.stdout.String(); got != "" {
		t.Errorf("stdout = %q, want nothing printed when nothing was bound", got)
	}
}

// TestServeRejectsMalformedAddress checks that a --addr that is not host:port
// is a usage error naming the flag, not a listener error later on.
func TestServeRejectsMalformedAddress(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "serve", "--addr", "7999")

	if code := h.run(); code != exitUsage {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, h.stderr)
	}
	if msg := h.stderr.String(); !strings.Contains(msg, "--addr") {
		t.Errorf("stderr does not name the flag:\n%s", msg)
	}
}

// TestServeHelpDocumentsTheRisk checks that the danger flag's help says what it
// exposes rather than only that it exists.
func TestServeHelpDocumentsTheRisk(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "serve", "--help")

	if code := h.run(); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, h.stderr)
	}
	out := h.stdout.String()
	for _, want := range []string{"--i-know-this-is-dangerous", "token", "127.0.0.1"} {
		if !strings.Contains(out, want) {
			t.Errorf("serve --help does not mention %q:\n%s", want, out)
		}
	}
}

// TestServeBindsBeforePrintingTheURL is the end-to-end shape of the command: a
// port of 0 can only resolve once the socket exists, so a URL printed with the
// real port proves the bind happened first — and the printed URL answers.
func TestServeBindsBeforePrintingTheURL(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := serveApp(t, h)

	// OpenURL runs after the bind and before ListenAndServe, which makes it the
	// test's synchronisation point: it hands over the URL without either side
	// touching the output buffers while the other is writing to them.
	opened := make(chan string, 1)
	h.env.OpenURL = func(_ context.Context, raw string) error {
		opened <- raw
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.serve(ctx, serveOptions{Addr: "127.0.0.1:0", Open: true}) }()

	var raw string
	select {
	case raw = <-opened:
	case err := <-done:
		t.Fatalf("serve returned before it opened anything: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve never bound a socket")
	}

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("printed URL %q does not parse: %v", raw, err)
	}
	if token := u.Query().Get("token"); len(token) != 64 {
		t.Errorf("printed URL carries token %q, want the 32-byte session token hex encoded", token)
	}
	if _, port, _ := splitURLPort(u); port == "0" || port == "" {
		t.Errorf("printed URL %q carries port %q, so it was printed before the bind resolved it", raw, port)
	}

	// The URL is real: it answers, and a mutation through it without the token
	// header is still refused.
	resp, err := http.Get(raw) //nolint:noctx // the deadline is the test's own.
	if err != nil {
		t.Fatalf("GET %s: %v", raw, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET %s = %d, want 200", raw, resp.StatusCode)
	}

	post, err := http.Post("http://"+u.Host+"/api/alpha/stop", "", nil) //nolint:noctx // same.
	if err != nil {
		t.Fatalf("POST /api/alpha/stop: %v", err)
	}
	_, _ = io.Copy(io.Discard, post.Body)
	post.Body.Close()
	if post.StatusCode != http.StatusForbidden {
		t.Errorf("POST without the token = %d, want 403", post.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve = %v, want nil after an interrupt", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after its context was cancelled")
	}

	// Safe to read the buffers now: the serving goroutine has finished.
	lines := strings.Split(strings.TrimSpace(h.stdout.String()), "\n")
	if len(lines) != 1 || lines[0] != raw {
		t.Errorf("stdout = %q, want exactly the URL %q", h.stdout.String(), raw)
	}
	if msg := h.stderr.String(); !strings.Contains(msg, "password") {
		t.Errorf("stderr does not say the token is a credential:\n%s", msg)
	}
}

// splitURLPort returns u's host and port.
func splitURLPort(u *url.URL) (host, port string, ok bool) {
	h, p := u.Hostname(), u.Port()
	return h, p, p != ""
}

// TestExposedToNetwork covers the predicate that decides whether the loud
// warning is printed. It fails towards warning: an address it cannot read is
// treated as exposed.
func TestExposedToNetwork(t *testing.T) {
	t.Parallel()
	tests := []struct {
		addr string
		want bool
	}{
		{addr: "127.0.0.1:7999", want: false},
		{addr: "127.0.0.53:7999", want: false},
		{addr: "[::1]:7999", want: false},
		{addr: "0.0.0.0:7999", want: true},
		{addr: "[::]:7999", want: true},
		{addr: "192.168.1.10:7999", want: true},
		{addr: ":7999", want: true},
		{addr: "not-an-address", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			t.Parallel()
			if got := exposedToNetwork(tc.addr); got != tc.want {
				t.Errorf("exposedToNetwork(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}
