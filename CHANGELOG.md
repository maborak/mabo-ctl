# Changelog

All notable changes to mabo-ctl are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). The status JSON
contract and service phase names are stable.

## [Unreleased]

### Added

- `console_terminal_scrollback` in `mabo-ctl.yaml` sets how many lines of
  history each Terminal tab shell keeps in the browser (default 10000, the
  previous fixed value; 0 to 100000, no unlimited). History costs about
  columns × lines × 12 bytes per shell, so a lower value bounds the page's
  memory. A config reload says to refresh the page to apply it.
- `mabo-ctl init --from Procfile|process-compose.yaml` imports another
  tool's processes into a live `mabo-ctl.yaml`: commands run through `sh -c`,
  foreman's `$PORT` assignment, and process-compose dependencies, health
  conditions, environment, working directories, `disabled` and HTTP readiness
  probes carry over. Unsafe entries (a `{{` template opener, an invalid env
  key) are written commented out with the reason, every value is quoted so
  repo text cannot inject YAML, and the file is validated before init reports
  success.
- `--port-offset N|auto`, a global flag, moves every declared port by N for
  this checkout, so two git worktrees or clones of one repository can run side
  by side. `auto` picks the first of +100…+1000 where every declared port is
  free. It is saved per checkout in `.mabo-ctl/run.env` (`MABO_PORT_OFFSET`),
  ranks below every explicit port level, and `config` reports those ports as
  `from offset`. A refused start for a held port now suggests it.
- The full-screen console (TUI) gains the web console's log and service
  controls: merged all-services log (`m`), pause (`p`) and clear (`c`), a
  level filter (`v`) using the same JSON level classification as the browser,
  pretty JSON (`J`), separators between entries (`|`), a detail block for the
  selected service (`i`), list filters by name (`f`) and phase (`1`-`7`, `0`),
  an activity history (`e`), a configuration view with port sources and start
  order (`C`), and restart of every running service (`R`). Health URLs are
  redacted on screen as they are in the browser.
- `mabo-ctl config --graph` prints the start order: one row per dependency
  level, each service with what it waits for. `config --json` and
  `GET /api/config` carry it as `levels`, and the web Config tab shows a
  "start order" table. All three come from the same levelling `start` uses.
- **Pretty JSON** checkbox in the web console's log toolbar. It indents log
  lines that are complete JSON objects or arrays by changing whitespace only,
  so big integers, duplicate keys and key order show exactly as written. Lines
  cut by the display limit stay raw, the text filter still matches the
  original line, the choice is remembered per browser, and VIEW share links
  mirror it.

### Changed

- The TUI's `a` (start all) now starts every service by name, autostart or
  not, like the web console's Start all; it used to start only the autostart
  selection.

### Fixed

- `reset --force` killed the other checkout's service. Two worktrees or clones
  of one repository declare the same ports, so the "foreign" listener `reset
  --force` reaped on a declared port could be the same service that another
  checkout's mabo-ctl started and still records. Reset now finds the holder's
  nearest `.mabo-ctl`, and when that checkout's pid records claim the holder or
  its process group, it names the checkout and leaves the process alone.

- Bound the browser's pending log-render queue as well as displayed history.
  Background tabs no longer retain every incoming line while animation frames
  are suspended. Log previews are limited to 2,000 lines and 2 Mi UTF-16 code
  units, with explicit display-only truncation for lines over 16 Ki code units.
  Raw service log files remain unchanged.
- Filtering, clearing and resuming logs cancel stale paints instead of replaying
  pending records twice. DOM pruning follows the retained text budget even when
  new lines do not match the active filter.
- Bound each terminal's unparsed browser output at 2 MiB. An overloaded display
  detaches and, when visible and drained, reloads recent server scrollback.
  Recovery explicitly warns that older terminal output may be omitted; the
  shell itself keeps running and replay cannot generate shell input.
- A paused terminal display now recovers when the page becomes visible again
  or the Terminal tab is reopened, not only after a click on that shell's tab.
  Pause and recovery notes name the shell they refer to.
- Log tailing bounds every line at 256 KiB at the source: backlog, live
  follow and failure excerpts alike. A newline-less progress bar or dump no
  longer grows `serve` (or the TUI, or `logs -f`) by re-reading the partial
  line every poll. One oversized line no longer makes a service's stream fail
  with `bufio.Scanner: token too long` and reconnect forever. Truncated lines
  carry an explicit marker; the log file is unchanged. A trailing `\r` is now
  stripped from live lines as it already was from backlog lines.
- The history a log tail replays is capped at 8 MiB per request, on top of
  its line count. 5000 lines of up to 256 KiB each could previously make one
  `?tail=` request (from any console or shared link) read over a gigabyte
  into memory. Lines dropped by the byte cap are replaced by one marker
  saying how many were omitted.
- The TUI's log buffer is capped at 4 MiB as well as 5000 lines; with 256 KiB
  lines the line count alone allowed over a gigabyte.
- Log streams in the web console, the TUI and `logs -f` buffer at most 16
  lines between the tailer and a slow reader (was 256, and 128 per service
  in `logs -f`), and merged all-services streams no longer buffer per
  service. With 256 KiB lines, one slow reader could pin 64 MiB per stream,
  and a merged stream that much again per service; now about 4 MiB per
  stream plus, per service, about three lines and a 64 KiB read buffer.
  Nothing is dropped: the tailer reads a file and simply waits.
- Removed private project names from public docs and examples; links to an
  unpublished roadmap are gone, and `docs/SPEC.md` is marked as the
  historical original spec.

## [1.4.0] - 2026-11-11

### Added

- Explicit remote-terminal permission with
  `serve --terminal --i-know-allow-remote-terminal-is-dangerous`. Non-loopback
  binds also require `--i-know-this-is-dangerous`; HTTPS tunnels additionally
  require their public origin in `--allow-origin`.
- A live connection indicator showing status-response latency, received
  application payload rate, reply freshness, and slow/disconnected states.
- A close control for the service-output panel below the terminal. Selecting a
  service in the sidebar reopens the panel without closing any shell.
- An editable same-origin API base URL, defaulting to the browser's origin so
  the playground works through HTTPS tunnels.

### Changed

- A non-loopback console keeps running with the Terminal box disabled when
  remote-terminal permission is missing, rather than refusing to start.
- Token/Copy lives in Config; theme selection and expandable keyboard help
  occupy a compact sidebar footer.
- API authentication failures return concise JSON
  `{"error":"invalid_api_key"}`. Role denials return `{"error":"forbidden"}`;
  HTTP status codes remain unchanged.
- Log separators mark recognizable multiline entries rather than every
  physical line. Python tracebacks, chained exceptions and indented
  continuations stay together; separate JSON records remain separate. Raw
  logs, line counts and severity filtering are unchanged. Plain-text grouping
  is best effort because the stream does not preserve logging-call boundaries.

### Fixed

- Remote-terminal permission now admits explicitly trusted HTTPS tunnels while
  retaining Host/Origin checks and operator-only access.
- The API playground initializes one editable header-key control from the UI
  session, avoiding duplicate token headers. Clearing or replacing the key
  sticks, and theme changes no longer reload the API frame or discard edits.
- API frames use the UI page's explicit credential instead of an ambient cookie
  that another browser tab may have replaced with a different access role.
- VIEW/mirror guests cannot override presenter scrolling with wheel, touch,
  keyboard or scrollbar input. READ/WRITE guests still browse independently.
- Following a log before its first creation no longer drops startup output.
  Backlog replay and live following now share one file handle and an exact
  consumed offset, preserving concurrent writes and incomplete final lines.

### Security

- Terminals remain loopback-only by default and unavailable to every shared-link
  role. Neither YAML terminal settings nor dangerous-bind or trusted-origin
  settings alone authorize remote shells. Proxy headers never establish trust.
- API base-URL edits remain same-origin; the playground never forwards the
  current session key to another origin or persists it to local storage.

### Upgrading

- Rebuild or install the release, restart `serve`, and reload browser tabs.
  Finish terminal work first: restarting closes shell sessions and invalidates
  shared links, while managed services keep running.
- OpenAPI uses one `tokenHeader` security scheme for reads and mutations. Clients
  configured with the former `sessionHeader` scheme name should use
  `tokenHeader`; the `X-Mabo-Ctl-Token` HTTP header is unchanged.
- API clients parsing authentication errors should expect the JSON bodies above
  instead of the previous plain-text login instructions.

## [1.3.0] - 2026-11-01

### Added

- Configuration change detection and confirmed reloads from the web console.
- Opt-in web terminals with persistent shell sessions, scrollback and multiple
  tabs. Enable with `serve --terminal` or `console_terminal: true`.
- Scoped READ ONLY, VIEW and WRITE sharing links, operator-token rotation,
  guest-session monitoring and link revocation.
- VIEW synchronization for service selection, tabs, log controls, cursor and
  click indicators. Clicks are displayed, never replayed as input.
- Restart all for running services, log separators and font-size controls.

### Changed

- Tabbed web workspace with Monaco fonts, themes, a collapsible/resizable
  sidebar and a compact service console below the resizable terminal pane.
- CONFIG sections for Overview, Services, Sharing & guests, and Security.
- Separate System Commands history from service output.

### Fixed

- Stalled status requests and streams after sleep or network interruptions.
- Duplicate lifecycle messages and repeated log tails for failed services.
- Stale service-action buttons and unclear severity-filter selection.
- Terminal scrollback generating shell input, clipped terminal grids, duplicate
  initial shells during rapid tab changes and duplicate client attachments.
- Session tracking when multiple shared links are open in one browser.

### Security

- Build with Go 1.26.10 to address standard-library security advisories.
- Guest permissions are enforced by a server-side route allowlist. WRITE permits
  service lifecycle controls only; guests cannot access terminals or administer
  configuration, origins, tokens or shares.
- Revocation closes shared streams. Operator-token rotation invalidates all
  shares. Invalid explicit credentials cannot fall back to an operator cookie.
- Terminal access remains opt-in and loopback-only. VIEW excludes terminal
  contents, credentials and administrative settings.

### Upgrading

- Rebuild or install the release, then restart `serve` and reload browser tabs.
  Restarting `serve` closes web terminal sessions and invalidates shared links;
  finish terminal work first. Managed services keep running.
- Guest links, session history and token changes made in CONFIG are runtime-only.

## Earlier releases (through v1.2.3)

### Changed

- Renamed the repository state directory from `.dev/` to `.mabo-ctl/`, including
  logs, PID records, sockets, and persisted ports. Stop services with the old
  binary before upgrading, rename the directory, and update `.gitignore`;
  migration is manual.

### Added

- **`console_addr` in `mabo-ctl.yaml`** — the address `mabo-ctl serve` binds
  when no `--addr` is given (default `127.0.0.1:7999`), e.g.
  `console_addr: "127.0.0.1:9000"`. An explicit `--addr` still wins; a taken
  explicit port still fails loudly rather than silently moving.
- **`GET /health`** — unauthenticated server liveness check on the web console.
  Returns `{"status": "ok"}` with a 200 status when the server is running. It is
  the only route that does not require a session token, designed for monitoring
  tools, load balancers and CI probes.
- **`GET /api-docs`** — interactive API reference page rendered by a **vendored
  RapiDoc** bundle (MIT, served at `/api-docs/rapidoc.js`) — a full API browser
  with a working **try-it playground** on every operation. The session token
  is wired in automatically as the `X-Mabo-Ctl-Token` api-key, so reads and
  mutations from the page authenticate exactly like the console buttons; the
  try-it server is pinned to the address actually bound. No CDN, no external
  requests.
- **`GET /api/openapi.yaml`** — the OpenAPI 3.0 specification in YAML, served
  from the binary. Requires a valid session.
- **`docs/API.md`** — comprehensive HTTP API reference for the web console:
  every route, authentication model, request/response shapes, SSE stream
  formats, error handling, and curl examples. The machine-readable catalogue
  (`mabo-ctl schema --commands`) is now paired with a human-readable companion.
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
  to overwrite, adds `.mabo-ctl/` to `.gitignore`, runs nothing it finds.
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
  or recycled pids, foreign port holders, unsurfaced crashes, loose `.mabo-ctl/`
  permissions. Warn exits 0, FAIL exits 1; it changes nothing.
- **JSON Schema for `mabo-ctl.yaml`**, shipped at `schema/mabo-ctl.schema.json`
  and printable via `mabo-ctl schema`. Editor-friendly: the parser accepts a
  `$schema:` key it ignores, and the schema is drift-guarded against the
  shipped example by a test.
- **fish and powershell completions**, alongside bash and zsh.
- **The previous run's log is kept** as `<svc>.log.1` — restarting no longer
  destroys the evidence you restarted to read. One generation, capped on
  purpose; `reset` deletes it like everything else under `.mabo-ctl/`.
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

### Changed

- **The web console keeps declaration order.** Rows used to re-sort by phase —
  a failing service jumped to the top, a ready one sank to the bottom — which
  made the list impossible to read by position across a start/stop. Rows now
  stay exactly where mabo-ctl.yaml declares them; attention is carried by the
  glyph, the colour and the summary tiles, as it always was elsewhere.

### Fixed

- **Every log follower went silent after a start or restart.** The
  keep-one-generation rotation renamed `<svc>.log` aside and created a fresh
  file at the same path, while web panes, TUI panes and `logs -f` kept reading
  the old inode at EOF. Followers now detect the replacement by inode identity
  against the path and resume from the top of the new file.
- **Two mabo-ctl processes could start the same service.** The per-service
  mutex serialises only inside one process; a second terminal passed the same
  already-running check before either pid file existed, and two copies ran —
  for a portless service, unreachably, because the pid record named only the
  later one. Starts now take an exclusive `.mabo-ctl/pids/<svc>.pid.claim` first
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
- **Concurrent writes to `.mabo-ctl/run.env` lost data** — 7 of 8 foreign keys under
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
