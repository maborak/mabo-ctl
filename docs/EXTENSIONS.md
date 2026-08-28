# EXTENSIONS — shipped extensions and their contracts

> Companion to `docs/SPEC.md` (spec of record) and `docs/ROADMAP.md`. This
> file documents the eight extension candidates that were audited, what was
> already built in, what was added, and the invariant each one had to respect.
> When SPEC.md and this file disagree, SPEC.md wins.

Audited 2026-08-27. Every claim below was verified against the live tree, not
carried over from earlier planning notes — three of the eight turned out to
already exist, which is recorded here so the next audit does not re-plan them.

---

## Already built in (no new work; recorded to prevent re-planning)

### Crash summaries — status, JSON, and the console already explain a death

`Status` already carries `ExitCode`, `ExitSignal`, `ExitedAt`
(internal/supervisor/supervisor.go, `type Status struct`), filled by
`describeExit` from the persisted `state.ExitRecord` (internal/state/exit.go).
`exitDetail` renders the one-line why — signal, exit code, recency — plus the
log tail underneath. The machine contract exposes `exit_code` / `exit_signal`
/ `exited_at` through `ui.StatusJSON`, and the web console renders `detail`
in its detail pane. A deliberate stop (`stopped: true`) never reads as a
crash; that is the load-bearing LANDMINES rule this path exists to honour.

### Per-service `env_file`

`Spec.EnvFile` (`env_file:` in mabo-ctl.yaml) with `Spec.EnvFilePath`
anchoring it at the repo root, `env:` overriding key by key. Validated at
load, re-read at every resolve.

### The console log pane

`GET /api/stream/{svc}` (and `/api/stream/all`) already drives an
EventSource-based log pane in the embedded page, with `?tail=` replay on
open. What the audit added is the phase history below, not the log pane.

---

## Shipped in this change set

### 1. `health --wait [--timeout DURATION]`

Waits until every service that declares a health URL reports phase `ready`,
then reprints the status block. Semantics:

- Services **without** a declared health URL are never waited on — there is
  nothing to observe changing, and holding the start of the world open on a
  portless worker would be a bug, not a feature.
- A service that reaches `failed` stops being "pending": the wait ends and
  the exit check reports it as not ready, rather than timing out on a
  service that already answered with bad news.
- `--timeout` bounds the wait; expiry exits with the not-ready code naming
  who was still down. `--timeout` without `--wait` is a usage error.
- Poll interval is a fixed constant (`healthWaitInterval`), because the
  supervisor runs the probes and the flag only bounds staleness.

Exit codes are unchanged: `0` all answered, `4` someone did not (or the
deadline passed). Layering: `cmd`-only change; `waitReady` consumes the
supervisor's own `Status`, so it derives nothing on its own.

### 2. `logs` — multiple services, `--since`, `--grep`

- `logs a b c` tails several services with per-service prefixes (the
  single-service verbatim path is preserved); `logs`/`logs all` still means
  every service.
- `--grep S` is a plain substring filter over line bodies, applied to both
  historical and followed output, before any prefix/timestamp decoration.
  Not a regex: the value is grep-shaped output, not a pattern language.
- `--since D` bounds the HISTORICAL tail by file mtime and is therefore
  historical-only: `-f --since` is a usage error, because a read-time window
  over a live stream cannot honestly bound which replayed lines belong to
  the window. Log lines carry no timestamps (the file is plain text), so the
  honest granularity is the file's mtime — coarse, but never invented. Each
  skipped service gets a stderr note; stdout stays parseable. A service with
  no log yet counts as stale.

The freshness gate reads `Status.LogPath` through `StatusNoPorts` (no probe
runs because you asked for logs) and stats the file from `cmd` — a read-only
peek at a path the supervisor itself vouches for; `.dev/` writing stays
exclusively in `internal/state`.

### 3. Lifecycle hooks (`hooks:` per service)

```yaml
services:
  - name: api
    cmd: [npm, run, dev]
    hooks:
      pre_start:  [./.scripts/wait-for-env.sh]
      post_start: [./.scripts/warm-cache.sh]
      pre_stop:   [./.scripts/drain.sh]
      post_stop:  [./.scripts/report.sh]
```

Contract:

- A hook is an **argv**, run directly — no shell, same rule and same reason
  as `cmd`. Same working directory, same resolved environment (including
  ports) as the service it hangs off.
- `pre_start` runs after the start claim is taken and the log truncated, but
  before the service spawns. **Failing it fails the start**: the deferred
  claim release runs, an exit record with `startup: true` is written (so the
  phase reads `failed`, and the reason is visible after this process is
  gone), and the service never spawns.
- `post_start` runs once the service is settled (ready, or running with no
  probe). **Best-effort**: a failing hook is reported as an event and into
  the log, and never reads as the service having failed.
- `pre_stop` runs before any signal, bounded by the stop grace. **It can
  never block the stop**: a failure is recorded and the signals proceed. A
  service that refuses to die is a supervisor finding, not a hook opinion.
- `post_stop` runs after the process group is confirmed dead, on both the
  graceful and SIGKILL paths. Best-effort.
- Hook stdout/stderr goes into the service's own log through
  `state.OpenLogAppend` — one redaction point (the source), one file to read
  after a mystery.
- Hooks honour the run's context: they die with the mabo-ctl invocation that
  ran them rather than outliving it.

Validation treats present-but-empty hooks as unrepresentable (empty program
or empty argument is a config error), mirroring the `cmd` rule. The schema
(`schema`) documents each hook.

### 4. Readiness-gated dependencies (`depends_ready_on`)

```yaml
services:
  - name: api
    cmd: [npm, run, dev]
    depends_on: [db]
    depends_ready_on: [db]
```

`depends_on` keeps its contract — it orders **starts only**, never stops,
and a plain edge starts its dependant as soon as the dependency is spawned.
`depends_ready_on` is the opt-in readiness gate on top, with three binding
rules enforced at load:

1. every `depends_ready_on` name must be a declared service;
2. every name must **also** appear in `depends_on` — a gate orders nothing
   by itself, so gating without an ordering edge is a config error, not a
   surprise at start time;
3. the gated dependency must declare a health probe — a probe-less service
   goes `running`, never `ready`, so a gate on one could only ever block
   forever.

At start time, the existing per-level fold gains an `unready` map: services
that settled at `slow`, `degraded`, or `running`-without-probe are recorded
there, and the next level's services with gated edges on them are skipped
with an explicit "gates on its readiness" message. The safety argument is
unchanged and load-bearing: both maps are written only **between** levels,
never during one, because every dependency of a level-N service is final by
the time level N is scheduled (the same reasoning the `failed` map always
had). Stops never consult gates.

### 5. Profiles (`profiles:` + `--profile` / `MABO_PROFILE`)

```yaml
services:
  - name: mailpit
    cmd: [mailpit]
    profiles: [mail]
  - name: api
    cmd: [npm, run, dev]      # no profiles: — always active
```

- The active set is `--profile a,b` (comma-separated), else `MABO_PROFILE`,
  else the empty set. The flag beats the env.
- An absent or empty `profiles:` list is **always active** — the pre-profiles
  behaviour, byte for byte, for every config that does not mention profiles.
- A service with a list is in the run only if a name overlaps the active
  set. Filtering happens once, at the single load choke point, so every
  command — start, status, config, logs, the console — sees the same run.
- Activating a profile set that excludes **every** declared service is a
  config error (exit 3) naming the declared profiles: a typo in
  MABO_PROFILE must not read as "everything filtered out happened to be
  fine".
- Empty entries in the flag/env are dropped (`a,, b` == `a,b`); empty
  entries in a service's `profiles:` list are a validation error, because a
  name that can never be activated is a service that silently never runs.

Deliberately out of scope for this iteration: rendering inactive-but-
declared services as a distinct class in `config` output, and file
`include:` (the include path re-opens the discovery trust boundary and needs
its own audit — see `docs/LANDMINES.md` §2 before anyone reaches for it).

### 6. Phase history — `GET /api/history` + console strip

The web console keeps a bounded **in-memory** ring (50 events) of lifecycle
events, appended by the same broker that feeds the event stream, and serves
it at `GET /api/history` in the exact event shape every other console
response uses (`{events: [{service, phase, msg, error?}]}`, oldest first).
The route lives in `consoleRoutes` like every other route, is guarded as a
read (session, not token), and never touches supervisor state. The embedded
page renders it in a "history" strip, polling rather than streaming on
purpose — history is context, not a second realtime channel. The ring is
deliberately not persisted: it is a dev console's "what just happened", and
`.dev/` writes belong to `internal/state`.

---

## Invariants every one of these had to respect

All verified in this change set, all still binding:

- The drift gate owns the surfaces. `go run ./tools/surfacemap` was run after
  the cli (`--wait`, `--timeout`, `--since`, `--grep`, `--profile`), config
  (`hooks`, `depends_ready_on`, `profiles`) and json inventories changed;
  `internal/surface/surfaces.json` is committed with the same change.
- The probe set stays closed: `depends_ready_on` *consumes* readiness, it
  does not add a probe kind.
- The phase set stays closed: no new phases; gating maps over the existing
  ones.
- Stops stay exact-name; `depends_on` and gates order starts only.
- A deliberate stop still never reads as a crash — hooks cannot turn a stop
  into a failure, and `pre_stop` cannot block a stop.
- All output channels redact at the source; hooks write through the same
  log the service writes through.
- Writes under `.dev/` come only from `internal/state` (the one new writer
  there is `OpenLogAppend`, append-only beside `TruncateLog`, and the state
  package's log-writer checks in `state_test.go` cover both paths).
- Web mutations stay POST-only and token-gated; the new endpoint is
  read-only and session-guarded.

## Verification

`go build ./... && go vet ./... && go test ./... -race -cover`,
`golangci-lint run`, `gofmt -l .` clean at time of writing, with new tests:
cmd-level (`inspect_test.go`, `profiles_test.go`), supervisor-level
(`hooks_test.go`: pre-start refusal + exit record, best-effort post-start,
stop hooks cannot block, gated skip/pass), config validation cases,
state (`OpenLogAppend`), and web (`history_test.go`: shape, ring bound,
page consumption pin).
