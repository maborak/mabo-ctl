# Changelog

All notable changes to mabo-ctl are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
mabo-ctl aims at [Semantic Versioning](https://semver.org/spec/v2.0.0.html). It is
**pre-1.0**: the `--json` status contract and the phase set are treated as
stable, everything else may still move.

## [Unreleased]

### Added

- **Web console** (`mabo-ctl serve`) — one embedded page listing every service
  with its phase, pid, port, resolved command, working directory and a live log
  stream, plus start/stop/restart. Loopback-bound and token-guarded.
- **Interactive prompt and full-screen console** — `mabo-ctl` with no arguments on
  a terminal, or `mabo-ctl start --interactive` / `--attach` / `--web-console`.
- **`mabo-ctl config`** — where `mabo-ctl.yaml` was loaded from and what it resolved
  to: the port *and which of the four precedence levels produced it*, the
  absolute `cmd[0]` and the runtime that chose it, the expanded health URL, and
  the declared environment with credential-shaped values redacted.
- **`exited` and `degraded` phases** — a service that crashed is no longer
  reported identically to one that was never started, and a service that has
  been silent past `ready_timeout` is no longer called "still starting".
- **Trusted browser origins** — `--allow-origin`, accepting a host or a
  `*.`-subdomain pattern, editable while mabo-ctl runs from the console's
  Configuration panel. Needed when the console is reached through a tunnel or a
  port forward, where the browser's origin can never match the bound address.
- **`--all` on `stop` and `restart`**, which only `start` had.
- **Parallel start** — independent services start level by level rather than
  serially (11.0s → 3.96s on a five-service stack).

### Fixed

- **The console page served its own session token to unauthenticated callers**,
  handing out the credential that guarded every mutating route. Reads and the
  page are now session-gated; mutations still require the token in a header, so
  CSRF cover is unchanged.
- **Config discovery walked to the filesystem root** and would load and execute
  a `mabo-ctl.yaml` from an unrelated parent directory, naming it nowhere. The
  walk now stops at a repository marker or `$HOME`, and the loaded path is
  announced whenever it came from outside the working directory.
- **A stray `<NAME>_PORT` invented a port for a portless service**, which then
  told `reset --force` to kill whatever held it.
- **Health-URL credentials reached the event stream** — redacted on
  `/api/status`, quoted verbatim into the slow/degraded events that travel to
  the SSE stream and every mutation response. Redaction moved to where the
  string is built.
- **`reset` could kill a service mabo-ctl had just started**, treating it as a
  foreign orphan, and then delete the record proving it existed.
- **Concurrent writes to `.dev/run.env` lost data** — 7 of 8 foreign keys under
  8 writers.
- **A stale pid file wedged a service permanently** — liveness is not ownership.
- **`mabo-ctl start` returned success when every service failed.**

Full detail, with a runnable detector for each, is in
[`docs/LANDMINES.md`](docs/LANDMINES.md).

### Security

- Toolchain moved to Go 1.26.6, clearing six standard-library advisories.
- See [SECURITY.md](SECURITY.md) for the trust boundary and the web console's
  controls.

---

mabo-ctl replaces `dev.sh`, 1,081 lines of bash duplicated across two repositories
and already drifted between them. That drift is why it exists.
