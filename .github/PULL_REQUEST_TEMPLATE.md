## What this changes

<!-- One or two sentences. What behaviour is different after this PR? -->

## Why

<!-- The problem, not the patch. If it fixes an issue, link it. -->

## Verification

Everything below runs from `make check` plus `make lint`:

- [ ] `gofmt -l .` prints nothing
- [ ] `go build ./...`
- [ ] `go vet ./...`
- [ ] `golangci-lint run` — 0 issues
- [ ] `go test ./... -race` — all pass
- [ ] `go mod tidy -diff` clean (only if dependencies changed)

**If this fixes a bug, the test must fail without the fix.** Say so here, and
say how you proved it — reverting the change and watching the test go red is the
proof, not the intention. See [`docs/LANDMINES.md`](https://github.com/maborak/mabo-ctl/blob/main/docs/LANDMINES.md)
for the format the project holds itself to.

## Anything touching the supervisor, state, or the web console

- [ ] Signals go to the process **group**, not a bare pid (or the PR explains why not)
- [ ] Nothing new writes under `.dev/` from outside `internal/state`
- [ ] `internal/ui`, `internal/console`, `internal/repl` and `internal/web` still
      import neither `os/exec` nor `syscall`
- [ ] A new phase, if any, is in `supervisor.Phases()` **and** every render site
- [ ] A new output channel does not fan out a credential that another channel redacts
