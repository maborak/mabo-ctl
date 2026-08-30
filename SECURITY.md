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

Expect an acknowledgement within a week. There is no bounty; this is a
single-maintainer hobby-scale project, and saying so up front is more honest
than implying an SLA that will not be met.

## Supported versions

The `main` branch. mabo-ctl is pre-1.0 and there are no backported fixes — if a
fix lands, it lands on `main`.

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
| Path handling of `name` | a service name escaping `.dev/` via `/` or `..` |
| Signal blast radius | mabo-ctl signalling a process it did not start |
| Secret handling | credentials reaching a log, `--json`, or an HTTP response that a reader would not expect |
| **The `mabo-ctl serve` web console** | any bypass of its token, `Host`/`Origin`, or method checks |

### `mabo-ctl serve` deserves particular attention

It is the only surface something other than the developer's keyboard can reach.
Its POST routes call the same supervisor `start`/`stop`/`restart` the CLI calls,
so **a bypass of any of its controls is remote code execution as the developer.**

The controls, as of the current `main`:

- **Loopback bind by default** (`127.0.0.1:7999`). A non-loopback `--addr`
  requires `--i-know-this-is-dangerous` and exits `2` without it.
- **A per-run session token**, 32 random bytes. Required as the
  `X-Mabo-Ctl-Token` **header** on every mutation — never a cookie, never a query
  parameter, because both of those ride a cross-origin request and a custom
  header cannot without a CORS preflight this server never answers.
- **The console page and read routes** accept the token from the query, a
  `SameSite=Strict` `HttpOnly` cookie, or the header — a browser navigation
  cannot send a header and an `EventSource` cannot either. A browser with no
  token gets a box to paste one into, and that page contains no token.
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
  `*.`-subdomain patterns, never a bare `*` without the danger flag.
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
- **The start claim trusts `.dev/` itself.** Cross-process double spawns ARE
  now prevented — an exclusive `.dev/pids/<svc>.pid.claim` taken before any
  spawn; see [`docs/LANDMINES.md`](docs/LANDMINES.md) §9 — but that claim, like
  every pid record, means nothing to someone who can write to `.dev/`: anyone
  with write access there can forge any record or hold a claim. Protecting
  `.dev/` has always been the boundary.

## Past vulnerabilities

Fixed issues are documented in [`docs/LANDMINES.md`](docs/LANDMINES.md) — each
with the failure it caused and a runnable detector that proves it stays fixed.
Three of them were RCE-class and were found by an internal audit before this
repository was public.
