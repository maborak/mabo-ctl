# LANDMINES — bugs mabo-ctl has actually shipped

Every row here was **earned**: a real bug, diagnosed in this repo, with the fix
that closed it and a detector that would have caught it. Nothing is here because
it seemed plausible. An invented row is worse than an empty table, because it
sends every future audit hunting a bug this project never had.

`/audit` Step 5 cross-checks findings against this file. A finding that matches a
row is CRITICAL regardless of the severity the agent gave it: it means a fix
regressed.

**If the same bug is hit twice, the catalogue failed.** Sharpen the detector
rather than adding a second row.

---

## 1. A read route served the credential that guarded the write routes

**Shape.** An access-control token is checked on the routes that mutate, and
handed out by a route that does not check anything.

**Where it bit us.** `internal/web/handlers.go:118` (`handleIndex`), found
2026-08-17 by the first full audit. All five mutating routes were wrapped in
`requireToken`; `GET /` rendered the page — with the token in a `<meta>` tag —
to any unauthenticated caller. A process running as another uid on the same host
could `curl /`, scrape the token, and drive start/stop/restart as the developer.
The token guarded the door and was posted on it. Reproduced end to end against
the built binary.

**Fix.** `Server.requireSession` now gates the index and every read route,
accepting the token from the query (a browser navigation), a `SameSite=Strict`
`HttpOnly` cookie (so a reload survives the page stripping the token from the
address bar), or the header. Mutations still accept the HEADER ALONE — a cookie
or query token must never authorise a POST, because both ride a cross-origin
request.

**Detector.**
```bash
go test ./internal/web/ -run 'TestUnauthenticatedRequestsNeverSeeTheToken|TestTheSessionCookieNeverAuthorisesAMutation|TestTheQueryTokenNeverAuthorisesAMutation'

# Structural: every GET route EXCEPT the index goes through the local get()
# helper, which wraps it in requireSession. The index is registered directly on
# purpose — handleIndex does its own session check so it can answer an
# unauthenticated browser with the unlock page instead of a bare 403 — so it is
# excluded here and covered by the behavioural test above instead.
#
# The exclusion is the point. This detector used to match that deliberate
# registration and fire on correct code, and this file's own rule says a
# catalogue hit is CRITICAL regardless of severity. A detector that always
# fires trains the next auditor to ignore it, and then it misses the regression
# it exists for. Verified both ways: silent on the real tree, and it still
# catches a GET moved off the helper.
rg -n 'mux\.Handle(Func)?\("GET ' internal/web/server.go | rg -v 'GET /\{\$\}'
```

---

## 2. Config discovery walked out of the repository and executed what it found

**Shape.** A "walk up until you find it" search with no upper bound, feeding
something that executes.

**Where it bit us.** `internal/config/config.go` (`Discover`), found 2026-08-17.
The walk ran to the filesystem root. Every command loads a config and most
EXECUTE what it declares, so a `mabo-ctl.yaml` in any ancestor — `$HOME`, `/tmp`, a
shared parent of several checkouts — silently became the config for every project
beneath it. `mabo-ctl start` in a deep subdirectory ran a parent's commands and
named the file nowhere. Reproduced with a proof-of-execution file.

**Fix (first attempt, INCOMPLETE — 2026-08-17).** The walk stops at a repo
marker (`.git`, `.hg`, `.svn`) or at `$HOME`, both checked AFTER searching that
directory so a `mabo-ctl.yaml` beside `.git` is still found.

**HIT AGAIN 2026-08-19.** That only bounds a walk that REACHES a boundary. A tree
with neither — `/tmp`, `/opt`, `/srv`, a mounted volume, a CI checkout without
`.git` — still ran to the filesystem root. Reproduced live from
`/private/tmp/.../parent/child/deep`. The catalogue caught it, which is what it
is for; the detector did not, because it only tested the marker case.

**Fix (second attempt).** The boundary stays, and a TRUST limit is added: a
config reached by CLIMBING must sit in a directory that is not group- or
world-writable and is owned by the invoking user (`climbableDir`,
`internal/config/owner_unix.go`). That is what separates "my project root" from
"a file somebody dropped in a shared parent", and it is the property that made
an unbounded climb dangerous in the first place. Refusing to climb at all was
tried and reverted: a project unpacked into `/opt/myapp` is not a repository and
still needs `mabo-ctl` to work from a subdirectory. The starting directory is
exempt — standing somewhere is a decision, the same one `make` acts on.
`--config` bypasses both limits. `app.announceDiscovery` prints the loaded path
whenever it came from outside the working directory.

**Detector.** Now covers BOTH shapes — the marker boundary and the climb into a
directory someone else can write.
```bash
go test ./internal/config/ -run 'TestDiscoverStopsAtTheRepoBoundary|TestDiscoverStillFindsAConfigBesideTheRepoMarker|TestDiscoverPathAgreesWithDiscover|TestDiscoverRefusesAConfigClimbedToInAWorldWritableDirectory|TestDiscoverStillClimbsToAPrivateDirectory'
```

---

## 3. A precedence level invented a port for a service that declares none

**Shape.** Two branches of one precedence chain, where only one carries the
guard both need.

**Where it bit us.** `internal/service/ports.go` (`resolvePorts`), found
2026-08-17. The persisted-`run.env` branch guarded on `s.Port > 0`; the
caller-env branch did not. A stray `WORKER_PORT` in the developer's shell gave a
portless worker a port, which reached the `--json` contract and armed the start
port-guard — and then `reset --force` killed whatever held that port. Verified
live: mabo-ctl killed a `python3` it never started, on a port no service declared,
announcing `killing pid 51327 … mabo-ctl did not start it`. `--force`'s own help
says it kills the holder of a **declared** port.

**Fix.** The caller-env branch carries the same `s.Port > 0` guard. The variable
is ignored rather than rejected — erroring would make mabo-ctl unusable in a shell
that exports the name for something else, which is how this triggers by accident.

**Detector.**
```bash
go test ./internal/service/ -run 'TestCallerEnvDoesNotInventAPortForAPortlessService|TestCallerEnvForAPortlessServiceIsIgnoredNotRejected'
```

---

## 4. Redaction applied per output route instead of at the source

**Shape.** A secret is withheld on the channel someone tested, and fans out
through the three nobody enumerated.

**Where it bit us.** `internal/supervisor/supervisor.go` (the `slow` and
`degraded` events), found 2026-08-17. `GET /api/status` redacted the health URL,
so the credential looked handled. The same URL was quoted verbatim into
`Event.Msg`, which travels to the **unauthenticated** SSE stream and back in the
body of every mutation response. Verified live: `/api/status` 0 hits,
`/api/events` 1 hit, POST body 1 hit.

A second instance of the same shape: mabo-ctl's own probe puts a health URL's query
credential into the SUPERVISED SERVICE's access log, which `/api/logs` and
`mabo-ctl logs` then serve. That one is not fixed — a service's log is opaque
output mabo-ctl must not rewrite — but it is why credentials belong in a header,
not a query string.

**Fix.** `redact.URL` is applied where the string is BUILT, not where it is
rendered. `internal/redact` remains the only implementation of the rules.

**Detector.**
```bash
go test ./internal/supervisor/ -run TestStartEventsNeverQuoteAHealthURLCredential

# Structural: every use of in.Health must be either redacted or one of the four
# legitimate non-rendering uses. Prints nothing when clean; prints the offending
# line when a new Event.Msg interpolates the URL raw. (An allowlist, not a
# pattern match on Sprintf — the format strings contain parentheses of their own
# and the argument lands on a continuation line, which defeats both a character
# class and a line-oriented grep. This version was verified by re-introducing
# the bug and confirming it fires.)
rg -n 'in\.Health' internal/supervisor/*.go | rg -v '_test\.go' \
  | rg -v 'redact\.URL|Health: +in\.Health|in\.Health == ""|health\.Wait\(|\}\(i, in\.Health\)'
```

---

## 5. `reset` killed a service mabo-ctl had just started, as a "foreign orphan"

**Shape.** A destructive sweep that identifies its target by an external fact
(who holds the port) without checking the internal fact (did we start it), while
another operation is free to run.

**Where it bit us.** `internal/supervisor/supervisor.go` (`Reset`), found
2026-08-17. `Reset` stops everything, then kills whatever holds each declared
port. Once mabo-ctl is RESIDENT — `mabo-ctl serve`, the interactive console — a
`start` can land in the window after that stop, and the sweep then killed a
healthy service mabo-ctl had spawned seconds earlier and deleted the record proving
it existed.

**Fix (first attempt, INCOMPLETE — 2026-08-17).** The sweep (`reapPort`) takes
the same per-service lock `startOne` and `stopOne` take, and skips a port whose
holder matches the service's own live pid record — announcing that it did,
because silence there reads as "nothing held it". Signalling still targets the
bare pid, never the group: that part is deliberate and documented at
`signal_unix.go:78`.

**HIT AGAIN 2026-08-19.** The sweep was locked; the whole-tree wipe two lines
below it was not. `s.st.Reset()` ran outside every lock, so a Start landing
between them wrote a pid record that the wipe then deleted — leaving a live
setsid-detached process mabo-ctl could no longer see, stop, or name. The orphan
this command exists to remove, produced by the command itself. Locking one step
of a two-step destructive operation is the shape to watch for.

**Fix (second attempt).** The wipe runs under `withAllServiceLocks`, which takes
every per-service lock in sorted name order — sorted so the order is
deterministic and cannot cycle against the single-lock callers.

**Detector.** Both halves: the sweep sparing a live service, and the wipe not
racing a concurrent start.
```bash
go test ./internal/supervisor/ -race \
  -run 'TestResetSweepSparesAServiceMaboCtlItselfStarted|TestResetDoesNotWipeStateUnderAConcurrentStart'
```

---

## 6. Lost update in the `run.env` read-modify-write

**Shape.** Read, merge, write — with nothing making the three one operation
across processes.

**Where it bit us.** `internal/state/runenv.go` (`WriteRunEnv`), found
2026-08-17. `WriteRunEnv` re-reads run.env for the keys it does not own and
writes them back. Two mabo-ctl invocations both read the same snapshot and the
second rewrite discarded whatever the first had added. Measured: **7 of 8 foreign
keys lost** with 8 concurrent writers.

Note the bound: port keys absent from the caller's set are dropped BY CONTRACT
(`Persist` requires the full instance list), so a missing port from a partial
write is not this bug.

**Fix.** `withFileLock` (flock, unix; no-op elsewhere) wraps the read and the
write, on a separate `run.env.lock` — a lock on run.env itself would guard an
inode that `writeFileAtomic` is about to replace.

**Detector.**
```bash
go test ./internal/state/ -run TestConcurrentWriteRunEnvDoesNotLoseAForeignKey -race -count=5
```

---

## 7. A precedence level silently skipped when the config was re-read

**Shape.** A reset that clears memoised state without re-running the work that
produced it.

**Where it bit us.** `cmd/mabo-ctl/app.go` (`reconcileConfig`), found 2026-08-17.
When cobra's parse of `--config` disagreed with the raw-argument peek, the
mismatch branch cleared the config and the resolution but never re-loaded or
re-captured. `capturedOnce` stayed stale, so the caller-env precedence level was
skipped for the whole invocation — and a variable that is never captured is never
UNSET, so it stayed in the environment forwarded to every child. That is exactly
the failure `service.CaptureEnv` exists to prevent: a service told it is on one
port while mabo-ctl supervises it on another.

**Fix.** The branch resets `capturedOnce` and calls `load()` + `capture()`.
`capture` now MERGES, because an earlier capture already removed the variable
from the environment and re-reading would find nothing.

**Detector.**
```bash
go test ./cmd/mabo-ctl/
rg -n 'a\.loaded, a\.cfg' cmd/mabo-ctl/app.go   # every reset of these must be followed by load()+capture()
```

## 8. `stop` inherited start's dependency expansion and killed what it was told to leave alone

**Shape.** Two verbs with opposite set semantics sharing one selector: the
selection answers "what belongs to this operation?" for START (a name plus its
dependency closure), and a verb that needed "exactly the named things" reused
the answer without noticing it was a different question.

**Where it bit us.** `internal/supervisor/supervisor.go`, diagnosed 2026-08-24
against the tiktok-bot stack. `mabo-ctl stop listener` went through
`service.Select`, whose DFS appends every transitive `depends_on` — so the
selection came back `[backend, listener]`, and Stop's reverse walk SIGTERM'd
the listener's own backend seconds after the listener died. The operator saw
`backend` flip to `degraded` (uvicorn draining behind a blackholed `/health`)
and then `stopped`, with no mabo-ctl line naming backend as a target. Three
documents said the opposite of the code: the command help ("stop signals the
named services"), AGENTS.md ("depends_on | Start ordering") and the user's own
mabo-ctl.yaml comment ("depends_on only orders the start").

**Fix.** `service.SelectExact` — validates names, deduplicates, keeps
declaration order, expands NOTHING; empty want still means everything, which is
stop's long-standing meaning for "none named". `Stop` selects through it;
Start keeps `SelectLevels`. Restart is deliberately asymmetric now: its stop
half takes exactly the named services, its start half still pulls a missing
dependency back up.

**Detector.**
```bash
go test ./internal/service/ -run TestSelectExact -race
go test ./internal/supervisor/ -run TestStopTakesExactlyTheNamedServices -race
rg -n 'service\.Select\(' internal cmd   # Select itself should now have NO production callers left
```

---

## 9. Two `mabo-ctl` processes could start the same service

**Shape.** A check-then-act sequence whose guard (a mutex) lives inside one
process, while the other participants are OTHER processes agreeing on nothing
but the filesystem.

**Where it bit us.** `internal/supervisor/supervisor.go` (`startOne`), diagnosed
2026-08-26 when closing roadmap #11; flagged in the code as a known gap since
the parallel-start work, and listed in SECURITY.md as an accepted risk until
then. The per-service mutex serialises lifecycle operations inside ONE mabo-ctl;
a second mabo-ctl in another terminal never reaches it. No pid file exists yet
on either side, so both pass the already-running check and both spawn. For a
service that declares a port the port guard catches the loser afterwards —
badly, but caught. For a portless service nothing intervenes at all: two
workers ran concurrently while `.dev/pids/<svc>.pid` recorded only the later
one, and the survivor was unreachable by every command mabo-ctl has — stop,
status, logs and exec all address a process through that record.

**Fix.** An exclusive-create START CLAIM taken before the port check:
`state.ClaimPID` creates `.dev/pids/<svc>.pid.claim` with `O_EXCL` — the one
primitive two independent processes agree on — recording who is starting and
since when. `WritePIDAt` supersedes the claim with the real record on success;
every failure path releases it. A claim found standing is refused with
`ErrClaimed` unless it is stale, where stale means its owner process no longer
exists, or it is older than ten minutes (longer than any legitimate start's
window), or it cannot be parsed — wreckage is cleared and retried, never fatal,
so a crashed mabo-ctl cannot wedge the service permanently.

**Detector.**
```bash
go test ./internal/state/ -race \
  -run 'TestClaimPIDExclusiveCreate|TestClaimPIDStalenessRules|TestReleaseClaimIsIdempotent'
go test ./internal/supervisor/ -race -run TestStartRefusesWhenAnotherMaboCtlHoldsTheClaim

# Structural: the claim must be taken BEFORE the port guard, or the
# check-then-spawn window reopens between the two lines. Read this output in
# order — ClaimPID first, lookupPortHolder second, both inside startOne.
rg -n 's\.st\.ClaimPID|lookupPortHolder' internal/supervisor/supervisor.go
```

---

## 10. Rotating the log silenced every attached follower

**Shape.** A feature that changed file identity (rename-and-recreate) breaking
another component's assumption written for in-place truncation (an open handle
keeps working through O_TRUNC — but not through a rename).

**Where it bit us.** `internal/state/state.go` (`TruncateLog`) and
`internal/supervisor/tail.go` (the follow loop), diagnosed 2026-08-27 from an
operator report that a web-console pane showed only lifecycle lines after a
restart. The rotation shipped so a restart stops destroying the crash evidence
of the run before it; but every follower — `/api/logs` SSE panes, the TUI's
pane, `logs -f` — held an fd on the OLD inode. The truncation check compared
that handle's own size against its offset, which never shrank after a rename,
so followers sat at the EOF of `.log.1` forever while the new run wrote into a
file they would never open again. Silent, across EVERY start/restart.

**Fix.** The follow loop now compares identity against the PATH each tick
(`sameFile`, Dev+Ino): a different inode there means reopen from the top of
the replacement. No duplicates are possible by construction — those bytes were
never delivered under the old handle. The legacy same-inode shrink check is
kept as a fallback.

**Detector.**
```bash
go test ./internal/supervisor/ -race -run TestTailFollowsTheLogAcrossARotation

# Structural: any future writer to .dev/logs must be checked against this
# invariant — if it changes the NAME↔INODE mapping of <svc>.log, Tail must see
# it through path identity, not handle state.
rg -n 'Rename|O_TRUNC' internal/state/ internal/supervisor/

## 11. `stop` killed a foreign process that was merely a group leader

**Shape.** An identity check that proved "ours ⇒ group leader" and then relied
on the unproven converse "group leader ⇒ ours". Every `setsid` process on the
machine — tmux panes, container inits, other supervisors' children — satisfies
the structural check, and the pid record's own spawn time, already on disk and
already in the struct, was never consulted before signalling.

**Where it bit us.** `internal/supervisor/signal_unix.go` (`verifyGroup`),
written in the initial release (2026-08-24); diagnosed 2026-08-27 by audit
H-1 with a live reproduction: a pid record stamped `2020-01-01` killed a
process the tester had spawned that same minute, because `pgid == pid` held.

**Fix.** `verifyGroup` takes the pid record's `StartedAt` and, when it is not
the zero time, requires the kernel's start time (`ps -o lstart=` under a
pinned `LC_ALL=C`, parsed for both the macOS and Linux layouts) to agree
within 2s. A start time that cannot be read at all refuses the signal:
a declined stop is recoverable, a wrong-group kill is not. The read-only
status path deliberately skips the comparison — display is not authority,
and paying a `ps` fork per status poll buys nothing.

**Detector.**
```bash
go test ./internal/supervisor/ -run TestVerifyGroupRefusesARecycledGroupLeader

# Structural: any identity check added to the signalling path must consult
# the record the pid file already carries.
rg -n 'StartedAt' internal/supervisor/ --glob '!*_test.go'
```

## 12. `tty: true` never worked: cobra ate the broker's own flags

**Shape.** A hidden subcommand whose argv IS its protocol registered with
`Args: cobra.ArbitraryArgs` — which bounds positional-argument COUNT and does
not stop flag parsing. Cobra rejected `--log`/`--sock` before RunE ran; the
parent JSON-unmarshalled cobra's error line as the handshake; the feature
failed on every platform with a syntax-error message. A package-var seam
(`ttyBrokerExecutable`) let every test substitute a well-behaved fake, so the
real dispatch path had no test at all.

**Where it bit us.** `cmd/mabo-ctl/attach.go` (`ttyBrokerCmd`) and
`internal/supervisor/tty_broker.go` (the handshake pipe), shipped in
95f277c (2026-08-26); diagnosed 2026-08-27 by audit H-2.

**Fix.** `DisableFlagParsing: true` on the broker command — the broker's
`parseBrokerArgs` owns its argv — and a dispatch test that executes the real
cobra command with RunE swapped, asserting the full raw argv arrives.

**Detector.**
```bash
go test ./cmd/mabo-ctl/ -run TestTTYBrokerDispatchPassesFlagsRaw
rg -n 'DisableFlagParsing' cmd/ internal/
```

## 13. A missing `lsof` turned the port guard off, silently, forever

**Shape.** A guard whose failure mode is indistinguishable from its success
value. `lsof` exits 1 when nothing listens — the common case — so "any error
means free" read ENOENT (no lsof on the box) as a free port on every service,
permanently. The project had already diagnosed the SAME shape for a different
cause (a dead context made every port read back as free) and closed only that
one path.

**Where it bit us.** `internal/supervisor/portholder.go` (`PortHolder`),
written in the initial release; diagnosed 2026-08-27 by audit M-2.

**Fix.** `lsof` is resolved once per process via an explicit `LookPath` with
the error retained (`LsofLookupErr`); the start path and preflight announce a
missing binary instead of leaving the fail-open guard unnoticed. The
fail-open contract itself is kept — "cannot tell" still must not block a
valid start — but it can no longer be silent.

**Detector.**
```bash
go build ./... && go run ./cmd/mabo-ctl preflight   # warns: guard is OFF

# Structural: an error swallowed into a zero value must have a companion
# probe the start path can surface.
rg -n 'LsofLookupErr' internal/ cmd/
```

## 14. The tty socket existed at umask permissions before its chmod

**Shape.** Ordering: `net.Listen` created the unix socket at `0777 & ~umask`
and the `Chmod(0600)` ran after — a brief window in which the socket carried
group/world bits. The same code also wrote under `.dev/` directly, breaking
the write-ownership invariant that names `internal/state` as the only writer.

**Where it bit us.** `internal/supervisor/tty_broker.go`, shipped in 95f277c
(2026-08-26); diagnosed 2026-08-27 by audit M-5. Unreachable in practice
until the broker's flag bug (§12) was fixed, which is how it survived.

**Fix.** `state.PrepareTTY` (create `.dev/tty`, clear a dead broker's stale
socket) runs before the listen; `state.SealTTY` runs immediately after it;
`state.RemoveTTY` replaces the direct remove. The broker no longer touches
the filesystem under `.dev/` itself.

**Detector.**
```bash
rg -n 'os\.(Remove|Chmod|Mkdir)' internal/supervisor/tty_broker.go
```

## 15. Clearing a stale start claim was silent

**Shape.** Asymmetric UX built alongside a security primitive: the REFUSAL
named the holder's pid and timestamp, but CLEARING the wreckage emitted
nothing on any branch. An operator could not distinguish a clean start from
"this start just overtook another mabo-ctl's in-flight claim" — which is
exactly the situation a hung pre_start hook (fixed in the same wave) used to
manufacture.

**Where it bit us.** `internal/state/state.go` (`ClaimPID`), shipped in
a9b72aa (2026-08-26); diagnosed 2026-08-27 by audit M-6.

**Fix.** `ClaimPID` returns a `ClaimReport` (who held the cleared claim, when
it was taken, why it was judged stale), and `startOne` emits it as an event.
A clean take reports nothing, so "evicted" stays distinguishable from "clean".

**Detector.**
```bash
go test ./internal/state/ -run TestClaimPIDReportsTheEviction
```
