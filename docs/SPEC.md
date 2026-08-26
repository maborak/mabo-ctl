# mabo-ctl — porting `dev.sh` to a standalone Go supervisor

**Status:** spec, not started. Written 2026-08-15 so the shell version can be
retired rather than reverse-engineered later.

**Source:** ``dev.sh` in the operator's application repository`
— 1,081 lines of bash, 48 functions. A sibling exists at
`a second copy of `dev.sh` in a sibling repository` (~29 KB) which has already
diverged: it carries a persisted-port cache and a `--ports` flag with different
precedence. **Two copies of this tool exist and they have already drifted.**
That is the actual reason to extract it.

---

## What it is

A foreground-free process supervisor for a local polyglot dev stack. It starts,
stops, restarts, health-checks, tails and inspects N long-running services, each
with its own working directory, runtime, environment and port — and it keeps
enough state on disk that a second invocation in a different terminal knows what
the first one started.

It is `supervisorctl` shaped, but per-repo, zero-install, and with the service
registry compiled in rather than configured.

## Why move it to Go

1. **It is duplicated and drifting.** Two repos, two versions, same job.
2. **The registry is a `case` statement repeated seven times.** Adding a service
   means editing seven `case` blocks in lockstep — `svc_label`, `svc_color`,
   `svc_dir`, `svc_port`, `svc_health_url`, `svc_needs_conda`, `is_service`,
   `svc_exec`. Miss one and you get a silent, confusing failure (see Bug 2).
3. **Bash makes the failure modes bad.** A missing `case` branch exits 0. An
   unbound variable under `set +u` is empty string. Both produce a supervisor
   that reports something untrue.
4. **A single static binary** drops the conda/nvm PATH problem, the
   `bash -n`-only validation, and the shell-completion generator.

## Non-goals

- Not a production supervisor. No restart-on-crash policy, no resource limits,
  no cgroups. `watch` exists to notify a human, not to self-heal.
- Not a container orchestrator. These are host processes on a developer laptop.
- Not a task runner. It does not build, test or migrate.

---

## Command surface (what the Go version must keep)

```
mabo-ctl [start] [services...] [-f] [--ports=A,B,C,D] [--with-browser] [--with-worker] [--all]
mabo-ctl stop    [services...]
mabo-ctl restart [services...] [-f]
mabo-ctl status  [--json]
mabo-ctl health                       parallel health checks, all services
mabo-ctl tailf   [svc|all] [--tail=N] follow (or last N) logs
mabo-ctl logs    [svc|all] [--tail=N] alias for tailf
mabo-ctl open                         open running URLs in the default browser
mabo-ctl psql                         open psql/sqlite3 against the dev DB
mabo-ctl reset                        stop, kill orphans, clear state dir
mabo-ctl preflight                    check Postgres + Redis reachability
mabo-ctl exec  <svc> <cmd> [args...]  run a command in the service's env
mabo-ctl shell <svc>                  interactive shell in the service's env
mabo-ctl watch                        foreground crash watcher with notifications
mabo-ctl completion <bash|zsh>
mabo-ctl <svc>                        shorthand for `mabo-ctl start <svc>`
```

`--json` on `status` is the integration point; keep it and make it the stable
contract.

## Service model

The shell version hardcodes five services. The Go version should read them from
a declarative file (`mabo-ctl.yaml` at repo root) so the binary is repo-agnostic —
that is the whole point of extracting it.

```yaml
services:
  - name: website
    dir: website
    port: 7100
    health: http://localhost:{{.Port}}/robots.txt
    cmd: [npm, run, dev, --, --port, "{{.Port}}"]
    env:
      PUBLIC_API_BASE: http://localhost:{{.Port "backend"}}
    color: green
  - name: backend
    dir: backend
    port: 7102
    health: http://localhost:{{.Port}}/health
    runtime: conda            # activate CONDA_ENV before exec
    cmd: [uvicorn, "api_main:app", --port, "{{.Port}}", --reload]
  - name: worker
    dir: backend
    runtime: conda
    cmd: [python, cli.py, monitor, run]
    depends_on: [backend]     # no port, no health check
```

Current registry, for reference (the 7100 port convention):

| service  | dir               | port | health          | runtime |
|----------|-------------------|------|-----------------|---------|
| website  | `website/`        | 7100 | `/robots.txt`   | node    |
| frontend | `frontend/`       | 7101 | `/`             | node    |
| backend  | `backend/`        | 7102 | `/health`       | conda   |
| browser  | `browser-service/`| 7103 | `/health`       | conda   |
| worker   | `backend/`        | —    | —               | conda   |

## State on disk

`$ROOT/.dev/` — git-ignored, safe to delete:

```
.dev/logs/<svc>.log     this run's output; the previous run is kept as <svc>.log.1
.dev/pids/<svc>.pid     written after spawn, removed on confirmed death
.dev/run.env            persisted resolved ports
```

**`run.env` outranks the compiled defaults.** This is a real trap: changing the
default ports in source does nothing until `.dev/run.env` is cleared, because a
persisted value wins. It cost a debugging round during the 7000→7100 move. The
Go version implements option (b) of the two below, and goes one step further:
it prints a visible line when a persisted port overrides a default, offers to
adopt the declared ports on an interactive terminal (yes rewrites `run.env`;
Enter keeps them), and ships a global `--refresh-ports` flag that adopts the
declared defaults without asking. Silently preferring stale state is the wrong
default; so is reordering the chain — consent, not precedence, is the remedy.

## Port resolution

Precedence, highest first:

1. `--port <svc>=<n>` — named, repeatable; outranks everything including
   `--ports`, and the two flag spellings are rejected together
2. `--ports=A,B,C,D` (positional; empty slot keeps the default)
3. caller env — `WEBSITE_PORT`, `FRONTEND_PORT`, `BACKEND_PORT`, `BROWSER_PORT`
4. persisted `.dev/run.env`
5. compiled default

The caller-env values must be captured and **unset** before anything else runs,
or a child process inherits them and a service that resolves a different port
still sees the caller's value in its environment. A service that declares a port
also gets it as a bare `PORT` (the Procfile/Heroku convention) in its child
environment; a portless service gets none.

Collision detection must be **computed pairwise over all services**, not a
hand-written list of comparisons. The shell version had three hardcoded
comparisons for three services; adding a fourth silently left three of the six
pairs unchecked until it was rewritten as a loop. In Go this is a map from port
to service name, and the error should name **both** services and the port.

## Lifecycle

**Start:** skip if already running (pid alive) → take the cross-process START
CLAIM (an O_EXCL create of `.dev/pids/<svc>.pid.claim`; a fresh claim from
another live mabo-ctl refuses the start, stale wreckage is cleared) → refuse if
the port is already in use by something else → truncate the log → spawn detached
with stdout+stderr to the log and stdin from `/dev/null` → write the pid
(superseding the claim) → poll health.

Spawn must survive the parent exiting: the shell version uses a subshell with
`trap '' HUP INT` plus `disown`. In Go, `SysProcAttr{Setsid: true}`.

**Readiness:** poll the health probe until it answers, the process dies, or the
timeout expires. Three outcomes, all distinct in the output: `ready`, `slow`
(still starting), `failed` (process died — print the log tail).

The probe is one of three families, chosen by how `health:` is written: an
http(s) URL (HEAD then GET on 405/501; any response is answering), a TCP dial
(`{tcp: host:port}` — connected is ready, nothing is written to the socket), or
an exec probe (`{exec: [argv]}` — run in the service's dir/env under a hard
timeout, exit 0 is ready). The non-HTTP families are what make readiness — and
therefore `slow` vs `degraded` vs `failed` — honest for portless services: queue
consumers, gRPC daemons, databases.

**Stop:** SIGTERM, wait `STOP_GRACE`, then SIGKILL. Kill the process *group*,
not the pid — `npm run dev` spawns a child that survives a bare pid kill and
keeps the port bound, which is how the parent repo accumulated 28 orphaned
`astro dev` processes across three days.

## Bugs found in the shell version — do not reproduce these

These are recorded because each one was expensive to diagnose and each is a
design lesson, not a typo.

**1. A wrong directory that could never have worked.**
`svc_dir` and `svc_exec` both pointed the browser service at `$ROOT/browser`.
That directory has never existed in this repo; it is `browser-service/`. The
branch could only ever fail on `cd`. *Lesson:* validate every declared `dir` at
load time and refuse to start with a clear message. A config-driven Go version
gets this for free.

**2. Silent fall-through reported as "process died".**
A service listed in `ALL_SERVICES` with no `svc_exec` branch falls through the
`case`, the subshell exits 0 having written nothing, and the supervisor prints
`failed  process died — last log lines:` followed by an empty log. Diagnosing
that took three rounds. *Lesson:* an unknown service must be a load-time error,
not a runtime no-op. The shell version now has a `*)` branch that exits 64; Go
should make it unrepresentable.

**3. A healthy service reported "slow" forever.**
The readiness probe does a full `curl` GET and the body must complete inside the
timeout. An Astro **dev** page response does not — the probe timed out
`with 71602 bytes received` while the server itself answered in 2 ms. *Lesson:*
health-check a **cheap, small, complete** endpoint. The website probes
`/robots.txt`. In Go, additionally: use a `HEAD` where supported, cap the body
read, and treat "response headers received" as ready rather than requiring EOF.

**4. IPv6-only bind.**
Astro dev binds `localhost` → `::1` only. Probing `127.0.0.1` fails while
`localhost` succeeds. *Lesson:* probe by hostname, or try both families and
succeed on either. Do not assume IPv4.

**5. Runtime not on PATH.**
`dev.sh` inherits whatever PATH the caller has. Under a non-login shell nvm is
absent and `npm` is either missing or the wrong version (v20 vs v24 here).
*Lesson:* resolve the interpreter explicitly per service (a `runtime:` field —
`conda:<env>`, `node:<version>`, `system`) and fail loudly with the resolved
path rather than inheriting ambiently.

## Behaviour worth preserving

- Per-service colour and fixed-width labels — scanning a five-line status block
  is the main thing this tool is for.
- `exec` and `shell` reusing the exact service environment. This is what makes
  `mabo-ctl exec backend pytest` correct rather than approximately correct.
- `reset` killing orphans by port, not only by pid file. Pid files go stale;
  the port is the ground truth.
- Refusing to start when the port is held by a foreign process, with the `lsof`
  command printed so the user can see who holds it.

## Open questions

1. **Config discovery** — walk up for `mabo-ctl.yaml`, or require `--config`?
   Walking up matches git and is what makes `mabo-ctl` usable from a subdirectory.
2. **Log rotation.** The shell version truncates on start and nothing else;
   `backend.log` reached 6 MB. Size-capped ring buffer, or rotate on start?
3. **Does `watch` belong in the same binary?** It is the only long-running
   foreground mode and the only piece that wants desktop notifications.
4. **Windows.** Process groups, signals and conda activation all differ. Declare
   it out of scope explicitly rather than half-supporting it.

## Migration

The port convention landed in commit `438437e`. When `mabo-ctl` exists:
`dev.sh` is deleted from both repos, each gets a `mabo-ctl.yaml`, and the port
table above moves into that file. Until then `dev.sh` stays as-is — it works,
and it is no longer being extended.
