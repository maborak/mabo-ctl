# Security Policy

## Reporting a vulnerability

**Please do not open a public issue for a security problem.**

Use GitHub's private reporting: **Security → Advisories → Report a vulnerability**
on this repository. If that is unavailable, email the maintainer listed in
`go.mod`'s module owner / the commit history.

Please include:

- the mabo-ctl version (`mabo-ctl --version`) and OS,
- a `mabo-ctl.yaml` that reproduces it (redact your own secrets),
- what you expected, and what happened.

Expect an acknowledgement within a week. There is no bounty or guaranteed
response time.

## Supported versions

Use the latest release. Fixes land on `main`; older releases do not receive
backports.

---

## The trust boundary — read this before filing

mabo-ctl runs the commands written in your `mabo-ctl.yaml`. **That is arbitrary code
execution by design, and it is not a vulnerability.**

```yaml
services:
  - name: api
    cmd: [anything, you, want]   # mabo-ctl will run exactly this, as you
```

Cloning an untrusted repository and running `mabo-ctl` inside it is equivalent to
running that repository's code. It has the same trust profile as `make`,
`npm run`, `docker compose up`, or a `Makefile`. This risk is **accepted and
documented**, not defended against.

### What IS in scope

Anything that lets code run, or data escape, **without** the operator having
written it into their own `mabo-ctl.yaml`:

| Class | Example |
|---|---|
| Config discovery escaping the repo | a `mabo-ctl.yaml` in an unrelated parent directory being loaded and executed |
| Template expansion | `{{.Port}}` reaching further than the declared data |
| Path handling of `name` | a service name escaping `.mabo-ctl/` via `/` or `..` |
| Signal blast radius | mabo-ctl signalling a process it did not start |
| Secret handling | credentials reaching a log, `--json`, or an HTTP response that a reader would not expect |
| **The `mabo-ctl serve` web console** | any bypass of its token, `Host`/`Origin`, or method checks |
| **The console's Terminal tab** | a shell reachable when the terminal is off, through a non-loopback bind, or from an origin other than the loopback address |

### `mabo-ctl serve` deserves particular attention

It is the only surface something other than the developer's keyboard can reach.
Its POST routes call the same supervisor `start`/`stop`/`restart` the CLI calls,
so **a bypass of any of its controls is remote code execution as the developer.**

The controls, as of the current `main`:

- **Loopback bind by default** (`127.0.0.1:7999`). A non-loopback `--addr`
  requires `--i-know-this-is-dangerous` and exits `2` without it.
- **A per-run session token**, 32 random bytes, or the configured access key.
  Required as the `X-Mabo-Ctl-Token` **header** on every mutation — never a
  cookie, never a query parameter, because both of those ride a cross-origin
  request and a custom header cannot without a CORS preflight this server never
  answers. A `console_access_key` changed in `mabo-ctl.yaml` and applied through
  the configuration reload replaces the key at once (unless `--access-key` was
  given): the old key, its cookie and every stream it opened stop working.
- **The console page and read routes** accept the token from the query, a
  `SameSite=Strict` `HttpOnly` cookie, or the header — a browser navigation
  cannot send a header and an `EventSource` cannot either. A browser with no
  token gets a box to paste one into, and that page contains no token.
- **Scoped sharing capabilities, not user accounts.** Operator-only CONFIG
  controls create anonymous named links with `read`, `view` or `write` access.
  READ and VIEW cannot mutate; WRITE can only start/stop/restart declared
  services (including bulk operations). Every guest is denied terminals,
  trusted-origin management, configuration reload and all access-management
  routes on the server, not merely in the UI. New routes are owner-only until
  explicitly added to the closed guest allowlist. Guest pages and `/api-docs`
  receive only the guest's own credential, never the operator's.
- **Credential precedence is explicit query → header → cookie.** An invalid
  explicit credential fails closed, rather than falling back to an operator
  cookie. This prevents a revoked/scoped link from being upgraded by ambient
  credentials. Native SSE URLs carry their page's credential, so another tab
  cannot alter that stream's scope by replacing the shared cookie. Treat these
  URLs as secrets too; audit logs record paths, never query strings.
- **Bounded, revocable shares.** Guest tokens are hashed in memory and checked
  in constant time (maximum 32 active links, 128 creations per run). Custom
  tokens require 16–256 characters from the access-key alphabet; generated
  tokens have 256 bits of randomness. Names are labels, not proof of identity.
  Up to 256 browser-session records retain socket peer, browser and join/last
  seen times for this serve run; forwarded addresses are not trusted. Revoking
  a share cancels its live streams and refuses further requests/cookies, and
  the page discards rendered content on its next denied status/mirror poll.
  The same URL is unusable afterward and previously shared tokens cannot be
  reused or promoted to operator tokens. Changing the operator token cancels
  operator streams and revokes all shares. Already accepted actions may finish;
  copied data cannot be recalled. No persistent user/session database is added.
  Each browser-and-link record also has bounded counters and only its latest
  lifecycle action/result. Results come from the supervisor, not HTTP status;
  no request bodies, query strings or operation error text are retained. Route
  tracking stores the HTTP method and route template, not arbitrary raw paths.
  The mutation header alone determines action attribution, never a query token.
  Public session IDs are derived per browser/link and do not expose the browser
  cookie. Browser/OS labels remain unverified User-Agent claims, and observed
  spans/automatic polling do not establish continuous human activity. API clients
  without the visitor cookie are not attributed to browser sessions. No new
  fingerprinting, geolocation or input history is added.
- **VIEW is presentation synchronization, not desktop sharing.** Only an
  operator can publish service/tab/log-control state, and only VIEW guests
  can read it. Terminal content, credentials, clipboard and security forms
  have no fields in the mirror payload. Configuration is redacted as usual;
  service logs remain child-owned output and may contain secrets. A publishing
  tab holds a five-second lease, renewed every 500ms; another operator must
  explicitly take over. Guests cannot publish or operate services. A viewer's
  log buffer/history and wrapping can differ from the operator's.
  VIEW's cursor and click ripple are non-interactive visual overlays, never
  replayed DOM/system input. The server accepts normalized points only in a
  closed allowlist of sidebar/tab/output/non-security CONFIG regions, not
  arbitrary selectors or security/terminal/API iframe areas. No text, values,
  keystrokes or movement/click history are captured by pointer tracking; the
  existing log filter still mirrors its search string. Only the latest point and
  recent click remain in memory. The client hides them on loss of broadcast,
  and the server suppresses them for inactive, stale or terminal broadcasts.
  Motion is sampled at most 10 times per second; idle publishing is every 500ms.
  CONFIG's Sharing & guests and Security sections are operator-only. Mirror
  publishing normalizes either administrative section to the read-only Overview
  and removes pointer/click data while it is open. Only Overview/Services section
  choices are returned to VIEW guests. The terminal's docked service Output
  reuses the existing log pane; docking/resizing never grants terminal access to
  a guest or changes the loopback-only shell gate.
  A translucent top banner tells guests their scope and warns a broadcasting
  operator that VIEW guests can see their console cursor/clicks. Reduced-motion
  preferences remove cursor interpolation and ripple animation.
- **`/api-docs` has the same boundary.** Unauthenticated, it answers the unlock
  page with no token and no page. Authenticated, the session token is rendered
  into the page as the `X-Mabo-Ctl-Token` api-key on the `<rapi-doc>` element
  so its try-it playground can issue real requests (including mutations) under
  the same session the console buttons use — the equal of the console page, no
  more. The vendored RapiDoc bundle is served at `/api-docs/rapidoc.js`,
  session-gated like the rest of the docs surface, and the docs page's CSP adds
  `'self'` to `script-src` for exactly that file.
- **`Host` and `Origin` validation** against the bound address. A `Host` that is
  a DNS name other than `localhost` is refused outright: that is the
  DNS-rebinding defence. `--allow-origin` widens it to named hosts or
  `*.`-subdomain patterns, never a bare `*` without the danger flag. An
  explicitly authorised non-loopback bind accepts its exact matching HTTP
  origin; configured non-loopback origins still require HTTPS.
- **POST-only mutations.** A `GET` is reachable from an `<img>` tag.
- **`GET /health` is unauthenticated** — the only route without a session gate.
  It reveals nothing beyond liveness (`{"status":"ok"}`): no service state,
  no version, no uptime, no credentials. Monitoring tools and CI probes need
  it without a token.
- **`{svc}` validated** against the declared service names before the supervisor
  sees it.
- **Declared-env-only rendering**, with credential-shaped values redacted. The
  inherited environment mabo-ctl forwards to children is never sent to a browser.
- **No CORS header is ever set**, so a cross-origin reader cannot see a body
  even on the read-only routes.
- **The Terminal tab is opt-in and loopback-only.** Its routes run interactive
  shells — strictly more than the declared commands — so they exist only under
  `serve --terminal` or `console_terminal: true` (404 otherwise), the terminal
  refuses a non-loopback bind even with `--i-know-this-is-dangerous`, and every
  terminal request must name the bound loopback address in `Host` and in any
  `Origin` it sends, and carry no proxy-forwarding header. An origin trusted
  through `--allow-origin` drives the rest of the console but gets 403 here.
  At most 16 shells are open at once, and they are hung up when serve stops.
  Terminal output is untrusted: the page follows OSC 8 links only for
  http(s), with `noopener`, because a `javascript:` link opened from the
  console would run with the console's session.
- **Known limit of the loopback rule:** a tunnel daemon on this machine that
  rewrites `Host` to the loopback address and strips every forwarding header
  is indistinguishable from a local browser. Do not run `--terminal` behind
  such a tunnel.
- **Framing.** Every response sends `X-Frame-Options: DENY` except the
  authenticated `/api-docs` page, which sends `SAMEORIGIN` and
  `frame-ancestors 'self'` so the console's API tab can frame it. The console
  page itself still sends `frame-ancestors 'none'`.

Audit these against the code (`internal/web/`), not against this list.

### Known, accepted, and documented

- **`mabo-ctl status`, `status --json` and `mabo-ctl health` print credentials that
  appear in a failed probe's health URL.** `/api/status` redacts the same value,
  because it answers any local process; the CLI prints to the operator's own
  terminal, where their own credentials are theirs to see. If you pipe
  `--json` into a shared log, redact it yourself. This is a known, accepted
  limitation rather than an oversight.
- **A health URL's query string reaches the supervised service's own log**,
  because mabo-ctl's probe requests it and the service logs the request line.
  Prefer a credential in a header over one in a query.
- **The start claim trusts `.mabo-ctl/` itself.** Cross-process double spawns ARE
  now prevented — an exclusive `.mabo-ctl/pids/<svc>.pid.claim` taken before any
  spawn; see [`docs/LANDMINES.md`](docs/LANDMINES.md) §9 — but that claim, like
  every pid record, means nothing to someone who can write to `.mabo-ctl/`: anyone
  with write access there can forge any record or hold a claim. Protecting
  `.mabo-ctl/` has always been the boundary.

## Past vulnerabilities

Fixed issues are documented in [`docs/LANDMINES.md`](docs/LANDMINES.md) — each
with the failure it caused and a runnable detector that proves it stays fixed.
Three of them were RCE-class and were found by an internal audit before this
repository was public.
