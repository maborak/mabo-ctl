package supervisor

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/maborak/mabo-ctl/internal/service"
)

// lsofTimeout bounds the port-holder lookup. lsof can block on a wedged mount
// or an unresponsive network filesystem, and a supervisor that hangs while
// answering "who has port 7100?" is worse than one that admits it does not
// know.
const lsofTimeout = 3 * time.Second

// holderTTL is how long [Supervisor.heldBy] may reuse an answer before asking
// lsof again.
//
// It exists because the read path now asks the question. `mabo-ctl status` asks
// once and exits, but the web console polls status every two seconds for as
// long as it is open, and every stopped service that declares a port would
// otherwise fork an lsof on every poll, forever, for a fact that changes when a
// process starts or dies rather than continuously. Two seconds is short enough
// that the block a human is reading is never stale by more than one poll, and
// long enough that one poll costs at most one lookup per port.
//
// The TTL must EXCEED every poller's period, not equal it. At exactly 2s — the
// web console's poll period — the next request always arrived a hair after the
// entry expired, because the console arms its next poll 2s after the previous
// response completes. The cache existed and never once hit. It is now well
// clear of both the console's 2s and the TUI's 1s tick, and correctness comes
// from explicit invalidation rather than from a short window: startOne and
// stopOne call forgetHolder, because those are the two moments mabo-ctl itself
// changes who holds a port.
//
// The cache is deliberately NOT consulted by startOne. There, "who holds this
// port" is a safety guard whose wrong answer spawns a second copy of a service
// onto a port something else already owns, and a two-second-old "free" is
// exactly the wrong answer to act on. A stale line in a status block is a
// cosmetic error; a stale port guard is an orphan.
const holderTTL = 15 * time.Second

// holderEntry is one cached port-holder answer and the moment it was learned.
type holderEntry struct {
	holder Holder
	at     time.Time
}

// Holder identifies the process listening on a port.
type Holder struct {
	// PID is the listening process, or 0 when the port is free or unknowable.
	PID int
	// Command is the executable name as reported by lsof, or "" when unknown.
	Command string
}

// PortHolder reports which process is LISTENING on port.
//
// It shells out to lsof because that is the only portable way to answer the
// question on both macOS and Linux without CAP_NET_ADMIN or parsing
// /proc/net/tcp by hand. The exact command is the one mabo-ctl prints to the user
// when it refuses to start a service, so the two can never drift:
//
//	lsof -nP -iTCP:<port> -sTCP:LISTEN
//
// A missing lsof, a timeout, or an unparseable line all yield a zero Holder and
// a nil error. That is deliberate: "I cannot tell who holds this port" must
// never become a fatal error that blocks an otherwise valid start. The caller
// treats a zero PID as "free, or at least not provably taken".
//
// PortHolder deliberately takes NO context and derives its deadline from
// context.Background(). It looks like an omission; it is the fix for a real
// defect. When this function accepted the caller's context, one Ctrl-C during a
// multi-service start cancelled that context, every subsequent lsof failed
// instantly, every port read back as "free", and the port-conflict guard in
// startOne silently turned OFF — mabo-ctl then spawned a second copy of a service
// onto a port another process already held. A lookup whose failure mode is
// "everything looks free" must not be cancellable by an unrelated caller.
// Callers that want to stop early check ctx themselves, before calling.
func PortHolder(port int) Holder {
	if port <= 0 {
		return Holder{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), lsofTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "lsof", "-nP",
		"-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN")
	out, err := cmd.Output()
	if err != nil {
		// lsof exits 1 when nothing matches, which is the common case for a
		// free port and not a failure worth reporting.
		return Holder{}
	}
	return parseLsof(string(out))
}

// LsofCommand returns the exact command mabo-ctl used to inspect port, so a
// refusal message can show the user how to reproduce the lookup themselves.
// Keeping this next to [PortHolder] is what stops the printed advice from
// drifting away from the command actually run.
func LsofCommand(port int) string {
	return "lsof -nP -iTCP:" + strconv.Itoa(port) + " -sTCP:LISTEN"
}

// portHeldError renders "this port is not free" the ONE way mabo-ctl says it.
//
// Two paths report it and they must not drift. `mabo-ctl start` refuses on it,
// and [Supervisor.Status] puts the same sentence in the DETAIL column of the
// stopped service the refusal was about — because the refusal is an event that
// scrolls past and the status block is the thing the user goes back and reads.
// A user who saw "port already in use" and then a status block with an empty
// DETAIL for that very service had been told, by the same command, both that
// something was in the way and that nothing was.
//
// name is the service whose start was refused, so the error can end with the
// REMEDY and not only the diagnosis: how to inspect the holder, then how to
// move this service somewhere else without editing the file. The escape hatches
// existed in the README; a failure message that does not carry them is where
// discoverability goes to die.
func portHeldError(name string, port int, h Holder) error {
	return fmt.Errorf("%w: port %d held by pid %d (%s) — inspect with: %s; "+
		"or start it elsewhere with %s=<port> mabo-ctl start %s",
		ErrPortHeld, port, h.PID, h.Command, LsofCommand(port), service.PortEnvVar(name), name)
}

// heldBy is [PortHolder] with a short memory, for the read path.
//
// A miss runs the lookup WITHOUT the cache lock held. Holding it across an lsof
// that can take up to [lsofTimeout] would serialise a whole status block behind
// one wedged port and undo the concurrency Status just paid for. The cost of
// letting go is that two simultaneous misses on the same port both look; that
// is one extra subprocess in a race that config validation already makes rare,
// since two services cannot declare the same port.
//
// A zero Holder is cached like any other answer. "Nobody holds it" and "this
// machine has no lsof" are the same result here, and re-forking a missing
// binary every two seconds to be told the same thing is the poll loop this
// cache exists to prevent.
func (s *Supervisor) heldBy(port int) Holder {
	if port <= 0 {
		return Holder{}
	}

	s.holdersMu.Lock()
	e, ok := s.holders[port]
	s.holdersMu.Unlock()
	if ok && time.Since(e.at) < holderTTL {
		return e.holder
	}

	h := s.lookupPortHolder(port)

	s.holdersMu.Lock()
	if s.holders == nil {
		s.holders = make(map[int]holderEntry)
	}
	s.holders[port] = holderEntry{holder: h, at: time.Now()}
	s.holdersMu.Unlock()
	return h
}

// forgetHolder drops the cached answer for a port, so the next read reflects a
// change mabo-ctl just made itself rather than waiting out the TTL.
func (s *Supervisor) forgetHolder(port int) {
	if port <= 0 {
		return
	}
	s.holdersMu.Lock()
	delete(s.holders, port)
	s.holdersMu.Unlock()
}

// lookupPortHolder performs the uncached lookup behind [Supervisor.heldBy]. It
// is [PortHolder] unless a test replaced it, which is the only way to exercise
// the cache and the held-port DETAIL on a machine whose lsof output no test can
// control.
func (s *Supervisor) lookupPortHolder(port int) Holder {
	if s.holderLookup != nil {
		return s.holderLookup(port)
	}
	return PortHolder(port)
}

// parseLsof extracts the first listening process from lsof's default output.
//
// The format is a header line followed by one row per handle:
//
//	COMMAND   PID USER   FD  TYPE DEVICE SIZE/OFF NODE NAME
//	node    44213 me     23u IPv6 0x1234      0t0  TCP *:7100 (LISTEN)
//
// Only COMMAND and PID are read. Everything else varies between macOS and
// Linux and is not worth depending on.
func parseLsof(out string) Holder {
	for i, line := range strings.Split(out, "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue // header, or trailing blank
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil || pid <= 0 {
			continue
		}
		return Holder{PID: pid, Command: fields[0]}
	}
	return Holder{}
}
