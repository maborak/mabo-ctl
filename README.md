# mabo-ctl

[![CI](https://github.com/maborak/mabo-ctl/actions/workflows/ci.yml/badge.svg)](https://github.com/maborak/mabo-ctl/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/maborak/mabo-ctl.svg)](https://pkg.go.dev/github.com/maborak/mabo-ctl)
[![Go Report Card](https://goreportcard.com/badge/github.com/maborak/mabo-ctl)](https://goreportcard.com/report/github.com/maborak/mabo-ctl)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
![macOS | Linux](https://img.shields.io/badge/platform-macOS%20%7C%20Linux-lightgrey)

A per-repository supervisor for the long-running processes a local development
stack is made of. One static Go binary, one declarative `mabo-ctl.yaml`, and
enough state on disk that a second terminal knows what the first one started.

```
$ mabo-ctl status
website   ● ready     7100  41823  1.4s   HTTP 200
frontend  ● ready     7101  41824  2.1s   HTTP 200
backend   ⚠ degraded  7102  41825  2.0s   dial tcp 127.0.0.1:7102: connection refused
browser   ◐ slow      7103  41826  30.0s  waiting for http://localhost:7103/health
worker    ⊘ exited    -     -      -      exit code 1, 4m ago
                                          Traceback (most recent call last):
                                            ImportError: no module named app
```

`mabo-ctl` starts, stops, restarts, health-checks, tails and inspects N services,
each with its own working directory, runtime, environment and port. It is
`supervisorctl`-shaped, but per-repo and zero-install.

**It is not** a production supervisor (no restart-on-crash policy, no resource
limits), not a container orchestrator, and not a task runner. macOS and Linux
only: process groups, signals and interpreter activation all differ on Windows,
so Windows is explicitly out of scope rather than half-supported.

## Install

```sh
go install github.com/maborak/mabo-ctl/cmd/mabo-ctl@latest
```

Or take a pre-built static binary from the
[latest release](https://github.com/maborak/mabo-ctl/releases/latest) — macOS and
Linux, `arm64` and `amd64`, built with `CGO_ENABLED=0`, so there is nothing to
install alongside it. Every release ships a `SHA256SUMS`; check it before you
trust the binary.

Then drop a `mabo-ctl.yaml` at the root of your repository and run `mabo-ctl`.

### 30-second start

```sh
cat > mabo-ctl.yaml <<'YAML'
services:
  - name: api
    dir: .
    port: 8000
    health: http://localhost:{{.Port}}/
    cmd: [python3, -m, http.server, "{{.Port}}"]
YAML

mabo-ctl start          # start it, wait for the probe, print the status block
mabo-ctl logs api -f    # follow its output
mabo-ctl serve          # drive the whole stack from a browser
mabo-ctl stop           # SIGTERM the process GROUP, then SIGKILL
```

`examples/mabo-ctl.yaml` is a fuller one: several services, a pinned runtime, and
one service told where another landed via `{{.Port "backend"}}`.

### Upgrading

```sh
mabo-ctl upgrade
```

`upgrade` asks GitHub for the latest release of this repository, compares it
against the version this binary was built from, and — when a newer one exists —
downloads the asset for this platform, verifies it against the release's
`SHA256SUMS`, and renames it over the running binary. A checksum mismatch or a
failed download leaves the installed binary untouched; the running process keeps
executing the old image until it exits.

`--force` reinstalls the latest release even when this binary is not older. A
binary built from source (a commit sha or `dev`) cannot be version-compared;
`upgrade` says so and installs the latest release anyway.

If the repository is private, `upgrade` authenticates with `GITHUB_TOKEN` (or
`GH_TOKEN`, the same precedence the gh CLI documents); without a token a private
repository is an honest 404. The token travels only in an Authorization header
to GitHub's https endpoints.

## Configuration

`mabo-ctl.yaml` lives at the repository root and is found by walking **up** from
the current directory, the way git finds `.git`, so `mabo-ctl status` works from
any subdirectory. `mabo-ctl --config <path>` skips the search.

```yaml
stop_grace: 10s       # SIGTERM, wait this long, then SIGKILL the process group
ready_timeout: 30s    # a probe that has not answered within this is "slow"
                      # before it and "degraded" after it

services:
  - name: website
    dir: website
    port: 7100
    health: http://localhost:{{.Port}}/robots.txt
    runtime: node:24.4.1
    cmd: [npm, run, dev, --, --port, "{{.Port}}"]
    env:
      PUBLIC_API_BASE: http://localhost:{{.Port "backend"}}
    color: green

  - name: backend
    dir: backend
    port: 7102
    health: http://localhost:{{.Port}}/health
    runtime: conda:app-dev
    cmd: [uvicorn, "api_main:app", --port, "{{.Port}}", --reload]
    env_file: backend.env   # KEY=VALUE lines; inline `env:` overrides a shared key
    color: blue

  - name: worker           # no port, no health: "running" once it is alive
    dir: backend
    runtime: conda:app-dev
    cmd: [python, cli.py, monitor, run]
    ready_timeout: 5s      # this service's own window; the global stays 30s
    depends_on: [backend]

  - name: seed             # autostart: false — kept out of a bare `mabo-ctl start`
    dir: backend
    autostart: false
    runtime: conda:app-dev
    cmd: [python, cli.py, seed]

checks:                    # mabo-ctl preflight
  - name: postgres
    tcp: localhost:5432
  - name: redis
    command: [redis-cli, -h, localhost, -p, "6379", ping]

shells:                    # mabo-ctl shell <name>
  - name: db
    service: backend
    command: [python]
```

A fully commented reference file, with every field explained, is in
[`examples/mabo-ctl.yaml`](examples/mabo-ctl.yaml).

Three things about that file are worth stating outright:

- **Everything is validated at load time**, and every problem is reported at
  once. A directory that does not exist, an empty `cmd`, a duplicate name, a
  duplicate port, an unknown `depends_on`, a dependency cycle and an unknown
  `runtime` are all errors *before* anything is spawned.
- **`cmd` is argv, never a shell string.** There is no shell, so there are no
  quoting rules and no word splitting.
- **`autostart: false` opts a service out of the DEFAULT start, not out of the
  registry.** A bare `mabo-ctl start` skips it. `mabo-ctl start seed`, `mabo-ctl start
  --all` and the console's **Start all** all start it — naming it, or asking for
  everything, is an instruction rather than a default. `depends_on` still pulls
  it in when something selected needs it, and `stop`, `status`, `logs` and
  `exec` treat it like any other service.
- **`runtime:` resolves the interpreter explicitly.** `conda:<env>` and
  `node:<version>` produce an absolute path and fail loudly naming the path they
  looked for, rather than inheriting whatever `PATH` your shell happens to have.
  mabo-ctl does not care whether it was invoked from a login shell.

`{{.Port}}` is this service's resolved port and `{{.Port "backend"}}` is another
service's. Templates are expanded in `cmd`, `env` values, `env_file` values and
`health`, *after* every port has resolved.

**`env_file:`** points at a file of `KEY=VALUE` lines (blank lines and `#`
comments ignored), anchored at the repository root like `dir:`. It lays the
base and the inline `env:` map overrides it key by key — the same rule as
docker-compose, so a one-off override does not mean editing the file. The file
is validated at load time and re-read at every resolve, so editing it takes
effect on the next start without touching `mabo-ctl.yaml`.

**Per-service `ready_timeout:`** overrides the global for one service — a
worker that legitimately needs two minutes to warm up sets its own window
instead of forcing the whole stack to call everything slow for two minutes.
Leave the key out to inherit the global.

## Commands

| Command | What it does |
|---|---|
| `mabo-ctl` | On a terminal: the full-screen console. Piped or redirected: the status block, then exit. |
| `mabo-ctl <service>` | Shorthand for `mabo-ctl start <service>`. |
| `mabo-ctl repl` | A resident prompt that runs these same commands — see [The prompt](#the-prompt). |
| `mabo-ctl start [svc...] [-f] [-a] [-i] [--web-console] [--ports=A,B,C,D] [--all]` | Start services (dependencies first) and wait for readiness. `-f` follows their logs afterwards; `-a`, `-i` and `--web-console` stay instead of exiting — see [Staying after the start](#staying-after-the-start). |
| `mabo-ctl stop [svc...]` | SIGTERM the process **group**, wait `stop_grace`, then SIGKILL. |
| `mabo-ctl restart [svc...] [-f]` | Stop, then start. |
| `mabo-ctl status [--json]` | One line per service. `--json` is the stable machine contract. Always exits 0: a service being down is information. |
| `mabo-ctl health` | The same phases `status` reports, with an exit code: 4 when any declared health URL did not answer. |
| `mabo-ctl config [svc] [--json] [--raw]` | Where `mabo-ctl.yaml` was loaded from and what it resolved to: the port **and which of the four precedence levels produced it**, the absolute command, the runtime, the expanded health URL, the declared env. `--raw` prints the file verbatim. |
| `mabo-ctl logs [svc\|all] [--tail=N] [-f]` | Tail a log, or interleave every log with per-service labels. `tailf` is an alias. |
| `mabo-ctl reset [--force]` | Stop everything and delete `.dev/`. `--force` also kills whatever still holds a declared port. |
| `mabo-ctl preflight` | Run the `checks:` block: a TCP dial for `tcp:`, an exec for `command:`. |
| `mabo-ctl exec <svc> <cmd>...` | Run a command in the service's exact environment and directory; forwards the child's exit code. |
| `mabo-ctl shell <name>` | Run a declared `shells:` entry, or open `$SHELL` in a service's environment. |
| `mabo-ctl open` | Hand each running service's URL to `open` (macOS) or `xdg-open` (Linux). |
| `mabo-ctl serve [--addr] [--open] [--i-know-this-is-dangerous]` | Serve the web console on `127.0.0.1:7999` until interrupted. It can start and stop services — see [Web console](#web-console). |
| `mabo-ctl completion <bash\|zsh\|fish\|powershell>` | Print a completion script. |
| `mabo-ctl upgrade [--force]` | Replace this binary with the latest GitHub release — see [Upgrading](#upgrading). |

Global `--config <path>` overrides discovery on every command.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | Success. |
| `1` | A runtime failure. |
| `2` | A usage error: an unknown service, an unknown flag, a wrong argument count. |
| `3` | `mabo-ctl.yaml` is missing, unreadable or invalid. |
| `4` | A service failed to become ready inside `ready_timeout`. |

`mabo-ctl exec` is the single exception: it forwards the child's exit code
verbatim, which is the point of running a command through it.

An unknown service always exits 2 and lists every declared name. A typo must be
a loud error, never a command that quietly does nothing.

A resident mode does not swallow the start that failed inside it: `mabo-ctl start
-a`, `-i` and `--web-console` still exit 4 when a selected service never became
ready, however cleanly the console or the prompt was quit. The failure already
happened; closing a window is not a fix.

## Staying after the start

`mabo-ctl start` exits as soon as the stack is up, which is right for a script and
wrong for a person who is about to watch it. Three flags make it stay:

| Flag | What you get |
|---|---|
| `-a`, `--attach` | The full-screen console, over the services that were just started — see [Interactive console](#interactive-console). |
| `-i`, `--interactive` | The resident prompt — see [The prompt](#the-prompt). |
| `--web-console` | The web console, bound to a free loopback port, its URL printed with its token on a line of its own, held open at the prompt — see [Web console](#web-console). |

```sh
$ mabo-ctl start --web-console
website  ● ready     7100  41823  1.4s  HTTP 200
backend  ● ready     7102  41825  2.0s  HTTP 200
mabo-ctl console listening on 127.0.0.1:59104
The URL below carries a session token that can start, stop and restart every
declared service. Treat it as a password.
http://127.0.0.1:59104/?token=6f1c…
Leaving the prompt — "quit", "exit" or Ctrl-D — stops the console, and so does
typing "unserve". The services keep running either way.
mabo-ctl(amazon-watcher)> logs backend -f
```

**Without a terminal, none of them happens.** In a script, a Makefile or CI the
flag is reported on stderr and ignored, and mabo-ctl starts the services and exits
with exactly the stdout and exit code it would have had with no flag at all. A
prompt reading input nobody is typing never ends, and `--web-console` would bind
a socket only to drop it a moment later as mabo-ctl exited — so the terminal is
checked, not assumed.

Only one of the four can be given at a time, `-f` included: each hands the one
terminal to something different, and combining two is a usage error that starts
nothing. `--interactive` with `--web-console` is the exception, because it is
not a combination — the web console lasts only as long as mabo-ctl does, so it
needs a resident host, and the prompt is that host.

`--web-console` picks its port with `127.0.0.1:0`, which is the kernel choosing
a free one: a console you did not ask for by name must never fail your start by
colliding with one already open. `--web-addr host:port` overrides it. It does
**not** imply `--i-know-this-is-dangerous` and never will — a non-loopback
`--web-addr` is refused with exit 2, and nothing is started or bound, unless
that flag is given as well. Asking for a console is not authorising one on the
network.

## Phases

Every front end — the status block, the interactive console, the web console and
`status --json` — reports one of exactly these, and all of them derive it from
the same place, so they cannot disagree about the same service at the same
instant.

| Glyph | Phase | Meaning |
|---|---|---|
| `●` | `ready` | Alive, and its health URL answered. |
| `◆` | `running` | Alive, and no health URL is declared — "is it answering?" is not a question mabo-ctl can ask. |
| `◐` | `slow` | Alive, the probe has not answered yet, and it is still inside `ready_timeout` of the spawn. |
| `⚠` | `degraded` | Alive, the probe is not answering, and `ready_timeout` has passed. `slow` out of excuses. |
| `✕` | `failed` | mabo-ctl started it and it died during startup. It never came up. |
| `⊘` | `exited` | mabo-ctl started it, it came up, and it is gone without mabo-ctl stopping it. |
| `○` | `stopped` | No process: never started, or stopped by mabo-ctl. |

The glyph, the word and the colour all carry the phase, so the block reads the
same through `grep`, on a monochrome terminal and to a colour-blind reader.

**`stopped` says what is in the way.** A stopped service that declares a port is
asked who is holding it, and the answer lands in the DETAIL column as the same
sentence `start` refuses with — `port already in use: port 7411 held by pid 5334
(nc) — inspect with: lsof -nP -iTCP:7411 -sTCP:LISTEN`. That refusal is an event
that scrolls past; the status block is what you go back and read, and it used to
be empty for exactly the service the refusal was about. The lookup runs only for
a service that is stopped, declares a port and has nothing else to explain — a
service that *died* keeps its exit reason and log tail, which is the better
answer — and it is cached for a couple of seconds so the web console's two-second
poll does not fork `lsof` continuously. A machine with no `lsof` reports what it
always did: a plain `stopped`, with no detail and no error.

**`exited` is a report, not a trigger.** mabo-ctl does not restart a service that
crashes and it is not going to: a supervisor that silently resurrects a crashing
service hides the crash loop it exists to show you. What it does instead is
remember: the exit code or signal, when it happened, and the last lines the
process printed, in `.dev/exits/<service>.json` and in the status block, until
you start or stop the service yourself.

How much it can remember depends on whether a mabo-ctl was still running when the
service died. A resident front end — `mabo-ctl serve`, the interactive console, or
the `mabo-ctl start` that is still waiting for readiness — is the process the
kernel hands the exit status to, so it records the code or the signal and the
time. When the death happens later, with no mabo-ctl anywhere, what is left is a
pid file naming a process that no longer exists; that is still `exited`, with
the log tail and without an exit status, because "it crashed and I cannot tell
you how" is a different answer from "you never started it".

> **`--json` contract change.** `degraded` and `exited` are new phase values,
> `failed` is newly reachable in `status` output rather than only in the live
> event stream, and the fields `started_at`, `uptime_ms`, `exit_code`,
> `exit_signal` and `exited_at` are appended to every record. The added fields
> are backwards compatible; the phase values are not, for a consumer that
> switches on `phase`. A crashed service used to report `stopped` —
> indistinguishable from one that was never started — a service that never came
> up reported `stopped` two lines under an event that called it `failed`, and a
> service broken for hours used to report `slow` forever.

## Port precedence

Highest first. This is the heart of the tool.

| # | Source | Notes |
|---|---|---|
| 1 | `--ports=A,B,C,D` | Positional. Slot *i* is the *i*-th service that declares a port, in declaration order. An **empty slot keeps the declared default**, so `--ports=,,7999` overrides only the third. |
| 2 | `<NAME>_PORT` in the caller's environment | e.g. `BACKEND_PORT=7999`. A `-` in a service name becomes `_`. |
| 3 | `.dev/run.env` | Persisted by the previous `start` or `restart`. |
| 4 | `port:` in `mabo-ctl.yaml` | The declared default. |

Two rules are not optional:

- **Caller variables are captured *and unset*** before anything spawns. Reading
  `BACKEND_PORT` without removing it would leave it in the environment every
  child inherits, so a service whose port resolved to something else would still
  bind the caller's value, and the supervisor would probe a port nobody is
  listening on. mabo-ctl re-injects the authoritative `<NAME>_PORT` for every
  service into each child's environment.
- **A persisted port that outranks a changed default is announced — and can be
  adopted.** Changing a default port in `mabo-ctl.yaml` appearing to do nothing,
  because `.dev/run.env` silently won, cost a real debugging round. mabo-ctl
  prints a line on stderr saying which service is on which port and where it
  came from, and on an interactive terminal asks whether to adopt the declared
  ports (answering yes rewrites `.dev/run.env`; Enter keeps them). Scripts and
  the committed yaml-as-truth skip the question with the global
  `--refresh-ports` flag, which adopts the declared defaults and rewrites the
  file in one step. Stderr, so `status --json` on stdout stays a clean machine
  contract — and `--json` is never asked a question.

Port collisions are computed pairwise over the **resolved** ports and the error
names both services and the port.

**`mabo-ctl config` prints which level won**, per service, so "why is this service
on 7999?" is a command rather than a source-reading exercise:

```
$ mabo-ctl config website
  config     /repo/mabo-ctl.yaml  (found by walking up from the working directory)
  root       /repo
  state      /repo/.dev
  timeouts   stop_grace 10.0s   ready_timeout 30.0s

website
  port       7999  from run.env  (OVERRIDES the declared 7100 — adopt it with `mabo-ctl --refresh-ports`, or clear it with `mabo-ctl reset`)
  dir        /repo/website
  cmd        /usr/local/bin/npm run dev -- --port 7999
  runtime    node:20  →  /usr/local/bin/npm
  health     http://localhost:7999/robots.txt
```

Credential-shaped values in the health URL, the command arguments and the
declared environment are redacted by the same rules the web console uses.
`mabo-ctl config --raw` prints `mabo-ctl.yaml` byte for byte and is **not** redacted:
it is the file already in the working tree, and the point of `--raw` is to pipe
it somewhere.

## State on disk

Everything mabo-ctl remembers lives in `.dev/`, next to `mabo-ctl.yaml`. Add it to
`.gitignore`; it is safe to delete, and `mabo-ctl reset` deletes it.

```
.dev/
├── logs/<service>.log      stdout+stderr of THIS run, mode 0600
│   logs/<service>.log.1    the previous run, kept as crash evidence
├── pids/<service>.pid    {"pid":…,"started_at":…}, written after spawn,
│                         removed on confirmed death
├── exits/<service>.json  the last observed death: exit code or signal, when it
│                         ran, a short log tail, and whether it died before it
│                         ever came up — which is `failed` rather than `exited`
└── run.env               persisted resolved ports (PORT_<SERVICE>=<n>)
```

The pid file records the spawn time as well as the pid, because uptime has to
outlive the process that knows it — every `mabo-ctl status` is a different process
from the one that did the spawning. That timestamp is also what separates `slow`
from `degraded`: "has this been up longer than `ready_timeout`?" is unanswerable
without it. A pid file written by an older mabo-ctl held a bare integer; that
format is still read, so upgrading the binary does not orphan an already-running
stack, and a service with no recorded spawn time is never accused of being
degraded.

The exit record exists for the same reason: the process that watches a service
die is almost never the process that has to report it. There is at most one
record per service and it always describes the most recent run — mabo-ctl removes
it when it starts the service, and when it stops the service, so a deliberately
stopped service can never masquerade as a crashed one. A record carries a slice
of the service's own output, which is why it obeys the same `0600` rule as the
logs.

The directories are created `0700` and the files `0600`. A supervised service
prints whatever it likes on stdout and that lands in a log file — and, truncated,
in an exit record — so nothing under `.dev/` is ever created group- or
world-readable. Secrets in your environment are forwarded to children and can end
up in those logs — treat `.dev/` as secrets-adjacent, especially on a shared host.

## Interactive console

Running the bare binary on a terminal opens a console: the service list on top,
the selected service's log below, key hints on the last line. `mabo-ctl start -a`
opens the same console with the stack already coming up in it.

```
↑/↓ or j/k  select      s  start     x  stop      r  restart
a  start all            S  stop all  l or tab  focus the log pane
/  filter logs          g/G  top/bottom          ?  help      q  quit
```

Quitting does **not** stop the supervised services. They were spawned with
`setsid` and mabo-ctl is not their parent.

## The prompt

`mabo-ctl repl` opens a line-oriented console instead — as does `mabo-ctl start -i`,
which starts the stack first and leaves you at the same prompt. It runs **the
same commands this page documents** — there is no second list of verbs, because
a fourth independently maintained command surface is exactly the drift that
produced two diverging copies of the shell script mabo-ctl replaces.

```
$ mabo-ctl repl
mabo-ctl(amazon-watcher)> start backend
mabo-ctl(amazon-watcher)> logs website -f
mabo-ctl(amazon-watcher)> exec backend pytest -k "not slow"
mabo-ctl(amazon-watcher)> status
```

Anything that works as `mabo-ctl <line>` works as `<line>` here, flags included.
Two verbs are the prompt's own, because they manage a listener whose lifetime is
the session rather than one command:

| Verb | What it does |
|---|---|
| `serve [host:port]` | Bind the web console and print its URL. Run it again and it prints the same URL — it does not bind twice. `serve 127.0.0.1:0` picks a free port. |
| `unserve` | Stop that console and release its port. Leaving the prompt does the same. |

Because the prompt is **resident**, it is the one place mabo-ctl can notice a
service dying while you watch. A crash lands in the scrollback as it happens —
`api exited (code 1) — mabo-ctl did not stop it; run "logs api" to see why` —
instead of waiting for the next time somebody runs `mabo-ctl status`.

`Ctrl-C` abandons the line you are typing, or cancels the command that is
running; it does not leave the prompt and it does not stop anything. `quit`,
`exit` and `Ctrl-D` leave, and **the services keep running** — type `stop`
first if you want them down. There is no tab completion and no history: the
prompt is a `bufio` line reader, not a line editor, and `mabo-ctl completion zsh`
already gives your real shell the completions.

The prompt shows the repository and nothing else. A count of running services
would be drawn once and be wrong the moment one died — the exact failure this
tool exists to remove — so `status` answers that on demand and the crash line
volunteers the only change worth interrupting for.

## Web console

`mabo-ctl serve` shows the same stack in a browser, with the two things a terminal
shows worst: the **exact command** each service runs, verbatim and copyable, next
to its resolved working directory, runtime and declared environment; and a live
log stream per service that you can filter, pause and scroll without losing your
place.

```sh
$ mabo-ctl serve
mabo-ctl console listening on 127.0.0.1:7999
The URL below carries a session token that can start, stop and restart every
declared service. Treat it as a password.
http://127.0.0.1:7999/?token=6f1c…
Ctrl-C stops the console; the services keep running.
```

Two other commands open the same console: `serve` at the prompt, and `mabo-ctl
start --web-console`, which starts the stack, binds a free loopback port and
holds the console open at the prompt. Both give the listener the session's
lifetime, so it is stopped exactly once — by `unserve`, or by leaving.

Open the printed URL — token included — or pass `--open` to hand it to the
platform browser opener. The page is a single embedded HTML file with no build
step, no framework and no CDN: it makes no request to anything but mabo-ctl. Status
refreshes on a timer and logs stream over server-sent events, so a closed tab
ends the stream behind it. `Ctrl-C` stops `mabo-ctl serve` and leaves every service
running, exactly like quitting the TUI — a console held open at the prompt is
stopped by `unserve` or by leaving the prompt instead, because there Ctrl-C
belongs to the line you are typing.

**Config** — the button in the top bar, or `c` — swaps the service list for the
resolved configuration: which `mabo-ctl.yaml` was loaded, the repo root, the state
directory, the effective `stop_grace` and `ready_timeout`, and a table of every
service's resolved port **with the precedence level that produced it** —
`flag`, `env`, `run.env` or `default` — flagging a persisted port that is
outranking a declared default you have since changed. Below it, per service: the
absolute `cmd[0]` the declared runtime chose, the working directory, the
expanded health URL, the declared environment and `depends_on`. It is a separate
view rather than extra rows, so the service list still fits your whole stack on
one screen; everything in it is redacted by the same rules as the rest of the
page. `mabo-ctl config` prints the same view in a terminal.

> **⚠ This is the one mabo-ctl surface that can be driven by something other than
> you.** Three of its routes start, stop and restart the commands in
> `mabo-ctl.yaml`, which makes it a local remote-code-execution surface — so it is
> guarded on four sides, and every guard is on by default:
>
> - **Loopback only.** It binds `127.0.0.1:7999`, reachable from this machine and
>   nothing else — `127.0.0.1` on a kernel-chosen free port when it is
>   `mabo-ctl start --web-console` that opened it.
> - **A session token.** 32 random bytes generated per run, printed once in the
>   URL, and required as an `X-Mabo-Ctl-Token` **header** on every start, stop and
>   restart. A page on the internet cannot read it, because it cannot read the
>   console page. Anyone who has the URL can run your dev stack: treat it as a
>   password, and do not paste it into a chat window or a bug report.
> - **`Host` and `Origin` checks.** Both must name the address mabo-ctl bound. That
>   is the DNS-rebinding defence: an attacker's domain can be pointed at
>   `127.0.0.1`, but the `Host` header still says the attacker's domain.
> - **POST-only mutations and no secrets in the page.** A `GET` cannot start
>   anything, so an `<img src>` cannot either; and only the environment
>   **declared in `mabo-ctl.yaml`** is rendered, with credential-shaped values
>   redacted. The inherited environment mabo-ctl forwards to children — the one
>   holding your real tokens — is never sent to the browser.
>
> **`--i-know-this-is-dangerous` is the only way to bind a non-loopback address,
> and it means exactly what it says.** A console on `0.0.0.0` is remote code
> execution offered to your network: everyone on the coffee-shop wifi who learns
> the token can run whatever your `mabo-ctl.yaml` declares, as you. Without the
> flag a non-loopback `--addr` — or `--web-addr` — exits 2 and binds nothing.
> Nothing implies it: not `--web-console`, not `serve` at the prompt, which
> cannot reach it at all because authorising this belongs on a command line you
> can find again in your shell history.

## Security

**`mabo-ctl.yaml` declares commands and mabo-ctl runs them. Running `mabo-ctl` in a
repository you do not trust runs that repository's code as you.** That is
arbitrary code execution by design — the trust boundary is "whoever can write
`mabo-ctl.yaml` can run code as the invoking user" — and it is the same bargain as
`make`, `npm run` or a `Makefile`. Clone-and-run is the risk; there is no
sandbox and none is claimed.

Some consequences worth knowing:

- Config discovery walks **up** the directory tree, so a `mabo-ctl.yaml` in a
  parent directory you did not intend can be found. Every error prints the
  absolute path of the file that was actually loaded.
- A service `name` composes `.dev/logs/<name>.log` and `.dev/pids/<name>.pid`,
  so it is restricted to `^[a-zA-Z0-9][a-zA-Z0-9_-]*$` and a `dir` may not escape
  the repository root. Both are load-time errors.
- `mabo-ctl reset` can reap by port as well as by pid file, because pid files go
  stale and the port is ground truth. That can kill a process mabo-ctl never
  started, so it is opt-in: a declared port still held after everything has been
  stopped is *named*, with the `lsof` command that identifies its holder, and
  left alone. `--force` kills it.
- `mabo-ctl open` passes the URL to the platform opener as a separate argument,
  never through a shell, and refuses any scheme other than `http` and `https`.
- `mabo-ctl serve` binds a listening socket, and its routes can start and stop
  services. It is loopback-only and token-guarded by default; the full control
  list is in [Web console](#web-console) above. No other command listens for
  anything.

mabo-ctl opens **no listening socket unless you run `mabo-ctl serve`**, ships **no
telemetry**, and makes **no authenticated outbound calls**. Its only unprompted
outbound traffic is the HTTP readiness probes to the URLs your config declares.

## Development

| Purpose | Command |
|---|---|
| build | `go build ./...` |
| vet | `go vet ./...` |
| test | `go test ./... -race -cover` |
| format | `gofmt -l .` / `gofmt -w .` |
| vulnerabilities | `govulncheck ./...` |

```
cmd/mabo-ctl/           flag wiring, exit codes, the console-vs-status decision
internal/config/      mabo-ctl.yaml: load and validate. Pure.
internal/service/     port resolution, template expansion, runtime resolution
internal/state/       .dev/: logs, pid records, exit records, run.env. The only writer.
internal/health/      HTTP readiness probes
internal/redact/      what is withheld from anything shown a reader. Pure, and the
                      ONLY copy of those rules: both front ends import it.
internal/supervisor/  spawn, signal process groups, pid files. Returns data.
internal/ui/          colour, fixed-width labels, status and config rendering, JSON
internal/console/     the full-screen TUI (bubbletea)
internal/repl/        the interactive prompt and its session
internal/web/         the web console: embedded page, JSON + SSE API, its guards
```

Dependencies point inward. `ui`, `console`, `repl` and `web` never call `os/exec` or `syscall`;
`supervisor` never formats a user-facing string; `config` has no side effects.

---

## Contributing

Bug reports, questions and patches are welcome — see
[CONTRIBUTING.md](CONTRIBUTING.md). It is short, and most of it is the handful
of rules that are load-bearing rather than stylistic: a fix ships with a test
that fails without it, signals go to the process group, and redaction happens at
the source rather than per output channel.

Please read the non-goals above before proposing a feature. They are stated
boundaries, not gaps.

## Security

mabo-ctl runs the commands in your `mabo-ctl.yaml`. **That is arbitrary code
execution by design** — cloning an untrusted repository and running `mabo-ctl`
inside it is equivalent to running that repository's code, exactly as with
`make` or `npm run`.

Everything above that boundary is in scope: config discovery escaping the repo,
a signal landing outside the process tree mabo-ctl started, a credential reaching
a channel that another channel redacts, or any bypass of the `mabo-ctl serve`
console's controls. **Report privately**, never in a public issue —
[SECURITY.md](SECURITY.md).

Bugs already found and fixed, each with the failure it caused and a detector
that proves it stays fixed, are in [`docs/LANDMINES.md`](docs/LANDMINES.md).

## Documentation

| Document | What it covers |
|---|---|
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | How the pieces fit: layering, the supervision path, the phase machine |
| [`examples/mabo-ctl.yaml`](examples/mabo-ctl.yaml) | Every `mabo-ctl.yaml` field, annotated — the configuration reference, and a test asserts it stays valid |
| [`docs/LANDMINES.md`](docs/LANDMINES.md) | Bugs mabo-ctl has actually shipped, and a runnable detector for each |
| [`docs/ROADMAP.md`](docs/ROADMAP.md) | What is being considered, and what was rejected and why |
| [`AGENTS.md`](AGENTS.md) | The full project brief — the source of truth for behaviour |
| [`SECURITY.md`](SECURITY.md) | Trust boundary, the console's controls, accepted risks |
| [`CHANGELOG.md`](CHANGELOG.md) | What changed, per release |

## License

[MIT](LICENSE) © Wilmer Adalid
