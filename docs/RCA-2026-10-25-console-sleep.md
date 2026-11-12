# RCA — web console froze after macOS sleep

**Date:** 2026-10-25 (reported), 2026-10-26 (fix deployed)
**Status:** fixed and deployed
**Scope:** `mabo-ctl serve` web console, `internal/web/console.html`
**Severity:** medium — local developer tooling unavailable until a manual
reload or restart; no service outage, data loss, or security impact.

---

## Summary

The web consoles of three separate projects
became unresponsive after the laptop slept. The supervisors were healthy the
whole time: a fresh `curl` to each listener answered in milliseconds while the
already-loaded page stayed frozen.

The failure was client-side state corruption in `internal/web/console.html`,
not a server deadlock:

- an in-flight poll latch with no independent escape;
- a request timeout that stopped at the response headers, leaving the body read
  unbounded;
- SSE streams that could sit in `CONNECTING` with no recovery path.

A server-side SSE write deadline was added as hardening, but it was not the
cause of the frozen UI.

## Impact

- Three local consoles affected; every supervised service kept running.
- No data loss, no service outage, no security impact — the console is
  loopback-only and token-gated.
- Recovery required a manual page reload or `mabo-ctl serve` restart, defeating
  the purpose of a long-running console.

## Causal chain

Long idle alone does not trigger this: normal idle keeps polling and SSE
heartbeats flowing. The failure needs a request that never settles, which sleep
produces by suspending the network stack and JavaScript timers.

1. The laptop sleeps. TCP connections and JavaScript timers are suspended; on
   wake, a connection can be left half-open and a `fetch` can remain pending
   indefinitely.
2. A `/api/status` poll is in flight when sleep begins. Its headers may already
   have arrived, so `request()` has already cleared the 8s abort timer. The
   subsequent `res.text()` has no timeout.
3. `pollNow()` has set its boolean guard `polling = true`. The guard is cleared
   only by that request's own completion handler.
4. The promise never settles. `polling` stays `true` forever, and every later
   poll — every 2s visible, 10s hidden — returns at the guard.
5. The page keeps rendering its last snapshot. The health banner may not even
   show "down", because the `catch` never runs.
6. Separately, an `EventSource` stuck in `CONNECTING` never fires `onerror`, so
   the page's only stream-reconnect path never runs either.

## Root causes

### RC-1 — Poll latch with no escape (primary)

```js
var polling = false;
function pollNow() {
  if (polling) { return; }
  polling = true;
  request(...).then(...).catch(...).then(function () {
    polling = false;
  });
}
```

A single request that never settles permanently disables polling. The guard
assumed every request eventually completes; a sleep/wake transport break
violates that assumption.

### RC-2 — Timeout ended at the headers

```js
return fetch(path, init).then(function (res) {
  clearTimeout(timer);        // headers arrived — timer gone
  return res.text().then(...) // body read is now unbounded
});
```

The 8s timeout protected only the wait for headers. A body that stalled after
headers — exactly what a connection frozen mid-response produces — hung
forever, feeding RC-1.

### RC-3 — SSE stuck in `CONNECTING`

The page reopened log and event streams from `onerror`. A connection that
stalls before opening does not necessarily fire `error`; it sits in
`readyState === 0`. There was no `visibilitychange`, `pageshow`, `online`, or
periodic recovery hook.

### RC-4 — Boot fetch had no retry (contributing)

A tab the browser reloaded while the machine was still waking could fail
`GET /api/services` and land on a dead "Cannot reach mabo-ctl" screen with no
retry. Same class of failure: one transient transport failure became permanent
UI state.

## Why the server looked responsible

Restarting `mabo-ctl serve` closed the sockets, which in practice rejected the
pending fetch and let the latch clear; reloading the page reset the JavaScript
state outright. Both actions masked the defect.

The decisive evidence was that fresh `curl` requests to all three servers
returned in milliseconds while the loaded pages were frozen. The handlers were
not stuck; the page had stopped asking.

## Server-side exposure (hardening, not cause)

The server intentionally has **no global `WriteTimeout`**, because an SSE
response may legitimately live for hours. But there was also no **per-write**
deadline, so a peer that stopped reading could block `sseWriter.write` forever
and pin a handler plus its tail goroutine. That was a real resource-leak
exposure, separate from the UI freeze.

## Fix

| Cause | Change | Guard |
|---|---|---|
| RC-1 | Poll carries an `AbortController` and sequence number; a new poll aborts the old one, stale results are ignored, and a 12s watchdog restarts a poll that never settles | `TestConsolePageRecoversFromAStalledTransport` |
| RC-2 | Timeout stays armed through `res.text()`; abort detection uses `ctl.signal.aborted` | same page test plus smoke |
| RC-3 | `recoverStreams()` on `visibilitychange`, `pageshow` (bfcache), `online`, plus a 30s interval; log panes track `openedAt` and reopen a stream stuck in `CONNECTING` | same page test |
| RC-4 | Initial `/api/services` fetch retries with backoff up to 30s | same page test |
| Server exposure | 15s per-write SSE deadline via `http.NewResponseController`; `statusRecorder.Unwrap()` exposes the real connection | `TestSSEWriteArmsAndClearsAWriteDeadline`, `TestStatusRecorderUnwrapsForResponseController` |

The deadline is per write, not per response, so a healthy idle stream survives;
each heartbeat gets a fresh 15s window.

## Verification

- `go test ./... -race -count=1` — all packages pass.
- `go vet ./...`, `gofmt -l .`, `golangci-lint run ./internal/web/...` — clean.
- `node --check` on the extracted page JavaScript — syntax OK.
- Temp-server smoke test: page/status 200, SSE events and log lines delivered,
  chatty output arrived.
- Live deployment: all three consoles serve the new page; `/api/status` returns
  the expected services; log streams deliver; `/api/events` survives past 15s
  and emits `: heartbeat`.

## Why it escaped detection

Tests covered HTTP status codes, payloads, and SSE frames — normal completion.
None covered **a transport that never settles**, which is the failure mode sleep
produces. There was no browser-level sleep/fault-injection test; AppleScript
inspection of Chrome timed out during diagnosis.

## Residual risk and follow-ups

- No real lid-close/wake reproduction was possible. The fix is validated by
  code inspection, regression tests, and a live smoke test; browser-specific
  timer throttling remains a theoretical variable.
- Tabs already open before deployment still run the old JavaScript until
  reloaded once. After that reload, recovery is automatic.
- A browser end-to-end test using CDP/Playwright to hang a request and then
  assert that polling resumes would close the detection gap. Not implemented.
- A page/build version handshake that auto-reloads a stale tab after a server
  upgrade would remove the one-time reload, but is a new feature and was not
  added.

## Lessons

- An "in flight" boolean needs an independent escape hatch; its owner can
  disappear.
- A timeout that ends at the headers is not a timeout.
- Recovery must be event-driven **and** have a periodic backstop, because sleep
  can fire none of the expected events.
- Server responsiveness does not imply client liveness; measure from the page's
  perspective.

## References

- `internal/web/console.html` — poll watchdog, stream recovery, boot retry.
- `internal/web/sse.go` — per-write SSE deadline.
- `internal/web/security.go` — `statusRecorder.Unwrap`.
- `internal/web/server_test.go` — `TestConsolePageRecoversFromAStalledTransport`.
- `internal/web/sse_test.go` — write-deadline and unwrap tests.
- `docs/LANDMINES.md` §19 — same bug in the regression catalogue.
