# AGENTS.md — mabo-ctl

> Written for AI coding agents working in this repository, but equally useful to
> a human contributor. Read it before changing anything. English throughout,
> because that is the language of the code, comments and docs.

---

## What this is

`mabo-ctl` is a **per-repo local dev process supervisor written in Go**, shipped
as a single static binary with no runtime dependencies. It is `supervisorctl`
shaped: it starts, stops, restarts, health-checks, tails and inspects N
long-running local dev services — each with its own working directory, runtime,
environment and port — and keeps enough state on disk (`.dev/`) that a second
invocation from a different terminal knows what the first one started.

Services are declared in `mabo-ctl.yaml` at the repo root (discovered by a
**bounded** walk up the directory tree; `--config` is the escape hatch). A file
named `devctl.yaml` is still accepted under its pre-rename spelling so existing
stacks keep working; discovery reports it and the CLI asks the operator to
rename.

Three front ends over ONE supervisor:

1. **One-shot CLI** — `start`, `stop`, `restart`, `status [--json]`, `health`,
   `config`, `logs`/`tailf`, `open`, `reset`, `preflight`, `doctor`, `init`,
   `attach`,
   `exec`, `shell`, `serve`, `completion`, `schema [--commands]`, `upgrade`.
   `ui.StatusJSON` is the stable machine contract, `--version` prints the full
   build report, and `schema --commands` is the machine-readable catalogue of
   the binary itself.
2. **Interactive console** — running the bare binary drops into a TUI
   (`internal/console`) or prompt (`internal/repl`).
3. **Web console** — `serve` binds a loopback HTTP listener (`127.0.0.1:7999`)
   serving one embedded page plus a JSON/SSE API. Token-gated, POST-only
   mutations, Host+Origin validated. It can start and stop processes, so treat
   every guard on it as security-critical. It is the one TCP listener in the
   repo; the tty broker additionally holds a `.dev/tty/*.sock` unix-domain
   socket for local PTY IPC, which is not a network surface.

### Non-goals — out of scope by declaration, never gaps

Not a production supervisor (no restart-on-crash, no resource limits). Not a
container orchestrator. Not a task runner. Not cross-platform: Windows is out.
No database, no user accounts, no telemetry. The one network surface is the
loopback console above.

## Package layout

Inventory, not aspiration — derive counts with `go list ./...`, never trust a
number written here.

```
cmd/mabo-ctl/         main, flag wiring, cobra command tree
internal/config/      mabo-ctl.yaml load, template expansion, validation, discovery
internal/service/     service model + registry, port precedence
internal/supervisor/  lifecycle: spawn, signals, process groups, pid files
internal/health/      readiness probes: http, tcp dial, exec
internal/state/       .dev/ dir: logs, pid files, start claims, run.env, exit records
internal/console/     full-screen TUI (bubbletea)
internal/repl/        interactive prompt + session
internal/ui/          colour, fixed-width labels, table + status rendering
internal/redact/      THE one credential-redaction implementation
internal/selfupdate/  self-upgrade: latest release, version compare, verified binary swap
internal/web/         web console: embedded page, JSON/SSE API, HTTP guards
internal/surface/     generated surface inventories (cli, config, json) and the drift gate
tools/surfacemap/     generator whose output must be byte-identical to surfaces.json
```

### Layering rules — BINDING

- Dependencies point inward:
  `cmd` → `console`/`web`/`ui` → `supervisor`/`health` → `service`/`state` → `config`,
  with `selfupdate` a stdlib-only leaf that only `cmd` calls.
- `ui`, `console`, `web` never call `os/exec` or `syscall`; web drives processes
  only through the supervisor and never opens a browser itself.
- `supervisor` never formats user-facing strings; it returns state, `ui` renders.
- `config` is pure: reads files, validates, no process side effects.
- Writes under `.dev/` come only from `internal/state`.

## Behaviours that are load-bearing (do not regress them)

- **Port precedence**: named `--port svc=N` > positional `--ports` > caller env
  > `.dev/run.env` > declared default; the two flag spellings are rejected
  together. Caller-env `<NAME>_PORT` variables are captured AND unset before any
  spawn; a service that declares a port also gets it as a bare `PORT`. Collision
  detection is computed pairwise over all services — never a hand-written
  comparison list.
- **A start takes the cross-process CLAIM first** (`state.ClaimPID`, an O_EXCL
  create superseded by the pid record), before the port guard. A fresh claim
  held by another live mabo-ctl refuses the start with `ErrClaimed`; stale
  wreckage (dead owner, past ten minutes, unparseable) is cleared, never fatal.
- **Preflight is two blocks**: MACHINE (this repo's declarations resolving HERE;
  detect-and-hint) then the declared checks. A service whose runtime cannot
  resolve must never render as bare `stopped` anywhere.
- **internal/surface's drift gate owns three inventories** (cli, config, json):
  adding any command, flag, schema field or --json key without running
  `go run ./tools/surfacemap` fails the suite. Regenerate, review, commit.
- **The probe set is closed** — http URL, tcp dial, exec argv — chosen by how
  `health:` is written. A scalar value stays an http probe byte-for-byte, so
  pre-existing configs parse unchanged.
- **`schema --commands` is GENERATED from the live cobra tree, and the web mux
  is built FROM `consoleRoutes`** — one source each, two consumers. Adding a
  subcommand without a `commandMetas` entry, or a handler outside the route
  table, fails loudly instead of shipping undocumented.
- **Stop kills the process GROUP**, SIGTERM → grace → SIGKILL, identity verified
  before signalling (pid recycling is real).
- **`stop`/`restart` take exactly the named services** — `depends_on` orders
  STARTS only. A start expands names into their dependency closure; a stop never
  does.
- **The phase set is closed** (`stopped running ready slow degraded failed
  exited`) — enumerated by `supervisor.Phases()`. `exited` deliberately grows no
  restart policy.
- **Discovery is bounded** (repo marker or `$HOME`), and a config reached by
  climbing must sit in an owner-writable directory. Re-audit the boundary when
  touching it; see `docs/LANDMINES.md` §2.
- **Exit records** make crashes visible after the spawning process is gone;
  `failed` ≠ `slow` ≠ `exited`, and a deliberate stop must never read as a crash.
- Every output channel (stdout, logs, JSON, web) redacts through
  `internal/redact` at the SOURCE, not per route. That covers everything
  mabo-ctl itself composes; a child's stdout in `.dev/logs/*.log` is the
  child's own output, stored verbatim and channel-consistently — never
  re-rendered worse on one channel than another.

## Tooling

Go only. Exact commands, no equivalents from other ecosystems:

| Purpose         | Command |
|-----------------|---------|
| build           | `go build ./...` |
| vet             | `go vet ./...` |
| lint            | `golangci-lint run` |
| format check    | `gofmt -l .` (any output is a failure) |
| test            | `go test ./... -race -cover` |
| dead code       | `deadcode ./...` |
| vulnerabilities | `govulncheck ./...` |
| module hygiene  | `go mod tidy -diff && go mod verify` |

Doc comments: a comment on an exported identifier begins with that identifier's
name and is a complete sentence.

## Docs map

- `docs/SPEC.md` — spec of record.
- `docs/ARCHITECTURE.md` — how the packages fit and why.
- `docs/EXTENSIONS.md` — the shipped extension contracts (hooks,
  `depends_ready_on`, profiles, `--wait`, log filters, phase history) and
  the audit that separated them from what was already built in.
- `docs/LANDMINES.md` — bugs this codebase actually shipped, each with a
  runnable detector. When you fix a diagnosed bug, adding its row is part of the
  fix. Cross-check findings against it before claiming anything new.
- `examples/mabo-ctl.yaml` — annotated reference config.
- `README.md` — user-facing everything else.
