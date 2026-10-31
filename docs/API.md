# Web Console HTTP API

> Machine-readable catalogue: `mabo-ctl schema --commands` prints every route
> with its auth level, flags, and exit codes as JSON. This document is the
> human-readable companion.

`mabo-ctl serve` binds a loopback HTTP listener and serves one embedded page
plus a JSON/SSE API. Every route below is registered from the single source of
truth in `internal/web/routes.go`.

> **Drift guard:** The machine-readable version of this surface is generated
> by `mabo-ctl schema --commands` from the live route table, and the
> surfacemap drift gate (`internal/surface`) now enumerates the HTTP surface —
> every method+path+guard is a tracked id. Adding a route to `routes.go` fails
> the gate until you run `go run ./tools/surfacemap` and commit the regenerated
> `surfaces.json`; update this file at the same time.

## Base URL

```
http://127.0.0.1:<port>
```

The port is printed when `serve` starts. The URL carries a `?token=…` query
parameter — treat it as a password.

The key precedence is `mabo-ctl serve --access-key`, then the YAML
`console_access_key` setting. If neither is supplied, mabo-ctl generates a new
random key for the run. A configured key is a credential and should not be
committed to shared configuration.

## Authentication

Three layers, enforced before the router sees the request:

| Layer | What it checks | Applies to |
|-------|---------------|------------|
| **Host** | `Host` header must name the address mabo-ctl bound to | Every request |
| **Origin** | `Origin` header must be in the trusted list (or match the bound address) | Every request with an Origin header |
| **Session** | Cookie (`mabo-ctl_console_<port>`, set by opening the printed URL), `?token=`, or `X-Mabo-Ctl-Token` header | All read and mutate routes |
| **Token** | `X-Mabo-Ctl-Token` header only (not cookie or query) | POST routes only |

A `GET` without a valid session gets the unlock page (HTML form to paste the
token). A `GET` with a valid session gets the console page. A `POST` without
the header gets `403`.

No `Access-Control-Allow-*` headers are ever set — cross-origin scripts cannot
read any response body.

## Security Headers

Every response carries:

```
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
X-Frame-Options: DENY
Cache-Control: no-store
```

The console page additionally carries a strict `Content-Security-Policy` that
permits only inline script/style, same-origin script (the terminal bundle),
same-origin `fetch`/`EventSource`, and a same-origin frame (the API tab). The
one exception to `X-Frame-Options: DENY` is the authenticated `/api-docs` page,
which sends `SAMEORIGIN` and `frame-ancestors 'self'` so the console's API tab
can frame it; no other origin can.

---

## Routes

### `GET /`

The console page. An unauthenticated browser gets a token-entry form (401);
a authenticated one gets the full console (200).

**Auth:** Session (cookie, query, or header).

```bash
# Open the console page (token in query)
curl -i 'http://127.0.0.1:7999/?token=YOUR_TOKEN'

# Or with header
curl -i -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' http://127.0.0.1:7999/
```

---

### `GET /health`

Unauthenticated server liveness check. Returns `200` when the web server is
running. This is the **only** route that does not require authentication —
monitoring tools, load balancers and CI probes need it without a session token.

The response reveals nothing beyond liveness: no service state, no version,
no uptime, no credential.

**Auth:** None.

```bash
# Simple liveness check
curl -i http://127.0.0.1:7999/health
```

**Response** `200 OK` — `application/json`:

```json
{
  "status": "ok"
}
```

---

### `GET /api/services`

Every declared service as resolved: name, dir, cmd, runtime, port, health URL,
dependencies, colours.

**Auth:** Session.

```bash
curl -s -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  http://127.0.0.1:7999/api/services | jq
```

**Response** `200 OK` — `application/json`:

```json
[
  {
    "name": "backend",
    "dir": "/repo/backend",
    "port": 7102,
    "health": "http://localhost:7102/health",
    "runtime": "conda:app-dev",
    "cmd": ["/opt/conda/envs/app-dev/bin/uvicorn", "api_main:app", "--port", "7102"],
    "cmd_line": "/opt/conda/envs/app-dev/bin/uvicorn api_main:app --port 7102",
    "cmd_error": "",
    "env": [
      {"key": "LOG_LEVEL", "value": "info"}
    ],
    "depends_on": [],
    "color": "blue"
  }
]
```

Fields:

| Field | Type | Notes |
|-------|------|-------|
| `name` | string | Declared service name |
| `dir` | string | Resolved absolute working directory |
| `port` | int | Resolved port; `0` when no port is declared |
| `health` | string | Expanded readiness URL; `""` when no probe |
| `runtime` | string | Declared runtime (`""`, `"system"`, `"conda:x"`, `"node:x"`) |
| `cmd` | string[] | Expanded argv, `cmd[0]` absolute |
| `cmd_line` | string | `cmd` rendered as one shell-quoted line, for copying |
| `cmd_error` | string | Why `cmd[0]` could not be resolved (omitted when empty) |
| `env` | array | Declared environment; values redacted by key |
| `depends_on` | string[] | Services that start first |
| `color` | string | Label colour from config; `""` when none |

**Redaction:** `health`, `cmd`, and `env` values are redacted by
`internal/redact` — credential-shaped values are replaced with `[redacted]`.

---

### `GET /api/config`

The loaded `mabo-ctl.yaml` plus where each resolved port came from.

**Auth:** Session.

```bash
curl -s -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  http://127.0.0.1:7999/api/config | jq
```

**Response** `200 OK` — `application/json`:

The shape matches `mabo-ctl config --json` exactly (generated by
`ui.ConfigJSON`). Includes the config path, repo root, state directory,
effective timeouts, the optional `console_addr`, and per-service port
precedence.

---

### `GET /api/status`

One status per service, shaped exactly like `mabo-ctl status --json`.

**Auth:** Session.

```bash
curl -s -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  http://127.0.0.1:7999/api/status | jq
```

**Response** `200 OK` — `application/json`:

```json
[
  {
    "service": "backend",
    "phase": "ready",
    "port": 7102,
    "pid": 41825,
    "uptime_ms": 2000,
    "started_at": "2026-08-28T10:00:00Z",
    "health": "[redacted]",
    "detail": "HTTP 200",
    "http_status": 200,
    "exit_code": -1,
    "exit_signal": "",
    "exited_at": "",
    "log_path": ".mabo-ctl/logs/backend.log"
  }
]
```

**This is a stable machine contract.** Fields and phase values may not change
without a version bump. The `health` URL is **redacted** here (unlike
`mabo-ctl status --json` on a terminal) because this route is reachable by any
local process.

**Side effect:** Running readiness probes is a side effect of this route.

---

### `GET /api/logs/{svc}`

The last N lines of a service's log.

**Auth:** Session.

```bash
# Last 100 lines
curl -s -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  'http://127.0.0.1:7999/api/logs/backend?tail=100' | jq

# Last 10 lines
curl -s -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  'http://127.0.0.1:7999/api/logs/backend?tail=10' | jq
```

**Query parameters:**

| Param | Default | Notes |
|-------|---------|-------|
| `tail` | 200 | Number of lines; clamped to max |

**Response** `200 OK` — `application/json`:

```json
{
  "service": "backend",
  "lines": [
    "INFO:     Uvicorn running on http://0.0.0.0:7102 (Press CTRL+C to quit)",
    "INFO:     Started reloader process [41825]"
  ],
  "count": 2
}
```

**Errors:**

| Status | Body | When |
|--------|------|------|
| `404` | `{"error": "unknown service", "valid": [...]}` | `{svc}` not in the declared set |

---

### `GET /api/stream/{svc}`

SSE: live log tail of one service.

**Auth:** Session.

```bash
# Stream with EventSource in the browser
# eventsource = new EventSource('/api/stream/backend?tail=50')

# Or use curl to see the stream (Ctrl-C to stop)
curl -N -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  'http://127.0.0.1:7999/api/stream/backend?tail=50'
```

**Query parameters:**

| Param | Default | Notes |
|-------|---------|-------|
| `tail` | 200 | Initial replay count; clamped to max |

**Response** `200 OK` — `text/event-stream`:

Each event is an unnamed SSE event with a JSON data payload:

```sse
data: {"service":"backend","line":"INFO:     Started reloader process [41825]"}

data: {"service":"backend","line":"INFO:     Finished server process [41825]"}

data: {"service":"backend","end":true}
```

**Event shapes:**

```json
// Log line
{"service": "backend", "line": "the log line"}

// Stream end (tail returned or service stopped)
{"service": "backend", "end": true, "error": "optional reason"}
```

Heartbeats are sent as SSE comments (`: heartbeat`) at a fixed interval.
Reconnect delay: 2 seconds.

---

### `GET /api/stream/all`

SSE: merged log tail of every running service.

**Auth:** Session.

```bash
# Stream all services
curl -N -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  'http://127.0.0.1:7999/api/stream/all?tail=100'
```

**Query parameters:**

| Param | Default | Notes |
|-------|---------|-------|
| `tail` | 200 | Initial replay count per service; clamped to max |

**Response** `200 OK` — `text/event-stream`:

Same event shapes as `/api/stream/{svc}`, but lines from every service are
interleaved. Each log line carries its `service` field so the client can
label or filter by service.

**Errors:**

| Status | Body | When |
|--------|------|------|
| `404` | plain text | No services are declared |

---

### `GET /api/events`

SSE: lifecycle events (phase transitions) for the whole stack.

**Auth:** Session.

```bash
# Stream lifecycle events
curl -N -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  http://127.0.0.1:7999/api/events
```

**Response** `200 OK` — `text/event-stream`:

```sse
data: {"service":"backend","phase":"running","msg":"spawned"}

data: {"service":"backend","phase":"ready","msg":"HTTP 200"}

data: {"service":"worker","phase":"failed","msg":"exit code 1","error":"traceback…"}
```

**Event shape:**

```json
{
  "service": "backend",
  "phase": "ready",
  "msg": "HTTP 200",
  "error": ""
}
```

`phase` is one of: `stopped`, `running`, `ready`, `slow`, `degraded`,
`failed`, `exited`. `error` is present only on failures.

---

### `GET /api/history`

The most recent lifecycle events recorded since the console started, oldest
first. Read-only; never touches supervisor state.

**Auth:** Session.

```bash
curl -s -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  http://127.0.0.1:7999/api/history | jq
```

**Response** `200 OK` — `application/json`:

```json
{
  "events": [
    {"service": "backend", "phase": "running", "msg": "spawned"},
    {"service": "backend", "phase": "ready", "msg": "HTTP 200"}
  ]
}
```

Capped at 50 events (ring buffer). The same event shape as `/api/events`.

---

### `GET /api/origins`

The browser origins currently trusted for cross-origin access.

**Auth:** Session.

```bash
curl -s -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  http://127.0.0.1:7999/api/origins | jq
```

**Response** `200 OK` — `application/json`:

```json
{
  "trusted": ["https://my-tunnel.example.com"],
  "implicit": "http://127.0.0.1:7999",
  "origin": "http://127.0.0.1:7999",
  "max": 10
}
```

| Field | Type | Notes |
|-------|------|-------|
| `trusted` | string[] | The editable allowlist, canonical and sorted |
| `implicit` | string | The address mabo-ctl bound; always accepted, not in `trusted`, cannot be removed |
| `origin` | string | The requester's `Origin` header (for UI to show which entry is active) |
| `max` | int | Cap on `trusted` |

---

### `POST /api/origins`

Replace the trusted-origin list.

**Auth:** Token header required.

```bash
curl -s -X POST \
  -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  -H 'Content-Type: application/json' \
  -d '{"trusted": ["https://my-tunnel.example.com"]}' \
  http://127.0.0.1:7999/api/origins | jq
```

**Request body:**

```json
{
  "trusted": ["https://my-tunnel.example.com", "https://another.example.com"]
}
```

**Response** `200 OK` — same shape as `GET /api/origins`.

**Errors:**

| Status | Body | When |
|--------|------|------|
| `400` | `{"error": "…"}` | Malformed body |
| `409` | `{"error": "…"}` | Would lock out the current origin (`ErrOriginLockout`) |

---

### `POST /api/config/reload`

Apply `mabo-ctl.yaml` as it now reads on disk — the edit the console announced
on `/api/events` with `config change detected; reload configuration?`.

- A **running** service whose command, directory, environment (an `env_file`
  included), port or `tty` changed is stopped under its old definition and
  started under its new one. A dependency it needs that is down is started too.
- A **running** service the file no longer declares is stopped.
- **Every other service keeps running, untouched.** A colour, probe, `open`,
  `ready_timeout`, `depends_on`, `autostart` or hook edit restarts nothing; the
  new value applies from here on. Adding or removing a service does not restart
  its siblings.
- A port edited in the file is adopted for that service even though
  `.mabo-ctl/run.env` pinned the old one; other services keep their pinned port.
- A service a reload stopped and could not start again is started by the next
  reload.
- `console_access_key` replaces the session key at once unless the console was
  started with `--access-key`; the old key, its cookie and every stream it
  opened stop working. A `console_addr` edit applies when `serve` restarts.

The reload runs to completion even if the client disconnects: one abandoned half
way leaves services stopped that nothing starts again. Progress is published on
`/api/events` as it happens.

**Auth:** Token header required.

```bash
curl -s -X POST \
  -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  http://127.0.0.1:7999/api/config/reload | jq
```

**Request body:** none.

**Response** `200 OK` — the new configuration is in force:

```json
{
  "ok": false,
  "error": "services did not start: worker",
  "stopped": ["legacy"],
  "restarted": ["backend", "worker"],
  "events": [
    {"service": "worker", "phase": "failed", "msg": "failed: process died\n..."}
  ]
}
```

`ok` is `true` when every affected service was stopped and started cleanly. When
it is `false`, `error` names what failed and `events` says why; like a start
that fails, that is an outcome of the request, not an HTTP error.

**Errors:**

| Status | Body | When |
|--------|------|------|
| `409` | `{"error": "…"}` | A start, stop or restart is in progress; the reload was not attempted |
| `422` | `{"error": "…"}` | Nothing was applied: the file did not load, validate or resolve, a service it needs cannot run, or a removed service survived its stop. The previous configuration remains in force |

Start, stop and restart answer `409` while a reload runs, for the same reason.

---

### `POST /api/start-all`

Start every declared service (including `autostart: false`).

**Auth:** Token header required.

```bash
curl -s -X POST \
  -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  http://127.0.0.1:7999/api/start-all | jq
```

**Request body:** none.

**Response** `200 OK` — `application/json`:

```json
{
  "operation": "start",
  "services": ["backend", "frontend", "worker"],
  "ok": true,
  "events": [
    {"service": "backend", "phase": "running", "msg": "spawned"},
    {"service": "backend", "phase": "ready", "msg": "HTTP 200"}
  ]
}
```

**Response** `200 OK` (operation failed):

```json
{
  "operation": "start",
  "services": ["backend", "frontend"],
  "ok": false,
  "error": "service frontend failed to start",
  "events": [...]
}
```

A service that refuses to start is an expected outcome, not an HTTP error —
the request succeeded, the start did not. `events` are capped at a fixed
limit.

**Errors:**

| Status | Body | When |
|--------|------|------|
| `409` | `{"error": "configuration reload in progress…"}` | A configuration reload is running; retry when it finishes |
| `429` | `{"error": "too many operations in flight…"}` | Concurrency limit reached |

Every start, stop and restart route answers `409` while a reload runs.

---

### `POST /api/stop-all`

Stop every running service.

**Auth:** Token header required.

```bash
curl -s -X POST \
  -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  http://127.0.0.1:7999/api/stop-all | jq
```

**Request body:** none.

**Response** `200 OK` — same shape as `/api/start-all` with `operation: "stop"`.

---

### `POST /api/restart-all`

Restart every service that is running now (`running`, `ready`, `slow` or
`degraded`). Stopped, failed and exited services are left alone, and the
services are named explicitly, so `autostart: false` does not change what is
restarted.

**Auth:** Token header required.

```bash
curl -s -X POST \
  -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  http://127.0.0.1:7999/api/restart-all | jq
```

**Request body:** none.

**Response** `200 OK` — same shape as `/api/start-all` with
`operation: "restart"` and `services` naming the services that were running.

**Errors:**

| Status | Body | When |
|--------|------|------|
| `409` | `{"error": "nothing is running, so there is nothing to restart"}` | No service is running |
| `409` | `{"error": "configuration reload in progress…"}` | A configuration reload is running |
| `429` | `{"error": "too many operations in flight…"}` | Concurrency limit reached |

---

### `POST /api/{svc}/start`

Start one named service.

**Auth:** Token header required.

```bash
curl -s -X POST \
  -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  http://127.0.0.1:7999/api/backend/start | jq
```

**Request body:** none.

**Response** `200 OK` — same shape as `/api/start-all` with `operation: "start"`
and `services: ["{svc}"]`.

**Errors:**

| Status | Body | When |
|--------|------|------|
| `404` | `{"error": "unknown service", "valid": [...]}` | `{svc}` not in the declared set |
| `409` | `{"error": "configuration reload in progress…"}` | A configuration reload is running |
| `429` | `{"error": "too many operations in flight…"}` | Concurrency limit reached |

---

### `POST /api/{svc}/stop`

Stop one named service.

**Auth:** Token header required.

```bash
curl -s -X POST \
  -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  http://127.0.0.1:7999/api/backend/stop | jq
```

**Response** — same shape as `/api/{svc}/start` with `operation: "stop"`.

---

### `POST /api/{svc}/restart`

Restart one named service (stop, then start).

**Auth:** Token header required.

```bash
curl -s -X POST \
  -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  http://127.0.0.1:7999/api/backend/restart | jq
```

**Response** — same shape as `/api/{svc}/start` with `operation: "restart"`.

---

## Terminal routes

The console's Terminal tab runs interactive login shells (`$SHELL -l`, falling
back to `/bin/sh`) on a pty in the repository root, as the user running
mabo-ctl, with `TERM=xterm-256color`. A shell can run anything, so these
routes are held to more than the rest of the API:

- **Opt-in.** They exist only when serve was started with `--terminal` or the
  config sets `console_terminal: true`. Otherwise every one answers `404`.
- **Loopback only.** `serve --terminal` refuses a non-loopback bind, and each
  request must name the loopback address itself in `Host` and, when it sends
  one, in `Origin`, and must carry none of the headers a relaying proxy adds
  (`Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host`, `X-Real-Ip`,
  `Cf-Connecting-Ip`). A browser reaching the console through `--allow-origin`
  (a tunnel, a LAN name) gets `403` here while every other route works.
- **Bounded.** At most 16 terminals are open at once; opening another answers
  `429`.
- The usual guards still apply first: session for reads, token header for
  POSTs.

A terminal outlives the browser tab that opened it and keeps the last 1 MiB of
output as scrollback. It ends when its shell exits — even if a process the
shell detached into its own session still holds the pty — when it is closed,
or when serve stops (SIGHUP to its session, then SIGKILL after 2 seconds).
The page follows OSC 8 hyperlinks in terminal output only for `http`/`https`,
in a window with no handle back to the console.

### `GET /api/terminals`

The open terminals, oldest first.

**Auth:** Session.

**Response** `200 OK`:

```json
[{"id": "1", "pid": 48121, "started_at": "2026-10-31T20:14:03Z"}]
```

### `POST /api/terminals`

Open a new shell.

**Auth:** Token header required.

```bash
curl -s -X POST -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' \
  -d '{"cols": 120, "rows": 40}' http://127.0.0.1:7999/api/terminals
```

**Request body:** `{"cols": N, "rows": N}` — clamped to 2–1000 × 2–500.

**Response** `200 OK` — one terminal, shaped as in the list above; `429` when
16 are already open.

### `GET /api/terminals/{id}/stream`

SSE: the terminal's scrollback as one frame, then its live output, then an end
frame when its shell exits. Output is base64 because pty output is bytes, not
necessarily UTF-8 at a frame boundary.

**Auth:** Session.

```sse
data: {"data": "bWFib0BkZXYgfiAlIA==", "replay": true}

data: {"data": "bHMK"}

data: {"end": true}
```

Every connection replays the scrollback, so a client resets its screen on
open. The scrollback frame is marked `"replay": true`, and a client must not
send anything its emulator generates while rendering it: the scrollback holds
queries programs sent earlier (device attributes, colour reports, mode
requests), and answering them again types the answers into the shell. A client that falls far behind is disconnected without an end frame and
reconnects.

### `POST /api/terminals/{id}/input`

Type into a terminal.

**Auth:** Token header required.

**Request body:** `{"data": "ls -la\r"}` — up to 1 MiB per request.

**Response** `204 No Content`; `404` for an unknown terminal, `410` when it
has ended. Accepted input is not written to the audit log; refusals are.

### `POST /api/terminals/{id}/resize`

**Auth:** Token header required.

**Request body:** `{"cols": N, "rows": N}`.

**Response** `204 No Content`.

### `POST /api/terminals/{id}/close`

Hang up a terminal: SIGHUP to its session, then SIGKILL if it is still there
after 2 seconds.

**Auth:** Token header required.

**Response** `204 No Content`; `404` for an unknown terminal.

### `GET /assets/xterm.js`

The embedded terminal emulator the Terminal tab loads: xterm.js 6.0.0 with its
fit and web-links addons, vendored verbatim under their MIT licence.

**Auth:** Session.

---

### `GET /api-docs`

Interactive API reference page served directly from the binary, rendered by
**RapiDoc** (the MIT-licensed bundle vendored into the binary at
`internal/web/rapidoc-min.js`, served at `/api-docs/rapidoc.js`). No CDN, no
external scripts, no telemetry — the page's only requests are to mabo-ctl
itself.

The spec is loaded from `/api/openapi.yaml` (a same-origin, session-authenticated
request), and the try-it server is pinned to the address mabo-ctl actually
bound, so the playground reaches this console whatever port it is on.

**Every endpoint has a try-it playground** — the "Try it" panel on each
operation sends a real same-origin request, with path/query parameters, a
request-body editor (generated sample payload), and a rendered response. The
session token is wired in automatically as the `X-Mabo-Ctl-Token` api-key, so
reads **and** mutations are authenticated exactly like the console buttons.
Live SSE streams render as the data arrives.

**Auth:** Session cookie or token header required (shows unlock form if
unauthenticated, like the console page).

```bash
curl -i -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' http://127.0.0.1:7999/api-docs
```

**Response** `200 OK` — `text/html; charset=utf-8`.

---

### `GET /api-docs/rapidoc.js`

The embedded RapiDoc bundle (MIT) that renders `/api-docs`. A static,
session-gated asset: it defines the `<rapi-doc>` custom element, so only a
browser that can load the documented page may load its renderer. Vendored from
the npm package `rapidoc`;
see `internal/web/rapidoc.LICENSE.txt`.

**Auth:** Session cookie or token header required.

```bash
curl -i -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' http://127.0.0.1:7999/api-docs/rapidoc.js
```

**Response** `200 OK` — `application/javascript`.

---

### `GET /api/openapi.yaml`

The OpenAPI 3.0 specification in YAML. This is the machine-readable version of
what `/api-docs` renders visually. Useful for code generators, Postman imports
and CI probes.

The canonical source is `docs/openapi.yaml`; the embedded copy must stay in
sync (run `cp docs/openapi.yaml internal/web/openapi.yaml`).

**Auth:** Session cookie or token header required.

```bash
curl -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' http://127.0.0.1:7999/api/openapi.yaml
```

**Response** `200 OK` — `text/yaml; charset=utf-8`.

---

## Phase Machine

Every service reports one of exactly seven phases. The set is closed — adding
a new phase is a one-way door because `status --json` is a stable contract.

| Phase | Process | Probe | Meaning |
|-------|---------|-------|--------|
| `stopped` | absent | — | Never started, or stopped by mabo-ctl |
| `running` | alive | none declared | "answering" is not a question mabo-ctl can ask |
| `ready` | alive | answered | The normal state |
| `slow` | alive | silent | Still inside `ready_timeout` of the spawn |
| `degraded` | alive | silent | Past `ready_timeout` — "still starting" would be a lie |
| `failed` | gone | — | Died *during* startup; never came up |
| `exited` | gone | — | Came up, then died without mabo-ctl stopping it |

Key distinctions:

- **`slow` vs `failed`**: A slow starter and a dead one look identical if you
  only check whether the port answers. mabo-ctl keeps them distinct.
- **`stopped` vs `exited`**: A process that crashed is simply *not there*,
  byte-identical to one that was never started. The exit record in
  `.mabo-ctl/exits/` is what makes a crash visible.
- **`exited` will not grow a restart policy.** mabo-ctl exists to make a death
  *visible*, not to silently resurrect it.

---

## Status JSON Contract

`GET /api/status` and `mabo-ctl status --json` emit the same shape. This is a
**stable machine contract** — fields and phase values may not change without a
version bump.

```json
[
  {
    "service": "backend",
    "phase": "ready",
    "pid": 41825,
    "port": 7102,
    "health": "http://localhost:7102/health",
    "http_status": 200,
    "detail": "HTTP 200",
    "log_path": ".mabo-ctl/logs/backend.log",
    "elapsed_ms": 2000,
    "started_at": "2026-08-28T10:00:00Z",
    "uptime_ms": 2000,
    "exit_code": -1,
    "exit_signal": "",
    "exited_at": ""
  }
]
```

### Field Reference

| Field | Type | Description |
|-------|------|-------------|
| `service` | string | Declared service name |
| `phase` | string | One of: `stopped`, `running`, `ready`, `slow`, `degraded`, `failed`, `exited` |
| `pid` | int | Process ID; 0 when nothing is running |
| `port` | int | Resolved port; 0 when none declared |
| `health` | string | Expanded readiness URL; redacted in API responses |
| `http_status` | int | Last HTTP probe status code; 0 when no probe or no response |
| `detail` | string | Human-readable detail (e.g. "HTTP 200", "dial tcp …: refused") |
| `log_path` | string | Path to the service's log file under `.mabo-ctl/` |
| `elapsed_ms` | int | Milliseconds since spawn; 0 when not started |
| `started_at` | string | RFC 3339 timestamp, or `""` when unknown |
| `uptime_ms` | int | Milliseconds the live process has been up; 0 when nothing running |
| `exit_code` | int | Last observed exit status; `-1` when signalled or never seen |
| `exit_signal` | string | Signal name (e.g. `"SIGKILL"`), `""` when none |
| `exited_at` | string | RFC 3339 timestamp of last death, or `""` when not seen die |

### Reading the contract

All three examples need the session credential — `/api/status` and
`/api/events` refuse an unauthenticated request with 403.

```bash
# Check if any service is degraded or worse
curl -s -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' http://127.0.0.1:7999/api/status | \
  jq '[.[] | select(.phase | IN("degraded","failed","exited"))] | length > 0'

# Get the exit reason for a crashed service
curl -s -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' http://127.0.0.1:7999/api/status | \
  jq '.[] | select(.phase == "exited") | {service, exit_code, exit_signal, detail}'

# Watch for phase transitions
# SSE frames arrive as "data: {json}" lines; strip the prefix before jq.
curl -s -N -H 'X-Mabo-Ctl-Token: YOUR_TOKEN' http://127.0.0.1:7999/api/events | \
  grep '^data:' | sed 's/^data: //' | jq --unbuffered -r '.phase'
```

---

## Common Response Shapes

### Error response

Every JSON error uses this shape:

```json
{
  "error": "description of what went wrong",
  "valid": ["optional", "list", "of", "valid", "names"]
}
```

`valid` is present when the error is an unknown service name, listing every
declared name so the caller does not have to guess.

### Event response (mutations)

All POST routes that trigger a supervisor operation return:

```json
{
  "operation": "start|stop|restart",
  "services": ["svc1", "svc2"],
  "ok": true,
  "error": "",
  "events": [
    {"service": "svc1", "phase": "running", "msg": "spawned"}
  ]
}
```

`events` contains up to `maxCollectedEvents` lifecycle events collected during
the operation. The same events also go to `/api/events` in real time.

---

## SSE Conventions

All SSE routes share these conventions:

- **Content-Type:** `text/event-stream`
- **Cache-Control:** `no-cache`
- **Connection:** `keep-alive`
- **X-Accel-Buffering:** `no` (for reverse proxies)
- **Events are unnamed** — routed to `onmessage` in the browser
- **Data is JSON** on a single `data:` line (no raw newlines possible)
- **Heartbeats** are SSE comments (`: heartbeat`) at a fixed interval
- **Reconnect** hint: 2 seconds
- **Write deadline:** one write that cannot complete within 15 seconds ends
  the stream, so a peer that stops reading cannot pin a handler forever. There
  is no overall response timeout — a stream may legitimately stay open for
  hours.

When a stream ends (service stopped, tail returned, server shutting down), the
handler sends an end event and closes the connection.

---

## Concurrency

Mutating POST routes are bounded by a semaphore (`maxInflightMutations`).
When the limit is reached, the response is `429 Too Many Requests`.

Operations run under `context.Background()` — closing the browser tab does
not abort an in-flight start. The operation finishes and its events reach
every other open console through the broker.

---

## Redaction

All output channels — JSON responses, SSE streams, the console page — redact
credential-shaped values through `internal/redact` at the source:

- **Health URLs** with `userinfo` or `api_key` query parameters
- **Command arguments** containing token-like strings
- **Environment values** declared in `mabo-ctl.yaml`

The inherited environment (the one holding your real tokens) is **never** sent
to the browser.

---

## See Also

- [`mabo-ctl schema --commands`](../README.md#for-agents-and-scripts) —
  machine-readable catalogue of every route, flag, and exit code
- [SECURITY.md](../SECURITY.md) — trust boundary, the console's guards,
  accepted risks
- [ARCHITECTURE.md](ARCHITECTURE.md) — how the web console fits in the layering
