# Architecture

mabo-ctl is one static Go binary with three front ends over one supervisor. This
document explains how the pieces fit and — more usefully — *why* each boundary
is where it is. Most of them are load-bearing: crossing one has produced a real
bug, and several of those are recorded in [LANDMINES.md](LANDMINES.md).

## The shape

```
                 ┌──────────────┐  ┌──────────────┐  ┌──────────────┐
   front ends    │  one-shot    │  │ interactive  │  │ web console  │
                 │     CLI      │  │ prompt / TUI │  │mabo-ctl serve  │
                 └──────┬───────┘  └──────┬───────┘  └──────┬───────┘
                        │                 │                 │
                        └────────┬────────┴────────┬────────┘
                                 ▼                 ▼
                        ┌─────────────────┐  ┌──────────┐
     supervision        │   supervisor    │  │  health  │
                        │ spawn · signal  │  │  probes  │
                        │ pid · reap      │  └──────────┘
                        └────────┬────────┘
                                 ▼
                    ┌────────────────────────┐
     model + state  │  service    │  state   │
                    │  ports,     │  .dev/   │
                    │  templates  │          │
                    └────────────┬───────────┘
                                 ▼
                          ┌─────────────┐
     config               │   config    │  pure: reads and validates
                          └─────────────┘
```

**Dependencies point inward**, and nothing points back out.

| Package | Owns | May not |
|---|---|---|
| `cmd/mabo-ctl` | flag wiring, exit codes, the console-vs-status decision | — |
| `internal/console` | the full-screen TUI (bubbletea) | import `os/exec` or `syscall` |
| `internal/repl` | the interactive prompt and its session | import `os/exec` or `syscall` |
| `internal/web` | the loopback console: embedded page, JSON + SSE API, HTTP guards | import `os/exec` or `syscall`; open a browser |
| `internal/ui` | colour, fixed-width labels, status/config rendering, the `--json` contract | import `os/exec` or `syscall` |
| `internal/supervisor` | spawn, signals, process groups, pid files, reaping | format a user-facing string |
| `internal/health` | HTTP readiness probes | — |
| `internal/service` | the service model, port precedence, template expansion | — |
| `internal/state` | everything under `.dev/` — it is the only writer | — |
| `internal/redact` | what is withheld from anything shown to a reader | anything else; it is pure |
| `internal/config` | loading and validating `mabo-ctl.yaml` | any side effect |

Three of those rules are worth their own sentence:

- **`ui`, `console`, `repl` and `web` never touch `os/exec` or `syscall`.** Every
  process operation goes through the supervisor. `web` does not even open a
  browser — opening one means spawning one, so `Options.Open` records the intent
  and `cmd/mabo-ctl` performs it.
- **`supervisor` returns state; `ui` renders it.** A phase is derived in exactly
  one place, so a service cannot read `slow` in the terminal and `failed` in the
  browser in the same second.
- **`state` is the only writer under `.dev/`.** A service `name` composes
  `.dev/logs/<name>.log`, so a name containing `/` or `..` would write outside
  the state directory — validation lives at load time, and the layout is owned
  by one package.

## The supervision path

What `mabo-ctl start` actually does, in order:

1. **Skip if already running** — the pid file is present *and* the process is
   alive *and* it is ours. Liveness alone is not ownership: a recycled pid is
   alive and belongs to someone else, so the check also verifies the process is
   its own group leader, which every mabo-ctl child is by construction.
2. **Refuse if the port is held** by something mabo-ctl did not start — and print
   the `lsof` command, so the operator can see who holds it rather than being
   told "in use" and left to guess.
3. **Truncate the log**, so the tail printed on failure is *this* run's output.
4. **Spawn detached**: `SysProcAttr{Setsid: true}`, stdout and stderr to the log,
   stdin from `/dev/null`. Setsid is what makes the child survive the invoking
   terminal closing — a supervisor whose children die with the shell is broken —
   and it makes every child a process-group leader, which is what makes step 1's
   ownership test free.
5. **Write the pid record** (`{"pid":N,"started_at":"…"}`).
6. **Poll the health URL** until it answers, the process dies, or the timeout
   expires. Those are three different outcomes and they are reported as three.

Stopping reverses it: `SIGTERM` to the process **group**, wait `stop_grace`,
then `SIGKILL`. The group matters — a supervised `npm run dev` spawns a child
that survives a bare pid kill and keeps the port bound. That is how the shell
script mabo-ctl replaces accumulated 28 orphans in three days.

`reset --force` is the exception that signals a **single pid**, because there
the pid came from `lsof` rather than from our own pid file: a foreign listener
may share the user's shell process group, and killing that group would take
their terminal with it.

## The phase machine

Seven phases, enumerated by `supervisor.Phases()`. The set is closed, and
`ui.StatusJSON` is a stable contract, so adding one is a one-way door.

| Phase | Process | Probe | Meaning |
|---|---|---|---|
| `stopped` | absent | — | never started, or stopped by mabo-ctl |
| `running` | alive | none declared | "answering" is not a question mabo-ctl can ask |
| `ready` | alive | answered | the normal state |
| `slow` | alive | silent | still inside `ready_timeout` of the spawn |
| `degraded` | alive | silent | **past** `ready_timeout` — "still starting" would be a lie |
| `failed` | gone | — | died *during* startup |
| `exited` | gone | — | came up, then died without mabo-ctl stopping it |

Two distinctions do real work:

- **`slow` vs `failed`.** Collapsing them is how the predecessor produced its
  most expensive misdiagnosis: "it's still starting" and "it died 20 seconds ago"
  look identical if you only check whether the port answers.
- **`stopped` vs `exited`.** A process that crashed is simply *not there*, which
  is byte-identical to one that was never started. The exit record in
  `.dev/exits/` is what makes a crash visible: the reaper keeps the wait status
  — the kernel hands it to whoever waits, once — and writes it down.

**`exited` will not grow a restart policy.** It is the phase that will tempt one,
and "no restart-on-crash" is a stated non-goal. The refusal is written on
`PhaseExited` in the code.

## Port resolution

Four levels, highest first:

1. `--ports=A,B,C,D` — positional; an empty slot keeps the default
2. `<NAME>_PORT` in the caller's environment
3. the persisted `.dev/run.env`
4. the port declared in `mabo-ctl.yaml`

Two rules that exist because of specific failures:

- **Caller env is captured *and unset*** before anything spawns. Reading
  `BACKEND_PORT` without removing it leaves it in the environment every child
  inherits, so a service that resolved a *different* port still sees the
  caller's value — listening on one port while being told it is on another.
- **A persisted port that outranks a changed default is announced.** Stale state
  silently winning cost a debugging round during a port move.

Collision detection is computed **pairwise over a `port → services` map**, never
a hand-written list of comparisons: three services need three comparisons and
four need six, and that is exactly the arithmetic the predecessor got wrong.

Neither `--ports` nor the caller environment may invent a port for a service
that declares none. That guard was missing on one of the two branches, and a
stray `WORKER_PORT` in a shell led `reset --force` to kill a process mabo-ctl had
never started — [LANDMINES.md](LANDMINES.md) §3.

## State on disk — `.dev/`

Git-ignored, safe to delete, `0700` with `0600` files.

```
.dev/logs/<svc>.log     truncated on each start
.dev/pids/<svc>.pid     {"pid":N,"started_at":"…"} — a legacy bare integer is still read
.dev/exits/<svc>.json   the last death observed: code or signal, timings, a capped log tail
.dev/run.env            persisted resolved ports (read-modify-write, under a lock)
```

The exit record carries two flags that decide what a *later, different* mabo-ctl
process reports:

- **`startup`** separates `failed` from `exited`, and is set only by the start
  path — the one caller that knows the service never became ready.
- **`stopped`** is written *before* stop signals anything and cleared once death
  is confirmed. Without it, `mabo-ctl stop` printed `stopped` and, one line lower,
  its own status block called the same service `exited — killed by SIGTERM`:
  `cmd.Wait` cannot tell our own SIGTERM from a segfault, and the in-memory mark
  only reaches a reaper in the *same* process.

## The web console

`mabo-ctl serve` is the only surface something other than the developer's keyboard
can reach, and its POST routes call the same supervisor the CLI does. Its
controls, and the asymmetry between them, are documented in
[SECURITY.md](../SECURITY.md).

The one architectural note: the page is a single embedded hand-written HTML file
with no framework, no build step, and no external request of any kind — enforced
by a strict CSP and by a test that fails on any fetching construct pointing off
-origin. mabo-ctl ships no telemetry, and a console that phoned out for a
stylesheet would be its first outbound call that was not a health probe.
