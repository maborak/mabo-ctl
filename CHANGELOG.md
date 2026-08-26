# Changelog

All notable changes to mabo-ctl are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
mabo-ctl aims at [Semantic Versioning](https://semver.org/spec/v2.0.0.html). It is
**pre-1.0**: the `--json` status contract and the phase set are treated as
stable, everything else may still move.

## [Unreleased]

### Added

- **`tty: true` makes a service attachable** — the child runs on a pty owned
  by a detached broker that tees its output into the normal log, and
  `mabo-ctl attach <name>` connects your terminal to it: Ctrl-Q detaches,
  one session at a time, detachment never surrendered. Linux ships full pty
  support; darwin declines honestly at start for now.
- **Preflight machine-readiness** — `preflight` front-runs "can THIS machine
  run what mabo-ctl.yaml declares?" per service (runtime resolution,
  cmd[0] executability, node_modules presence under node:, .nvmrc agreement —
  the last two advisory), diagnose-and-hint, never fix. Status surfaces the
  same signal so an unrunnable service stops reading as bare `stopped`.
- **`expect: free|listening` on tcp checks** — flip the dial to demand an
  empty port before start; failures name whoever holds it.
- **`logs --timestamps`** — opt-in HH:MM:SS.mmm stamps on follows, stamped at
  READ time and honest about it: historical tails refuse the flag.
- **The surface drift gate** — `tools/surfacemap` enumerates every CLI
  command+flag, config schema field and status --json key from the built
  binary into a committed map, and a package test fails on either direction of
  drift until it is regenerated. 148 surfaces mapped today.
- **Readiness probes beyond HTTP** — `health:` now also accepts a mapping:
  `{tcp: host:port}` (a dial; connected is ready, nothing is ever written to
  the socket) or `{exec: [argv]}` (run once per poll in the service's working
  directory and environment, under a hard timeout; exit 0 is ready). A scalar
  value keeps parsing as an http(s) URL byte for byte. This is what makes
  `slow`/`degraded`/`failed` honest for databases, queue consumers and gRPC
  daemons — every service without an HTTP endpoint.
- **`open:` per service** — the URL or path `mabo-ctl open` prefers over the
  derived origin: `/docs` joins against the service's origin, an absolute
  http(s) URL opens as-is. Non-http schemes are refused before the platform
  opener can turn one into a handler launch.
- **Named port overrides** — `--port <svc>=<n>`, repeatable, outranking every
  other precedence level including `--ports`; combining the two spellings is a
  usage error rather than an accidental merge. A ported child also receives its
  resolved port as a bare `PORT` (the Procfile/Heroku convention), gated on
  declaring a port.
- **Full build provenance behind `--version`** — commit, dirty flag, link time,
  toolchain, platform and dependencies in one pastable block; unstamped source
  builds fall back to the toolchain's embedded VCS state so an upgrade can
  never mistake one for a release tag.
- **`mabo-ctl init`** — scaffolds a fully commented-out `mabo-ctl.yaml` from
  what the repo looks like (`package.json` + `.nvmrc`, `manage.py`,
  `pyproject.toml`, `Cargo.toml`). Every guess lands commented with the
  evidence that produced it; nothing runs until a human uncomments it. Refuses
  to overwrite, adds `.dev/` to `.gitignore`, runs nothing it finds.
- **Desktop death notifications** — `--notify`, on any resident front end and
  on `serve`: a dying service announces itself via `osascript`/`notify-send`
  with its name and a truncated log line. The watcher reads on-disk exit
  records, so deaths that happened while nobody was resident are still reported
  by the next session. Off by default.
- **`mabo-ctl schema --commands`** — a machine-readable catalogue of the binary
  itself: every command's flags and argument semantics, which commands mutate
  state, side effects, the exit-code table, which outputs are stable machine
  contracts, and every web-console HTTP route with its authentication level.
  Generated live from the same cobra tree `--help` renders; a new subcommand
  without catalogue metadata fails loudly instead of shipping undocumented.
- **`mabo-ctl doctor`** — a read-only stack exam: unresolvable runtimes, stale
  or recycled pids, foreign port holders, unsurfaced crashes, loose `.dev/`
  permissions. Warn exits 0, FAIL exits 1; it changes nothing.
- **JSON Schema for `mabo-ctl.yaml`**, shipped at `schema/mabo-ctl.schema.json`
  and printable via `mabo-ctl schema`. Editor-friendly: the parser accepts a
  `$schema:` key it ignores, and the schema is drift-guarded against the
  shipped example by a test.
- **fish and powershell completions**, alongside bash and zsh.
- **The previous run's log is kept** as `<svc>.log.1` — restarting no longer
  destroys the evidence you restarted to read. One generation, capped on
  purpose; `reset` deletes it like everything else under `.dev/`.
- **Per-service `ready_timeout:`** — one slow-warming service sets its own
  window instead of forcing the whole stack to call everything slow for two
  minutes; absent means inherit the global.
- **`env_file:` per service** — a strict `KEY=VALUE` file anchored at the repo
  root; the inline `env:` map overrides it key by key, compose semantics like
  docker-compose. Re-read at every resolve, so editing it takes effect on the
  next start without touching `mabo-ctl.yaml`.
- **Web console all-services log view** — one merged stream of every log with
  cross-service text search, and a real All toggle beside each service.
- **Trusted browser origins** — `--allow-origin`, accepting a host or a
  `*.`-subdomain pattern, editable while mabo-ctl runs from the console's
  Configuration panel. Needed when the console is reached through a tunnel or a
  port forward, where the browser's origin can never match the bound address.
- **Web console** (`mabo-ctl serve`) — one embedded page listing every service
  with its phase, pid, port, resolved command, working directory and a live log
  stream, plus start/stop/restart. Loopback-bound and token-guarded.
- **Interactive prompt and full-screen console** — `mabo-ctl` with no arguments
  on a terminal, or `mabo-ctl start --interactive` / `--attach` /
  `--web-console`.
- **`mabo-ctl config`** — where `mabo-ctl.yaml` was loaded from and what it
  resolved to: the port *and which of the five precedence levels produced it*,
  the absolute `cmd[0]` and the runtime that chose it, the expanded health URL,
  and the declared environment with credential-shaped values redacted.
- **`exited` and `degraded` phases** — a service that crashed is no longer
  reported identically to one that was never started, and a service that has
  been silent past `ready_timeout` is no longer called "still starting".
- **`--all` on `stop` and `restart`**, which only `start` had.
- **Parallel start** — independent services start level by level rather than
  serially (11.0s → 3.96s on a five-service stack).

### Fixed

- **Two mabo-ctl processes could start the same service.** The per-service
  mutex serialises only inside one process; a second terminal passed the same
  already-running check before either pid file existed, and two copies ran —
  for a portless service, unreachably, because the pid record named only the
  later one. Starts now take an exclusive `.dev/pids/<svc>.pid.claim` first
  (`ErrClaimed` refuses a fresh rival; stale wreckage is cleared automatically).
  See [LANDMINES.md §9](docs/LANDMINES.md).
- **Port-conflict errors stopped at diagnosis.** They now carry the remedy as
  well as the culprit: the `lsof` line plus the `<NAME>_PORT=…` override form,
  and `serve`'s own bind refusal names the holder and suggests
  `--addr 127.0.0.1:0` when another console holds the port.
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
