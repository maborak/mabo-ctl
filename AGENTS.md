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
   `config`, `logs`/`tailf`, `open`, `reset`, `preflight`, `exec`, `shell`,
   `serve`, `completion`. `ui.StatusJSON` is the stable machine contract.
2. **Interactive console** — running the bare binary drops into a TUI
   (`internal/console`) or prompt (`internal/repl`).
3. **Web console** — `serve` binds a loopback HTTP listener (`127.0.0.1:7999`)
   serving one embedded page plus a JSON/SSE API. Token-gated, POST-only
   mutations, Host+Origin validated. It can start and stop processes, so treat
   every guard on it as security-critical.

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
internal/health/      readiness probes
internal/state/       .dev/ dir, pid files, run.env, exit records
internal/console/     full-screen TUI (bubbletea)
internal/repl/        interactive prompt + session
internal/ui/          colour, fixed-width labels, table + status rendering
internal/redact/      THE one credential-redaction implementation
internal/web/         web console: embedded page, JSON/SSE API, HTTP guards
```

### Layering rules — BINDING

- Dependencies point inward:
  `cmd` → `console`/`web`/`ui` → `supervisor`/`health` → `service`/`state` → `config`.
- `ui`, `console`, `web` never call `os/exec` or `syscall`; web drives processes
  only through the supervisor and never opens a browser itself.
- `supervisor` never formats user-facing strings; it returns state, `ui` renders.
- `config` is pure: reads files, validates, no process side effects.
- Writes under `.dev/` come only from `internal/state`.

## Behaviours that are load-bearing (do not regress them)

- **Port precedence**: `--ports` > caller env > `.dev/run.env` > declared
  default. Caller-env `<NAME>_PORT` variables are captured AND unset before any
  spawn. Collision detection is computed pairwise over all services — never a
  hand-written comparison list.
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
  `internal/redact` at the SOURCE, not per route.

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
- `docs/LANDMINES.md` — bugs this codebase actually shipped, each with a
  runnable detector. When you fix a diagnosed bug, adding its row is part of the
  fix. Cross-check findings against it before claiming anything new.
- `examples/mabo-ctl.yaml` — annotated reference config.
- `README.md` — user-facing everything else.
