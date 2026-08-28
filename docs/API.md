# Web Console HTTP API

> Machine-readable catalogue: `mabo-ctl schema --commands` prints every route
> with its auth level, flags, and exit codes as JSON. This document is the
> human-readable companion.

`mabo-ctl serve` binds a loopback HTTP listener and serves one embedded page
plus a JSON/SSE API. Every route below is registered from the single source of
truth in `internal/web/routes.go`.

> **Drift guard:** The machine-readable version of this surface is generated
> by `mabo-ctl schema --commands` and verified by `TestSchemaCommandsMatchesTheLiveTree`
> in `cmd/mabo-ctl/catalog_test.go`. Adding a route to `routes.go` without a
> matching entry in `commandMetas` fails the test suite. Regenerate the
> surfacemap (`go run ./tools/surfacemap`) and update this file when routes change.

## Base URL

```
http://127.0.0.1:<port>
```

The port is printed when `serve` starts. The URL carries a `?token=…` query
parameter — treat it as a password.

## Authentication

Three layers, enforced before the router sees the request:

| Layer | What it checks | Applies to |
|-------|---------------|------------|
| **Host** | `Host` header must name the address mabo-ctl bound to | Every request |
| **Origin** | `Origin` header must be in the trusted list (or match the bound address) | Every request with an Origin header |
| **Session** | Cookie (`mabo_session`), `?token=`, or `X-Mabo-Ctl-Token` header | All read and mutate routes |
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
permits only inline script/style and same-origin `fetch`/`EventSource`.

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
effective timeouts, and per-service port precedence.

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
    "log_path": ".dev/logs/backend.log"
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
| `429` | `{"error": "too many operations in flight…"}` | Concurrency limit reached |

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
  `.dev/exits/` is what makes a crash visible.
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
    "log_path": ".dev/logs/backend.log",
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
| `log_path` | string | Path to the service's log file under `.dev/` |
| `elapsed_ms` | int | Milliseconds since spawn; 0 when not started |
| `started_at` | string | RFC 3339 timestamp, or `""` when unknown |
| `uptime_ms` | int | Milliseconds the live process has been up; 0 when nothing running |
| `exit_code` | int | Last observed exit status; `-1` when signalled or never seen |
| `exit_signal` | string | Signal name (e.g. `"SIGKILL"`), `""` when none |
| `exited_at` | string | RFC 3339 timestamp of last death, or `""` when not seen die |

### Reading the contract

```bash
# Check if any service is degraded or worse
curl -s http://127.0.0.1:7999/api/status | \
  jq '[.[] | select(.phase | IN("degraded","failed","exited"))] | length > 0'

# Get the exit reason for a crashed service
curl -s http://127.0.0.1:7999/api/status | \
  jq '.[] | select(.phase == "exited") | {service, exit_code, exit_signal, detail}'

# Watch for phase transitions
curl -s -N http://127.0.0.1:7999/api/events | \
  jq --unbuffered '.phase'
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
