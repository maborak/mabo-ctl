// Package health implements mabo-ctl's readiness probes: HTTP, TCP dial and
// exec, selected by how the service declares its health check.
//
// The package is small on purpose, but every rule in it is a fix for a
// diagnosed failure of the shell predecessor. None of them is incidental and
// none of them should be "simplified":
//
//  1. HEAD is sent first; GET is used only as a fallback for servers that
//     refuse HEAD with 405 or 501. HEAD costs the supervised service nothing.
//  2. The response body is never read. Receiving the response headers is the
//     definition of ready. A full-body GET once timed out "with 71602 bytes
//     received" against a dev server that had answered in 2 ms, and the
//     supervisor called a perfectly healthy service "slow" forever.
//  3. Any HTTP response means ready, including 4xx and 5xx. The question this
//     package answers is "is the server answering?", not "is the route happy?".
//     The status code is recorded in [Result.Status] so the UI can show it.
//  4. The address family is never forced and the host is never rewritten. A
//     dev server that binds localhost to ::1 only is unreachable on
//     127.0.0.1; see [newClient] for why this must stay as it is.
//  5. A TCP probe is a DIAL, nothing more: connected is ready, the socket is
//     closed immediately, and nothing is written to it. An exec probe runs an
//     argv once under a hard timeout and reads its exit code; its output is
//     captured only as a truncated diagnostic for a failure.
//
// The package depends only on the standard library and performs no filesystem
// side effects of its own; an exec probe runs exactly the command it was given,
// in the working directory and environment its caller chose.
package health

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// ProbeTimeout is the wall-clock budget for a single [Probe] call,
	// including the optional GET fallback. A shorter deadline on the caller's
	// context wins.
	ProbeTimeout = 2 * time.Second

	// PollInterval is the delay [Wait] leaves between two readiness attempts.
	PollInterval = 250 * time.Millisecond

	// MaxBodyBytes is the hard cap on any read of a probe response body.
	//
	// The current implementation reads exactly zero bytes: the body is closed
	// the instant the headers arrive, because draining it is what made a
	// healthy service report "slow" forever in the predecessor. The constant
	// exists so that if a future change ever genuinely needs to inspect a
	// response, it does so through io.LimitReader(resp.Body, MaxBodyBytes) and
	// never with an unbounded read.
	MaxBodyBytes = 4 << 10

	// aliveInterval is how often [Wait] re-checks the supervised process while
	// a probe is in flight. It is much shorter than ProbeTimeout so that a
	// process that dies during a hanging request is reported immediately
	// instead of after the probe deadline.
	aliveInterval = 50 * time.Millisecond

	// userAgent identifies mabo-ctl in the supervised service's access log.
	userAgent = "mabo-ctl-health/1"
)

// ErrProcessGone reports that the supervised process stopped running before
// its health URL ever answered. [Wait] wraps it, so callers can distinguish a
// dead process (PhaseFailed) from a slow one (PhaseSlow) with errors.Is.
var ErrProcessGone = errors.New("supervised process is no longer running")

// ErrBadURL reports that a health URL is not an absolute http or https URL
// with a host. It is returned before any network activity happens.
var ErrBadURL = errors.New("invalid health URL")

// Result is the outcome of a readiness check.
type Result struct {
	// OK is true when the server produced an HTTP response of any status,
	// including 4xx and 5xx. It is false only when nothing answered.
	OK bool

	// Status is the HTTP status code of the response, or 0 when there was
	// none. For a HEAD that fell back to GET this is the GET's status.
	Status int

	// Elapsed is how long the check took: a single request for [Probe], the
	// whole polling loop for [Wait].
	Elapsed time.Duration

	// Err explains the failure when OK is false. When OK is true it is
	// normally nil, but it may still carry a non-fatal problem such as a
	// failure to close the connection; a server that answered is ready
	// regardless.
	Err error
}

// Probe issues one readiness check against rawURL and reports whether the
// server answered.
//
// It sends HEAD first and retries once with GET when the server rejects HEAD
// with 405 Method Not Allowed or 501 Not Implemented. The response body is
// never read: the body is closed as soon as the headers arrive, so a dev
// server streaming a multi-megabyte page still reports ready in milliseconds.
// Redirects are not followed, because a 3xx already proves the server is
// answering and following it would probe a different host.
//
// Any HTTP status counts as ready. Err is non-nil only when the connection,
// the TLS handshake or the request itself failed, or when rawURL is not an
// absolute http(s) URL, in which case Err wraps [ErrBadURL] and no network
// call is made.
//
// Probe blocks for at most [ProbeTimeout], or less if ctx expires sooner. It
// starts no goroutines of its own.
func Probe(ctx context.Context, rawURL string) Result {
	start := time.Now()
	if err := checkURL(rawURL); err != nil {
		return Result{Elapsed: time.Since(start), Err: err}
	}

	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	client := newClient()
	defer client.CloseIdleConnections()

	res := attempt(ctx, client, http.MethodHead, rawURL, start)
	if res.OK && (res.Status == http.StatusMethodNotAllowed || res.Status == http.StatusNotImplemented) {
		// The server understands HTTP but not HEAD. GET is the only way to
		// learn anything more, and it is still safe because the body is never
		// read.
		res = attempt(ctx, client, http.MethodGet, rawURL, start)
	}
	return res
}

// Wait polls rawURL until the server answers, the supervised process dies, or
// ctx expires, whichever happens first.
//
// alive reports whether the supervised process still exists; a nil alive means
// liveness is not tracked and is treated as "always alive". Wait re-checks
// alive every aliveInterval, including while a request is in flight, so a
// process that dies mid-probe is reported within tens of milliseconds instead
// of after [ProbeTimeout] — a dead process must fail fast, not sit until the
// readiness timeout.
//
// Wait returns Result.OK true on the first HTTP response of any status.
// Otherwise Err wraps [ErrProcessGone] when alive turned false, ctx.Err() when
// the deadline expired, or [ErrBadURL] when rawURL is not usable. Result.Elapsed
// is the total time spent waiting.
//
// Wait blocks. It runs each probe in its own goroutine so an in-flight request
// can be abandoned; every such goroutine is bounded by [ProbeTimeout] and is
// cancelled when Wait returns.
func Wait(ctx context.Context, rawURL string, alive func() bool) Result {
	// Rejected before any polling starts, exactly as before: a URL that cannot
	// ever work must fail in microseconds, not after a readiness timeout.
	if err := checkURL(rawURL); err != nil {
		return Result{Err: err}
	}
	return wait(ctx, rawURL, func(c context.Context) Result { return Probe(c, rawURL) }, alive)
}

// wait is the polling loop every probe family shares: run one attempt, stop on
// success, a dead process or an expired context, otherwise sleep and repeat.
// describe names the target in error messages exactly as the caller declared
// it, so the failure text quotes the config rather than a derived form of it.
func wait(ctx context.Context, describe string, probe func(context.Context) Result, alive func() bool) Result {
	start := time.Now()

	var last Result
	for {
		if !isAlive(alive) {
			return deadResult(start, last, describe)
		}
		if ctx.Err() != nil {
			return expiredResult(ctx, start, last, describe)
		}

		res, stop := runProbe(ctx, probe, alive)
		switch stop {
		case stopDead:
			return deadResult(start, last, describe)
		case stopCtx:
			return expiredResult(ctx, start, last, describe)
		}
		if res.OK {
			res.Elapsed = time.Since(start)
			return res
		}
		last = res

		switch waitFor(ctx, PollInterval, alive) {
		case stopDead:
			return deadResult(start, last, describe)
		case stopCtx:
			return expiredResult(ctx, start, last, describe)
		}
	}
}

// attempt performs one HTTP request and turns it into a Result. start is the
// beginning of the enclosing probe, so Elapsed covers the HEAD as well as any
// GET fallback.
func attempt(ctx context.Context, client *http.Client, method, rawURL string, start time.Time) Result {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return Result{Elapsed: time.Since(start), Err: fmt.Errorf("health: build %s %s: %w", method, rawURL, err)}
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		return Result{Elapsed: time.Since(start), Err: fmt.Errorf("health: %s %s: %w", method, rawURL, err)}
	}

	// Response headers received == ready. Close the body without reading a
	// single byte of it. Reading — even partially, even with a cap — is what
	// made a service that answered in 2 ms look "slow" until the probe
	// deadline. Do not add a drain here for connection reuse either: probes
	// deliberately never reuse a connection.
	res := Result{OK: true, Status: resp.StatusCode, Elapsed: time.Since(start)}
	if err := resp.Body.Close(); err != nil {
		res.Err = fmt.Errorf("health: close %s %s response: %w", method, rawURL, err)
	}
	return res
}

// newClient builds a single-use client for exactly one probe.
//
// A fresh transport per probe is deliberate: nothing is reused between probes,
// so a connection the server half-closed between polls can never make the next
// probe look like a failure.
//
// The dialer is a plain net.Dialer on network "tcp" and the URL's host is
// passed through untouched. This must not be "optimised" into a tcp4 dial or a
// localhost-to-127.0.0.1 rewrite: a dev server that binds localhost resolves to
// ::1 only on many machines, so probing 127.0.0.1 failed while localhost
// succeeded. Dialing "tcp" by hostname lets Go's Happy Eyeballs try every
// resolved address, IPv6 and IPv4, and succeed on whichever answers.
//
// Proxy is nil rather than http.ProxyFromEnvironment: a readiness probe must
// test the service, not the caller's HTTP_PROXY. Redirects are not followed
// because a 3xx is already proof that the server answered.
func newClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   ProbeTimeout,
		KeepAlive: -1, // single-use connection; TCP keepalives are pointless
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			DisableKeepAlives:     true,
			DisableCompression:    true,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   ProbeTimeout,
			ResponseHeaderTimeout: ProbeTimeout,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// stopReason says why a helper stopped short of its normal completion.
type stopReason int

const (
	stopNone stopReason = iota // ran to completion
	stopDead                   // alive() reported false
	stopCtx                    // ctx expired or was cancelled
)

// runProbe runs one attempt in its own goroutine so that a dead process or an
// expired context can abandon an in-flight request instead of waiting out
// [ProbeTimeout]. The goroutine writes to a buffered channel and therefore
// never leaks, even when its result is abandoned.
func runProbe(ctx context.Context, probe func(context.Context) Result, alive func() bool) (Result, stopReason) {
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan Result, 1)
	go func() { done <- probe(probeCtx) }()

	ticker := time.NewTicker(aliveInterval)
	defer ticker.Stop()

	for {
		select {
		case res := <-done:
			return res, stopNone
		case <-ctx.Done():
			return Result{}, stopCtx
		case <-ticker.C:
			if !isAlive(alive) {
				return Result{}, stopDead
			}
		}
	}
}

// waitFor sleeps for d, returning early if ctx expires or alive turns false.
func waitFor(ctx context.Context, d time.Duration, alive func() bool) stopReason {
	timer := time.NewTimer(d)
	defer timer.Stop()
	ticker := time.NewTicker(aliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-timer.C:
			return stopNone
		case <-ctx.Done():
			return stopCtx
		case <-ticker.C:
			if !isAlive(alive) {
				return stopDead
			}
		}
	}
}

// isAlive reports whether the supervised process is still running. A nil
// callback means liveness is not tracked, which counts as alive.
func isAlive(alive func() bool) bool {
	return alive == nil || alive()
}

// deadResult builds the Result for a process that died before answering,
// keeping the last probe's error so the caller can say what the last attempt
// actually saw.
func deadResult(start time.Time, last Result, describe string) Result {
	res := Result{Status: last.Status, Elapsed: time.Since(start)}
	if last.Err != nil {
		res.Err = fmt.Errorf("health: %s never answered (last probe: %w): %w", describe, last.Err, ErrProcessGone)
	} else {
		res.Err = fmt.Errorf("health: %s never answered: %w", describe, ErrProcessGone)
	}
	return res
}

// expiredResult builds the Result for a readiness timeout, preserving both the
// last probe's error and ctx.Err() so errors.Is finds either.
func expiredResult(ctx context.Context, start time.Time, last Result, describe string) Result {
	elapsed := time.Since(start)
	res := Result{Status: last.Status, Elapsed: elapsed}
	if last.Err != nil {
		res.Err = fmt.Errorf("health: %s not ready after %s (last probe: %w): %w", describe, elapsed.Round(time.Millisecond), last.Err, ctx.Err())
	} else {
		res.Err = fmt.Errorf("health: %s not ready after %s: %w", describe, elapsed.Round(time.Millisecond), ctx.Err())
	}
	return res
}

// checkURL rejects anything that is not an absolute http or https URL with a
// host, so a misconfigured health entry fails with a clear message instead of
// producing a confusing dial error on every poll.
func checkURL(rawURL string) error {
	if strings.TrimSpace(rawURL) == "" {
		return fmt.Errorf("health: empty health URL: %w", ErrBadURL)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("health: parse %q: %w: %w", rawURL, err, ErrBadURL)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("health: %q has scheme %q, want http or https: %w", rawURL, u.Scheme, ErrBadURL)
	}
	if u.Host == "" {
		return fmt.Errorf("health: %q has no host: %w", rawURL, ErrBadURL)
	}
	return nil
}
