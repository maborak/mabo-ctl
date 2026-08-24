# Contributing to mabo-ctl

Thanks for looking. mabo-ctl is small, opinionated, and has a few rules that are
load-bearing rather than stylistic — this file is mostly about those.

## Getting set up

```bash
git clone https://github.com/maborak/mabo-ctl
cd mabo-ctl
make tools     # golangci-lint, govulncheck, deadcode (all optional but expected)
make check     # gofmt + build + vet + race tests
make lint      # golangci-lint + govulncheck + deadcode
```

Go version comes from `go.mod`. There is nothing else to install: mabo-ctl has
four direct dependencies and no code generation, no build step for the web
console (it is one embedded HTML file), and no external services in the tests.

To try it against real processes:

```bash
make install                 # into $(GOBIN)
cd /some/repo/with/mabo-ctl.yaml
mabo-ctl start --interactive
```

`examples/mabo-ctl.yaml` is a working config to copy.

## The rules that are not negotiable

These exist because breaking them produced real bugs. Each one is a line in
[`docs/LANDMINES.md`](docs/LANDMINES.md) with a detector you can run.

1. **A bug fix ships with a test that fails without the fix.** Not "a test that
   passes" — a test you have watched go red with the fix reverted. A test that
   passes both ways proves nothing and will be asked about in review.
2. **Signals go to the process group, never a bare pid** — except in `reset`'s
   reap-by-port, where the reason is written down at `signal_unix.go`. A
   supervised `npm run dev` spawns a child that survives a pid kill and keeps
   the port.
3. **Redact at the source, not per output channel.** If a value is withheld on
   one route and printed on another, that is the bug — not a formatting
   difference. `internal/redact` is the only place those rules live.
4. **Layering points inward.** `cmd` → `console`/`repl`/`web`/`ui` →
   `supervisor`/`health` → `service`/`state` → `config`. `ui`, `console`, `repl`
   and `web` import neither `os/exec` nor `syscall`; `supervisor` returns state
   and never formats user-facing strings; `config` is pure.
5. **Port-collision detection is computed pairwise**, never a hand-written list
   of comparisons. Three services need three comparisons; four need six, and the
   shell predecessor got that wrong in exactly that way.
6. **The phase set is closed.** Adding one means `supervisor.Phases()` plus every
   render site: `internal/ui`, `internal/console`, `internal/repl`,
   `internal/web/console.html`, and the aliveness table the console's buttons
   read. Tests enforce this.

## Commits

Conventional commits: `type(scope): imperative subject`.

- **type**: `feat`, `fix`, `refactor`, `perf`, `test`, `chore`, `docs`
- **scope**: `cmd`, `config`, `service`, `supervisor`, `health`, `state`,
  `console`, `repl`, `ui`, `web`, `deps`, `tooling`, `docs`

Write the body for someone who will read it in a year with no memory of the
problem: what was wrong, why the fix is the right shape, and what it cost to
find out. The existing history is the reference.

## Scope

Please read the **non-goals** in [`AGENTS.md`](AGENTS.md) before proposing a
feature. mabo-ctl is deliberately not a production supervisor, not a container
orchestrator, not a task runner, and not cross-platform. Windows is out of
scope by declaration — process groups, signals and runtime activation all
differ, and pretending otherwise would make the Unix path worse.

`exited` will not grow a restart policy. That is the phase that will tempt one.

## Security

Do not open a public issue. See [SECURITY.md](SECURITY.md).
