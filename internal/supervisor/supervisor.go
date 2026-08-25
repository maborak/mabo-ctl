// Package supervisor owns the process lifecycle: spawning services detached,
// probing them for readiness, signalling them down by process group, and
// reporting what is running.
//
// It returns DATA, never presentation. No colour, no padding, no tables — see
// package ui for that. The split matters because two very different front ends
// consume this package: a one-shot CLI that prints and exits, and a long-lived
// TUI that folds the same events into a model.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/health"
	"github.com/maborak/mabo-ctl/internal/redact"
	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/state"
)

// Errors reported by this package.
var (
	// ErrUnsafeSignal reports that a pid could not be signalled safely: its
	// process group is unresolvable, privileged, or does not match the pid,
	// which means the pid file is stale and the pid has been recycled.
	ErrUnsafeSignal = errors.New("unsafe to signal")

	// ErrPortHeld reports that a service cannot start because a process mabo-ctl
	// did not spawn is already listening on its port.
	ErrPortHeld = errors.New("port already in use")

	// ErrUnknownService reports a name that is not declared in mabo-ctl.yaml.
	ErrUnknownService = errors.New("unknown service")

	// ErrNotStarted reports that [Supervisor.Start] finished with at least one
	// service not running: it was refused, it failed to spawn, it died during
	// startup, or it was skipped because a dependency did not come up.
	//
	// Start wraps it and names the services, so a caller can map it to an exit
	// code with errors.Is. It exists because per-service failures reach the
	// caller only as Events, and an Event stream is easy to not consume: a Start
	// that returned nil after refusing every service reported success for a
	// stack that was entirely down.
	ErrNotStarted = errors.New("services did not start")
)

// Phase is the observable state of one service.
type Phase string

const (
	// PhaseStopped means no live process is recorded for the service.
	PhaseStopped Phase = "stopped"
	// PhaseRunning means the process is alive and the service declares no
	// readiness probe, so "answering" is not a question mabo-ctl can ask.
	PhaseRunning Phase = "running"
	// PhaseReady means the process is alive and its health URL answered.
	PhaseReady Phase = "ready"
	// PhaseSlow means the process is alive, a readiness probe is declared, the
	// probe has not answered yet, and the service is still INSIDE its startup
	// window. It is a distinct outcome from failure on purpose: a slow starter
	// and a dead one look identical if you collapse them.
	//
	// Slow is now bounded by [PhaseDegraded], which is what makes the word
	// honest. It used to be reported for any live service whose probe failed,
	// however long ago it started, so a service broken since breakfast still
	// read "still starting" at lunchtime.
	PhaseSlow Phase = "slow"
	// PhaseDegraded means the process is alive, a readiness probe is declared,
	// the probe is not answering, and more than ready_timeout has passed since
	// the process was spawned. It is [PhaseSlow] out of excuses.
	//
	// The distinction is the difference between "wait a moment" and "go look at
	// the log", and it is the word the shell predecessor already used —
	// DEGRADED (process alive but not responding) — which it kept distinct from
	// both DOWN and OK.
	PhaseDegraded Phase = "degraded"
	// PhaseFailed means mabo-ctl started the process and it died DURING startup:
	// it never came up at all.
	PhaseFailed Phase = "failed"
	// PhaseExited means mabo-ctl started the service, it came up, and it is gone
	// without mabo-ctl having stopped it. It is kept distinct from [PhaseFailed]
	// because "it never came up" and "it crashed at three o'clock" are
	// different problems that start with different questions.
	//
	// mabo-ctl does NOT restart a service in this phase, and adding a policy that
	// does is a STATED NON-GOAL — see the "not a production supervisor" line in
	// the README and in AGENTS.md. This is the phase that will tempt it, so the
	// refusal is written down here rather than anywhere else. mabo-ctl exists to
	// make a death VISIBLE to the developer sitting in front of it; a
	// supervisor that silently resurrects a crashing service hides the crash
	// loop it was supposed to report, and a restart policy never stays one flag
	// — it grows backoff, retry limits and flap detection until the tool is a
	// production supervisor with none of the guarantees of one. The record
	// behind this phase says what died, when, and what it printed. Fixing it is
	// the developer's job.
	PhaseExited Phase = "exited"
)

// Phases returns every phase mabo-ctl can report, ordered from most alarming to
// most healthy.
//
// It exists so the render sites cannot silently miss one. [Phase] is the stable
// machine contract behind `mabo-ctl status --json`, and it is drawn by three
// separate front ends; a new phase that one of them has no glyph for shows up
// as "unknown" to a user rather than as a compile error to whoever added it.
// Anything that enumerates phases — column widths, tests, the console's tile
// row — reads this slice instead of writing its own list.
func Phases() []Phase {
	return []Phase{
		PhaseFailed,
		PhaseExited,
		PhaseDegraded,
		PhaseSlow,
		PhaseStopped,
		PhaseRunning,
		PhaseReady,
	}
}

// Status is one service's state. It is pure data: internal/ui renders it, and
// [github.com/maborak/mabo-ctl/internal/ui.StatusJSON] serialises it as the
// stable machine contract.
type Status struct {
	// Name is the declared service name.
	Name string
	// Phase is the observable state.
	Phase Phase
	// PID is the supervised process, or 0 when stopped.
	PID int
	// Port is the resolved port, or 0 for a portless service.
	Port int
	// Health is the expanded readiness URL, or "" when none is declared.
	Health string
	// HTTP is the status code of the last probe, or 0 when there was none.
	HTTP int
	// Detail explains a non-obvious state in one line: who holds a contended
	// port, why a start was skipped, or the tail of a failed service's log.
	Detail string
	// LogPath is where this service's output is written.
	LogPath string
	// Elapsed is how long the last readiness wait took. It is PROBE LATENCY,
	// not uptime; Uptime is uptime.
	Elapsed time.Duration
	// StartedAt is when mabo-ctl spawned the process this status describes: the
	// live one, or — for [PhaseExited] — the one that died. It is the zero time
	// when mabo-ctl has never started the service and when the pid file was
	// written by a mabo-ctl old enough not to have recorded a spawn time.
	StartedAt time.Time
	// Uptime is how long the live process has been running. It is 0 whenever
	// nothing is running and whenever StartedAt is unknown, so it is never a
	// duration measured from the zero time.
	Uptime time.Duration
	// ExitCode is the status of the last death mabo-ctl observed, or -1 when a
	// signal killed the process or the wait status was never seen. It means
	// something only when ExitedAt is non-zero: 0 is both "exited cleanly" and
	// "there is no exit record".
	ExitCode int
	// ExitSignal names the signal that killed the process ("SIGKILL"), or "" when
	// it exited on its own and ExitCode is authoritative.
	ExitSignal string
	// ExitedAt is when mabo-ctl observed the death, or the zero time when there
	// is no exit record. It is the field that says whether the two above mean
	// anything.
	ExitedAt time.Time
}

// Event reports one lifecycle transition as it happens.
//
// The CLI prints events synchronously; the TUI folds them into its model. Sends
// are NON-BLOCKING: a slow or absent receiver drops events rather than wedging
// a start half-way through. Callers that need every event must supply a
// buffered channel and drain it.
type Event struct {
	// Service is the service the event concerns, or "" for a global event.
	Service string
	// Phase is the state being entered, or "" when the event is informational.
	Phase Phase
	// Msg is a one-line human-readable description.
	Msg string
	// Err is set when the transition failed.
	Err error
}

// Supervisor drives a resolved set of services.
type Supervisor struct {
	cfg    *config.Config
	st     *state.Dir
	insts  []service.Instance
	byName map[string]service.Instance

	// reap holds the goroutines waiting on spawned children. A child spawned
	// with Setsid is still OUR child until we exit, so something must call
	// waitpid or every stopped service leaves a zombie. That is invisible in a
	// one-shot CLI run and very visible in a TUI left open all afternoon.
	reap sync.WaitGroup

	// ops serialises lifecycle operations PER SERVICE, one mutex per name.
	//
	// startOne is a check-then-act: it reads the pid file, sees nothing
	// running, spawns, and only then writes the pid. Two concurrent starts of
	// the same service both read "not running", both spawn, and the second
	// WritePID overwrites the first — leaving a live process mabo-ctl has no
	// record of. Stop can then only ever signal the recorded pid, so the loser
	// keeps its port bound forever: every later start fails with ErrPortHeld
	// and every stop reports "not running". An orphan mabo-ctl cannot name is
	// precisely the failure this tool exists to prevent.
	//
	// One CLI invocation could not hit this, which is why it survived v1. The
	// web console can: two browser tabs, two Start clicks. The per-card busy
	// flag is per-page state and does not exist across tabs.
	//
	// This closes the race WITHIN one mabo-ctl process. Two mabo-ctl processes
	// racing the same .dev/ directory would still interleave; that needs an
	// O_EXCL pid-file create in package state and is recorded as a known gap.
	opsMu sync.Mutex
	ops   map[string]*sync.Mutex

	// stopping names the services mabo-ctl is deliberately taking down, so the
	// reaper can tell a crash from a shutdown it caused itself.
	//
	// Both look identical to cmd.Wait: a child killed by our own SIGTERM
	// returns the same shape of error as one that segfaulted. Without this,
	// every `mabo-ctl stop` would leave an exit record behind and every stopped
	// service would report itself crashed the moment it was stopped — the exact
	// lie the exit record exists to remove, inverted.
	//
	// It is set by stopOne before it signals anything and cleared by startOne
	// the instant a new process exists, both under the per-service lock the
	// reaper also takes, so the reaper always reads a settled value. It is
	// guarded by opsMu, which is only ever held for a map lookup.
	stopping map[string]bool

	// holders caches port-holder lookups for [holderTTL], keyed by port. Only
	// the READ path uses it; see Supervisor.heldBy for why startOne must not.
	holdersMu sync.Mutex
	holders   map[int]holderEntry

	// holderLookup overrides the uncached port-holder lookup. It is nil in
	// production, where [PortHolder] is used; it is a test seam, because lsof's
	// answer on the machine running the tests is not something a test can
	// arrange.
	holderLookup func(port int) Holder
}

// markStopping records that mabo-ctl is deliberately taking svc down.
func (s *Supervisor) markStopping(svc string) {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	if s.stopping == nil {
		s.stopping = make(map[string]bool)
	}
	s.stopping[svc] = true
}

// clearStopping forgets a deliberate stop, because svc is running again.
func (s *Supervisor) clearStopping(svc string) {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	delete(s.stopping, svc)
}

// wasStopped reports whether the last thing THIS mabo-ctl did to svc was stop it.
func (s *Supervisor) wasStopped(svc string) bool {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	return s.stopping[svc]
}

// markStoppedOnDisk writes svc's exit record as a deliberate stop, immediately
// before the signal that will cause the death it describes.
//
// [Supervisor.wasStopped] is memory, and memory only reaches the reaper inside
// the SAME process. The reaper is very often somewhere else: `mabo-ctl serve` or
// the interactive console spawns a service and stays resident, so it is the
// process the kernel will hand that child's wait status to — and a one-shot
// `mabo-ctl stop` in another terminal is a different process entirely. That
// reaper sees a child die of SIGTERM, has no idea anyone asked for it, and
// writes a crash record; the stop that caused it has already cleared the
// record and cannot clear it again. The result was a `mabo-ctl stop` printing
// "stopped" and its own status block, one line below, calling the same service
// `exited — killed by SIGTERM`.
//
// Writing the record first, with Stopped set, closes it from both sides:
// recordExit refuses to overwrite a record already written for this pid, so the
// foreign reaper stays quiet, and describeExit honours Stopped, so anything
// that reads the record before the stop finishes reports a stopped service.
// EndedAt is the moment of the DECISION, not of the death — it is what keeps
// the record from looking superseded by the pid file it is about to outlive.
//
// A failed write is reported and not fatal: the stop itself is what the user
// asked for, and the worst case is the misreport this exists to prevent.
func (s *Supervisor) markStoppedOnDisk(svc string, pid int, ev chan<- Event) {
	err := s.st.WriteExit(svc, state.ExitRecord{
		PID:     pid,
		EndedAt: time.Now(),
		Stopped: true,
	})
	if err != nil {
		emit(ev, Event{Service: svc, Err: err, Msg: fmt.Sprintf(
			"could not record that this stop was deliberate: %v", err)})
	}
}

// forgetStopped clears both halves of the record of a confirmed stop: the pid
// file, and the deliberate-stop marker markStoppedOnDisk left for the reaper.
// A service mabo-ctl stopped and watched die is simply stopped, and it should
// leave nothing behind that a later read has to interpret.
func (s *Supervisor) forgetStopped(svc string) {
	_ = s.st.RemovePID(svc)
	_ = s.st.RemoveExit(svc)
}

// lockService returns the mutex serialising operations on svc, creating it on
// first use.
func (s *Supervisor) lockService(svc string) *sync.Mutex {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	if s.ops == nil {
		s.ops = make(map[string]*sync.Mutex)
	}
	m, ok := s.ops[svc]
	if !ok {
		m = &sync.Mutex{}
		s.ops[svc] = m
	}
	return m
}

// New returns a Supervisor over the given resolved instances.
func New(cfg *config.Config, st *state.Dir, insts []service.Instance) *Supervisor {
	byName := make(map[string]service.Instance, len(insts))
	for _, in := range insts {
		byName[in.Name] = in
	}
	return &Supervisor{cfg: cfg, st: st, insts: insts, byName: byName}
}

// Instances returns the resolved instances in declaration order.
func (s *Supervisor) Instances() []service.Instance {
	out := make([]service.Instance, len(s.insts))
	copy(out, s.insts)
	return out
}

// Wait blocks until every reaper goroutine has finished. It exists so tests and
// the TUI can shut down without leaking goroutines; production callers that
// simply exit do not need it.
func (s *Supervisor) Wait() { s.reap.Wait() }

// emit delivers e without ever blocking the caller. See [Event].
func emit(ev chan<- Event, e Event) {
	if ev == nil {
		return
	}
	select {
	case ev <- e:
	default:
	}
}

// stopGrace is how long Stop waits after SIGTERM before escalating.
func (s *Supervisor) stopGrace() time.Duration {
	if s.cfg != nil && s.cfg.StopGrace > 0 {
		return s.cfg.StopGrace
	}
	return 10 * time.Second
}

// readyTimeout is how long Start polls a health URL before calling it slow.
func (s *Supervisor) readyTimeout() time.Duration {
	if s.cfg != nil && s.cfg.ReadyTimeout > 0 {
		return s.cfg.ReadyTimeout
	}
	return 30 * time.Second
}

// readyTimeoutFor is the per-service form: an instance that declares its own
// ready_timeout overrides the global, because a service that legitimately
// needs two minutes to warm up must not force the whole stack to wait two
// minutes before anything is called slow.
func (s *Supervisor) readyTimeoutFor(in service.Instance) time.Duration {
	if in.ReadyTimeout > 0 {
		return in.ReadyTimeout
	}
	return s.readyTimeout()
}

// ErrStalePID reports that a pid file names a live process that mabo-ctl did not
// start — the pid was recycled by an unrelated process while the file survived.
var ErrStalePID = errors.New("stale pid file")

// livePID returns the recorded pid for svc when a process with that pid is
// still alive AND mabo-ctl started it, and 0 otherwise. A malformed pid file is
// reported as an error rather than silently treated as "not running": a corrupt
// pid must never be acted on, and it must never be ignored either. A live pid
// that is provably NOT ours is reported as ErrStalePID.
//
// Liveness alone is not ownership, and conflating the two wedged a service
// permanently. `state.Alive` is a bare kill(pid, 0): it answers "does this
// number exist", not "is it mine". After a reboot recycles a pid into a stale
// `.dev/pids/<svc>.pid`, status reported someone else's process as running,
// start refused with "already running", and stop correctly refused to signal a
// process it could prove was not ours — and then left the file in place, so the
// next start refused again. Forever, until the user found `mabo-ctl reset`.
//
// The ownership test is the one Setsid gives us for free: every process mabo-ctl
// spawns is its own process-group leader, so pgid == pid. Anything else is not
// ours, whatever the pid file says.
func (s *Supervisor) livePID(svc string) (int, error) {
	rec, err := s.liveRecord(svc)
	return rec.PID, err
}

// liveRecord is livePID with the spawn time attached, for the callers that need
// to know how long the process has been up as well as that it is.
//
// The spawn time comes off DISK rather than out of memory, and it has to: every
// one-shot `mabo-ctl status`, and every poll of the web console behind it, is a
// different process from the one that did the spawning. "Has this been up long
// enough that not answering is a problem?" — the question [PhaseDegraded]
// answers — is unanswerable without it. A zero StartedAt means "unknown" (a pid
// file written before mabo-ctl recorded spawn times), never the epoch.
func (s *Supervisor) liveRecord(svc string) (state.PIDRecord, error) {
	rec, err := s.st.ReadPIDRecord(svc)
	if err != nil {
		return state.PIDRecord{}, fmt.Errorf("read pid for %s: %w", svc, err)
	}
	if rec.PID <= 0 || !state.Alive(rec.PID) {
		return state.PIDRecord{}, nil
	}
	if _, err := verifyGroup(rec.PID); err != nil {
		return state.PIDRecord{}, fmt.Errorf("%w: %s names pid %d, which mabo-ctl did not start: %w",
			ErrStalePID, s.st.PIDPath(svc), rec.PID, err)
	}
	return rec, nil
}

// clearStalePID removes a pid file that livePID proved is not ours, so the next
// operation is not blocked by it. Reporting without repairing is what turned a
// correct diagnosis into a permanent wedge.
func (s *Supervisor) clearStalePID(svc string, cause error, ev chan<- Event) {
	if rmErr := s.st.RemovePID(svc); rmErr != nil {
		emit(ev, Event{Service: svc, Err: rmErr, Msg: fmt.Sprintf(
			"could not clear the stale pid file: %v", rmErr)})
		return
	}
	emit(ev, Event{Service: svc, Msg: fmt.Sprintf(
		"cleared a stale pid file (%v)", cause)})
}

// StatusNoPorts is [Supervisor.Status] without the port-holder lookup.
//
// It exists for callers that read only the phase — the interactive console's
// crash watcher, which polls every two seconds for the life of a session. Those
// callers never render DETAIL, so paying an lsof fork per stopped-but-ported
// service on every tick buys nothing. Anything that DISPLAYS a status block
// wants Status.
func (s *Supervisor) StatusNoPorts(ctx context.Context) []Status {
	return s.status(ctx, false)
}

// Status reports the current state of every instance.
//
// It is the ONE place a phase is derived. `mabo-ctl status`, `mabo-ctl health`, the
// interactive console and the web console all render what this returns, so a
// service cannot read "slow" in the terminal and "failed" in the browser at the
// same instant — which is exactly what two derivations produced before.
//
// Health probes run concurrently — a five-service status block should cost one
// probe timeout, not five. Probing is skipped entirely for a service that is
// not alive, because a stopped service cannot be slow.
//
// When nothing is alive, Status consults the exit record rather than assuming
// "stopped". A service that died two seconds ago with a stack trace in its log
// used to be byte-identical to one that was never started; [PhaseExited] and
// the record behind it are the answer.
//
// A service that is genuinely stopped and declares a port also gets asked who
// is holding that port, in the same round of concurrency as the health probes.
// That is the question the status block is read to answer — "I ran start, it
// refused, what is in the way?" — and `mabo-ctl start` used to answer it in an
// event that scrolls past and then print a status block whose DETAIL for that
// very service was empty. See [Supervisor.heldBy] for the cost of asking and
// what bounds it.
func (s *Supervisor) Status(ctx context.Context) []Status {
	return s.status(ctx, true)
}

func (s *Supervisor) status(ctx context.Context, withHolders bool) []Status {
	out := make([]Status, len(s.insts))
	var wg sync.WaitGroup

	for i, in := range s.insts {
		st := Status{
			Name:    in.Name,
			Port:    in.Port,
			Health:  in.Health,
			LogPath: s.st.LogPath(in.Name),
			Phase:   PhaseStopped,
		}

		rec, err := s.liveRecord(in.Name)
		if err != nil {
			// Status is a read: it names the problem and leaves the repair to
			// the next start or stop, which is where a side effect belongs.
			st.Detail = err.Error()
			if errors.Is(err, ErrStalePID) {
				st.Detail += " — it will be cleared on the next start or stop"
			}
			out[i] = st
			continue
		}
		if rec.PID == 0 {
			s.describeExit(&st)
			out[i] = st
			if withHolders && wantsHolder(st, in.Port) {
				wg.Add(1)
				go func(idx, port int) {
					defer wg.Done()
					if ctx.Err() != nil {
						// A cancelled status render — a browser that navigated
						// away mid-poll — must not fork lsof for a column
						// nobody will read. Checked HERE and not inside
						// PortHolder, which ignores the caller's context on
						// purpose: a lookup whose failure mode is "everything
						// looks free" must not be cancellable by an unrelated
						// caller. Here the worst case of skipping is the empty
						// DETAIL this change replaces.
						return
					}
					if h := s.heldBy(port); h.PID > 0 {
						out[idx].Detail = portHeldError(port, h).Error()
					}
				}(i, in.Port)
			}
			continue
		}

		st.PID = rec.PID
		st.StartedAt = rec.StartedAt
		if !rec.StartedAt.IsZero() {
			st.Uptime = time.Since(rec.StartedAt)
		}

		if in.Health == "" {
			// Alive with nothing to probe. "running" is the honest answer;
			// claiming "ready" would assert something mabo-ctl never checked.
			st.Phase = PhaseRunning
			out[i] = st
			continue
		}

		// Assume the probe will fail and let the goroutine below upgrade it, so
		// a probe that never returns leaves the pessimistic answer rather than
		// a hopeful one.
		st.Phase = s.probeFailPhase(in, rec.StartedAt)
		out[i] = st

		wg.Add(1)
		go func(idx int, url string) {
			defer wg.Done()
			res := health.Probe(ctx, url)
			out[idx].Elapsed = res.Elapsed
			if res.OK {
				out[idx].Phase = PhaseReady
				out[idx].HTTP = res.Status
				return
			}
			// Name the dial failure verbatim. This text is why `mabo-ctl health`
			// used to be worth running separately; it belongs on the one path
			// that derives a phase, not on a second one beside it.
			out[idx].Detail = probeFailure(res)
		}(i, in.Health)
	}

	wg.Wait()
	return out
}

// wantsHolder reports whether Status should ask who holds st's port.
//
// Three conditions, and each one is a thing the lookup must not cost or must
// not overwrite:
//
//   - a declared port, because there is nothing to ask about without one;
//   - [PhaseStopped] exactly, because a service that DIED already has the
//     better answer — the exit status and the log tail that explain the death —
//     and a port is not usually what is wrong with it. describeExit sets
//     [PhaseExited] or [PhaseFailed] in that case, and neither is touched here;
//   - an empty Detail, because the two remaining ways to be stopped with
//     something already written there are a stale pid file and an unreadable
//     exit record, and both name a problem that outranks "and also the port is
//     busy".
//
// Everything else is skipped, which is what keeps a five-service block from
// forking lsof for services that are running perfectly well.
func wantsHolder(st Status, port int) bool {
	return port > 0 && st.Phase == PhaseStopped && st.Detail == ""
}

// probeFailPhase is the phase of a live service whose readiness probe is not
// answering: [PhaseSlow] inside the startup window, [PhaseDegraded] past it.
//
// startOne and Status both call it, so the phase `mabo-ctl start` prints and the
// phase the status block underneath it prints cannot disagree about the same
// service in the same second.
//
// An unknown spawn time reports slow. That is the generous answer, and it is
// the right one: a legacy pid file carries no spawn time, and accusing a
// service of being degraded on the strength of a timestamp mabo-ctl does not have
// would be the same kind of invention this whole change removes.
func (s *Supervisor) probeFailPhase(in service.Instance, startedAt time.Time) Phase {
	if startedAt.IsZero() || time.Since(startedAt) < s.readyTimeoutFor(in) {
		return PhaseSlow
	}
	return PhaseDegraded
}

// probeFailure renders why a readiness probe did not answer.
func probeFailure(res health.Result) string {
	if res.Err != nil {
		return res.Err.Error()
	}
	return "no response"
}

// describeExit explains a service with no live process: it either died, or it
// is genuinely stopped. It is called only when nothing is alive, and it leaves
// st alone — i.e. leaves it [PhaseStopped] — whenever there is nothing
// trustworthy to say.
//
// There are two grades of evidence, and the second one matters more than it
// looks.
//
// The exit record is the good one: mabo-ctl watched the process die and wrote
// down how, including whether the death happened before the service ever came
// up — which is what makes the difference between [PhaseFailed] and
// [PhaseExited] survive the process that observed it. Three things stop it
// turning a clean stop into a false crash —
// Stop removes the record outright, a record mabo-ctl knows it caused carries
// Stopped, and a record older than the pid file beside it describes a run that
// has since been superseded.
//
// The pid file alone is the weak one, and it is the ONLY evidence in the most
// common case there is. Only a mabo-ctl that is still running can reap a child,
// and `mabo-ctl start` exits seconds after spawning: the service it started
// outlives it, so when that service dies at three in the afternoon there is no
// mabo-ctl anywhere to notice. What survives is a pid file naming a process that
// no longer exists, and that is not ambiguous — a recycled pid is ALIVE and is
// caught by the process-group check in liveRecord long before this, so a
// recorded pid that is gone means exactly what it says. Reporting it as
// "stopped" is the byte-for-byte confusion between "crashed" and "never
// started" that all of this exists to remove, so it is reported as exited with
// no exit status rather than as a service nobody ever ran.
func (s *Supervisor) describeExit(st *Status) {
	pid, perr := s.st.ReadPIDRecord(st.Name)

	rec, ok, err := s.st.ReadExit(st.Name)
	if err != nil {
		// A record mabo-ctl wrote and cannot read back is a bug or tampering.
		// Saying so beats reporting the service as never started.
		st.Detail = err.Error()
		return
	}

	superseded := perr == nil && pid.StartedAt.After(rec.EndedAt)
	if ok && rec.Stopped && !superseded {
		// mabo-ctl asked for this. It is decisive, and it deliberately returns
		// BEFORE the pid-file branch below: a stop that has signalled but not
		// yet confirmed the death still has a pid file on disk, and reading
		// that as evidence of a crash is the very report the marker exists to
		// suppress.
		return
	}
	if ok && !rec.Stopped && !superseded {
		// A death observed while startOne was still waiting for readiness is
		// [PhaseFailed]: the service never came up. Anything else is
		// [PhaseExited]. The distinction cannot be re-derived here — by now the
		// process is gone and the only witness was the start that watched it —
		// so it is read back off the record rather than guessed.
		st.Phase = PhaseExited
		if rec.Startup {
			st.Phase = PhaseFailed
		}
		st.StartedAt = rec.StartedAt
		st.ExitCode = rec.ExitCode
		st.ExitSignal = rec.Signal
		st.ExitedAt = rec.EndedAt
		st.Detail = exitDetail(rec)
		return
	}

	if perr != nil || pid.PID <= 0 {
		return // no pid file: never started, or stopped and cleaned up
	}

	// ExitedAt stays zero and ExitCode is -1: mabo-ctl does not know when this
	// happened or how, and inventing either would be worse than saying so. The
	// log is still there, and it is the part that explains the death.
	st.Phase = PhaseExited
	st.StartedAt = pid.StartedAt
	st.ExitCode = -1
	st.Detail = fmt.Sprintf(
		"pid %d is gone and mabo-ctl did not stop it; no mabo-ctl was running when it "+
			"exited, so there is no exit status", pid.PID)
	if tail := strings.TrimRight(s.logTail(st.Name, failLogLines), "\n"); tail != "" {
		st.Detail += "\n" + tail
	}
}

// Start brings up the named services, or all of them when names is empty.
//
// Services start in dependency LEVELS: everything with no dependency inside the
// selection starts concurrently, then everything depending only on those, and
// so on. A service whose dependency failed is SKIPPED with that reason rather
// than started into a world it cannot work in.
//
// The levels are what make this fast. Walking a flat topological order serially
// makes independent services queue behind each other, so a stack pays the SUM
// of every startup time — three unrelated services taking three seconds each
// cost eleven seconds instead of three. Nothing is relaxed to get it: startOne
// is already safe under concurrency because of the per-service lock, the
// context is re-checked inside every goroutine, and failures are folded in
// slice order so the reported set is deterministic however the goroutines
// interleaved.
//
// It returns an error wrapping [ErrNotStarted] naming every service that did
// not come up, in the order they were attempted. Reporting those only as Events
// is not enough: a caller that does not drain the channel would see a nil error
// and conclude a wholly failed start had succeeded.
func (s *Supervisor) Start(ctx context.Context, names []string, ev chan<- Event) error {
	// An empty selection means "everything the operator wants started by
	// default", which is not the same as "everything declared". Turning it into
	// an EXPLICIT list here, rather than filtering after selection, is what
	// keeps a manual service reachable as a dependency: SelectLevels still
	// expands depends_on, so naming `worker` pulls in the `backend` it needs
	// even when backend is autostart: false. Filtering afterwards would have
	// dropped that dependency and started worker against a backend that was not
	// there.
	//
	// Naming a service explicitly always wins. autostart only ever decides what
	// happens when the operator named nothing.
	if len(names) == 0 {
		names = autostartNames(s.insts)
		if len(names) == 0 {
			emit(ev, Event{Msg: "every service sets autostart: false, so a bare start has nothing to do — " +
				"name the ones you want, or use --all (the console's \"Start all\") to start every declared service"})
			return nil
		}
	}

	levels, err := service.SelectLevels(s.insts, names)
	if err != nil {
		return err
	}

	// failed is read by firstFailedDep at the start of each LEVEL and written
	// only between levels, never during one. That is what makes it safe without
	// a lock: every dependency of a level-N service is in some level < N and is
	// therefore already final by the time level N is scheduled.
	failed := make(map[string]string)
	var order []string

	for _, level := range levels {
		// A cancelled context must STOP the run, not quietly weaken it. Before
		// this check, one Ctrl-C mid-start left every remaining service to be
		// spawned with its port-conflict guard disabled, because the lsof
		// lookup that guard depends on fails instantly on a dead context and a
		// failed lookup reads as "port free". Checked per level here AND again
		// inside each goroutine below, because a level's worth of services can
		// be in flight when the interrupt lands.
		if err := ctx.Err(); err != nil {
			emit(ev, Event{Msg: "interrupted; remaining services were not started", Err: err})
			return err
		}

		type outcome struct {
			name   string
			failed bool
			why    string // carried so a transitive dependant says "was skipped"
		}
		results := make([]outcome, len(level))
		var wg sync.WaitGroup

		for i, in := range level {
			if dep, why := firstFailedDep(in, failed); dep != "" {
				emit(ev, Event{Service: in.Name, Phase: PhaseStopped,
					Msg: fmt.Sprintf("skipped: dependency %s %s", dep, why)})
				results[i] = outcome{in.Name, true, "was skipped"}
				continue
			}
			wg.Add(1)
			go func(i int, in service.Instance) {
				defer wg.Done()
				if err := ctx.Err(); err != nil {
					results[i] = outcome{in.Name, true, "was interrupted"}
					return
				}
				phase, err := s.startOne(ctx, in, ev)
				results[i] = outcome{in.Name, err != nil || phase == PhaseFailed, "failed to start"}
			}(i, in)
		}
		wg.Wait()

		// Fold results in slice order, not completion order, so ErrNotStarted
		// names services deterministically however the goroutines interleaved.
		for _, r := range results {
			if r.failed {
				if _, seen := failed[r.name]; !seen {
					failed[r.name] = r.why
				}
				order = append(order, r.name)
			}
		}
	}

	if len(order) > 0 {
		return fmt.Errorf("%w: %s", ErrNotStarted, strings.Join(order, ", "))
	}
	return nil
}

// firstFailedDep returns the first dependency of in that did not come up.
func firstFailedDep(in service.Instance, failed map[string]string) (string, string) {
	for _, d := range in.DependsOn {
		if why, bad := failed[d]; bad {
			return d, why
		}
	}
	return "", ""
}

// startOne spawns a single service and waits for it to become ready.
//
// The order of the steps is load-bearing; each guard exists because skipping it
// produced a diagnosed failure in the shell predecessor.
func (s *Supervisor) startOne(ctx context.Context, in service.Instance, ev chan<- Event) (Phase, error) {
	// Serialise everything below against another start or stop of this same
	// service. The check-then-act from here to WritePID is the race; holding
	// the lock across it is what makes "already running" a real answer rather
	// than a stale one. See Supervisor.ops.
	lk := s.lockService(in.Name)
	lk.Lock()
	defer lk.Unlock()

	// 1. Already running? Starting a second copy is never what the user meant.
	if pid, err := s.livePID(in.Name); errors.Is(err, ErrStalePID) {
		// Provably not our process. Clear the record and start for real,
		// instead of refusing forever on the strength of a dead file.
		s.clearStalePID(in.Name, err, ev)
	} else if err != nil {
		emit(ev, Event{Service: in.Name, Msg: err.Error(), Err: err})
		return PhaseFailed, err
	} else if pid > 0 {
		emit(ev, Event{Service: in.Name, Phase: PhaseRunning,
			Msg: fmt.Sprintf("already running (pid %d)", pid)})
		return PhaseRunning, nil
	}

	// 1b. Did this service's runtime resolve? [service.Resolve] defers that
	//     failure to here so that one missing interpreter cannot stop the other
	//     services from being managed. Spawning anyway would run whatever
	//     ambient PATH turned up, which is dev.sh bug #5.
	if err := in.Runnable(); err != nil {
		emit(ev, Event{Service: in.Name, Phase: PhaseFailed, Err: err, Msg: err.Error()})
		return PhaseFailed, err
	}

	// 2. Port held by someone else? Refuse, and show the user who and how to
	//    look for themselves. Starting anyway produces a service that binds
	//    nothing and a supervisor that lies about it.
	if in.Port > 0 {
		//
		//    The lookup is the UNCACHED one on purpose, and the sentence is
		//    built by the same portHeldError the status block uses. See
		//    Supervisor.heldBy.
		if h := s.lookupPortHolder(in.Port); h.PID > 0 {
			err := portHeldError(in.Port, h)
			emit(ev, Event{Service: in.Name, Phase: PhaseFailed, Msg: err.Error(), Err: err})
			return PhaseFailed, err
		}
	}

	// mabo-ctl is about to change who holds this port; a cached answer from
	// before the change would be read back as current.
	s.forgetHolder(in.Port)

	// 3. Truncate the log so the tail we may print below is THIS run's output.
	logFile, err := s.st.TruncateLog(in.Name)
	if err != nil {
		emit(ev, Event{Service: in.Name, Phase: PhaseFailed, Err: err,
			Msg: fmt.Sprintf("cannot open log: %v", err)})
		return PhaseFailed, err
	}
	// The parent's copy is closed once the child has inherited the descriptor;
	// a close error on it says nothing about the child, which holds its own.
	defer func() { _ = logFile.Close() }()

	devnull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		emit(ev, Event{Service: in.Name, Phase: PhaseFailed, Err: err,
			Msg: fmt.Sprintf("cannot open %s: %v", os.DevNull, err)})
		return PhaseFailed, err
	}
	defer func() { _ = devnull.Close() }()

	// 4. Spawn detached. stdin from /dev/null matters: a service that reads
	//    stdin would otherwise inherit the terminal and steal the user's keys —
	//    catastrophic under the TUI.
	emit(ev, Event{Service: in.Name, Msg: "starting…"})
	cmd := exec.Command(in.Cmd[0], in.Cmd[1:]...) // #nosec G204 -- argv comes from mabo-ctl.yaml, which is arbitrary code execution BY DESIGN; see THREAT_MODEL.md
	cmd.Dir = in.Dir
	cmd.Env = in.Env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = devnull
	setDetached(cmd)

	if err := cmd.Start(); err != nil {
		err = fmt.Errorf("spawn %s: %w", in.Name, err)
		emit(ev, Event{Service: in.Name, Phase: PhaseFailed, Msg: err.Error(), Err: err})
		return PhaseFailed, err
	}
	pid := cmd.Process.Pid
	// Spawn time is captured here, at the instant the child exists, and written
	// to disk with the pid. It cannot live in memory: the next `mabo-ctl status`
	// is a different process with no recollection of this one, so uptime and the
	// "is it still inside its startup window" question are both unanswerable
	// unless the answer was written down at the spawn.
	startedAt := time.Now()

	// A new process supersedes whatever mabo-ctl last saw happen to this service:
	// the crash it recorded and the stop it was asked for are both history now.
	// Clearing them here, immediately after the spawn, is what stops a stale
	// record from making a service that is demonstrably running read as one
	// that died.
	s.clearStopping(in.Name)
	if err := s.st.RemoveExit(in.Name); err != nil {
		emit(ev, Event{Service: in.Name, Err: err, Msg: fmt.Sprintf(
			"could not clear the previous exit record: %v", err)})
	}

	// Reap the child when it exits. Setsid does not reparent, so mabo-ctl stays
	// its parent for as long as mabo-ctl lives; without a waitpid every stopped
	// service accumulates a zombie.
	//
	// The wait status is KEPT now. It is the only evidence of how the process
	// died and the kernel hands it over exactly once; see reapChild and recordExit.
	// The channel is buffered so the reaper never blocks on a startOne that
	// took the ready path and is not listening.
	reaped := make(chan exitInfo, 1)
	s.reap.Add(1)
	go func() {
		defer s.reap.Done()
		s.reapChild(in.Name, pid, startedAt, cmd, reaped)
	}()

	if err := s.st.WritePIDAt(in.Name, pid, startedAt); err != nil {
		emit(ev, Event{Service: in.Name, Err: err,
			Msg: fmt.Sprintf("started pid %d but could not record it: %v", pid, err)})
	}

	// 5. Readiness. Three outcomes, each distinct in the output.
	if in.Health == "" {
		emit(ev, Event{Service: in.Name, Phase: PhaseRunning,
			Msg: fmt.Sprintf("running (pid %d, no health check declared)", pid)})
		return PhaseRunning, nil
	}

	waitCtx, cancel := context.WithTimeout(ctx, s.readyTimeoutFor(in))
	defer cancel()
	res := health.Wait(waitCtx, in.Health, func() bool { return state.Alive(pid) })

	switch {
	case res.OK:
		emit(ev, Event{Service: in.Name, Phase: PhaseReady,
			Msg: fmt.Sprintf("ready in %s (pid %d, HTTP %d)",
				res.Elapsed.Round(time.Millisecond), pid, res.Status)})
		return PhaseReady, nil

	case !state.Alive(pid):
		// The process died. Print the log tail — and when there is nothing to
		// print, SAY SO. A bare "process died" over an empty log is what made
		// the equivalent shell bug take three rounds to diagnose.
		detail := s.logTail(in.Name, failLogLines)
		if strings.TrimSpace(detail) == "" {
			detail = fmt.Sprintf("log is empty (%s) — the process wrote nothing before dying, "+
				"which usually means the command itself could not be executed",
				s.st.LogPath(in.Name))
		}
		// Write the death down. The event below scrolls past; the status block
		// printed underneath it, and every `mabo-ctl status` after it, reads the
		// record instead of reporting a service that never started.
		//
		// The exit status comes from the reaper, which has already got it: a
		// zombie reports as alive, so reaching this branch means the waitpid
		// returned. Waiting for the handover rather than writing "unknown" and
		// letting the reaper correct it a moment later is what makes the record
		// — and the block printed from it — the same on every run.
		s.recordStartupDeath(in.Name, pid, startedAt, awaitReaped(reaped), detail)
		err := fmt.Errorf("%s died during startup", in.Name)
		emit(ev, Event{Service: in.Name, Phase: PhaseFailed, Err: err,
			Msg: "failed: process died\n" + detail})
		return PhaseFailed, err

	case s.probeFailPhase(in, startedAt) == PhaseDegraded:
		// The whole startup window went by. Saying "still starting" here would
		// be the lie that made `slow` worthless, and it is the same phase the
		// status block below is about to derive from the same spawn time.
		emit(ev, Event{Service: in.Name, Phase: PhaseDegraded,
			Msg: fmt.Sprintf("degraded: alive (pid %d) but not answering %s after %s — "+
				"past ready_timeout (%s), so it is not merely slow",
				pid, redact.URL(in.Health), res.Elapsed.Round(time.Millisecond), s.readyTimeoutFor(in))})
		return PhaseDegraded, nil

	default:
		emit(ev, Event{Service: in.Name, Phase: PhaseSlow,
			Msg: fmt.Sprintf("slow: alive (pid %d) but not answering %s after %s — still starting",
				pid, redact.URL(in.Health), res.Elapsed.Round(time.Millisecond))})
		return PhaseSlow, nil
	}
}

// Stop takes down the named services, or all of them when names is empty.
// Services stop in REVERSE dependency order so a dependant never outlives what
// it depends on.
// Stop signals the named services down and confirms each is gone.
//
// The selection is EXACT — SelectExact, not Select — on purpose. Start must
// expand a name into its dependency closure or the named service would come up
// against a missing foundation; stop has no such need, and inheriting start's
// set made `mabo-ctl stop listener` silently kill the backend it depends on
// (docs/LANDMINES.md §8). Naming none still means everything: for a stop that
// already meant "all", and nothing narrows it.
func (s *Supervisor) Stop(ctx context.Context, names []string, ev chan<- Event) error {
	sel, err := service.SelectExact(s.insts, names)
	if err != nil {
		return err
	}
	for i := len(sel) - 1; i >= 0; i-- {
		s.stopOne(ctx, sel[i].Name, ev)
	}
	return nil
}

// stopOne signals one service down and confirms it is gone.
func (s *Supervisor) stopOne(ctx context.Context, name string, ev chan<- Event) {
	// Same per-service lock as startOne: a stop that interleaves with a start
	// can signal a pid the start is midway through replacing.
	lk := s.lockService(name)
	lk.Lock()
	defer lk.Unlock()

	// Everything below this line is a deliberate shutdown, whatever it finds.
	//
	// The mark tells this process's reaper that the death it is about to
	// observe was asked for, because cmd.Wait cannot tell our own SIGTERM from
	// a segfault. Removing the record is the other half: after `mabo-ctl stop`,
	// the service is stopped, and a crash it happened to suffer a moment
	// earlier must not leave it reading "exited" for the rest of the afternoon.
	// A deliberately stopped service never masquerades as a crashed one.
	//
	// The mark alone is not enough; see markStoppedOnDisk below, which is what
	// covers the reaper living in a DIFFERENT mabo-ctl.
	s.markStopping(name)
	if err := s.st.RemoveExit(name); err != nil {
		emit(ev, Event{Service: name, Err: err, Msg: fmt.Sprintf(
			"could not clear the exit record: %v", err)})
	}

	pid, err := s.st.ReadPID(name)
	if err != nil {
		emit(ev, Event{Service: name, Err: err, Msg: err.Error()})
		return
	}
	if pid <= 0 {
		emit(ev, Event{Service: name, Phase: PhaseStopped, Msg: "not running"})
		return
	}
	if !state.Alive(pid) {
		// Stale pid file for a process that is already gone: clean it up, but
		// never signal on the strength of it.
		_ = s.st.RemovePID(name)
		emit(ev, Event{Service: name, Phase: PhaseStopped,
			Msg: fmt.Sprintf("not running (cleared stale pid %d)", pid)})
		return
	}

	// The recycled-pid guard. See verifyGroup: every service mabo-ctl spawns is
	// its own process-group leader, so a live pid whose group differs is not
	// ours and must not be signalled.
	pgid, err := verifyGroup(pid)
	if err != nil {
		// Refusing to signal is right; leaving the file behind is not. Without
		// the clear, every later start reported "already running" and every
		// later stop repeated this same refusal.
		emit(ev, Event{Service: name, Err: err,
			Msg: fmt.Sprintf("refusing to signal pid %d: %v", pid, err)})
		s.clearStalePID(name, err, ev)
		return
	}

	if in, ok := s.byName[name]; ok {
		s.forgetHolder(in.Port)
	}
	emit(ev, Event{Service: name, Msg: fmt.Sprintf("stopping… (pid %d, group %d)", pid, pgid)})

	// Say on DISK that this death is ours, before the signal that causes it.
	// See markStoppedOnDisk.
	s.markStoppedOnDisk(name, pid, ev)

	// SIGTERM the GROUP, not the pid: `npm run dev` spawns a child that
	// survives a bare pid kill and keeps the port bound.
	if err := signalGroup(pgid, termSignal); err != nil {
		emit(ev, Event{Service: name, Err: err, Msg: err.Error()})
		return
	}

	if s.awaitDeath(ctx, pid, s.stopGrace()) {
		s.forgetStopped(name)
		emit(ev, Event{Service: name, Phase: PhaseStopped, Msg: "stopped"})
		return
	}

	emit(ev, Event{Service: name, Msg: fmt.Sprintf(
		"did not exit within %s — escalating to SIGKILL", s.stopGrace())})
	if err := signalGroup(pgid, killSignal); err != nil {
		emit(ev, Event{Service: name, Err: err, Msg: err.Error()})
		return
	}
	if s.awaitDeath(ctx, pid, killGrace) {
		s.forgetStopped(name)
		emit(ev, Event{Service: name, Phase: PhaseStopped, Msg: "stopped (SIGKILL)"})
		return
	}

	// Leave the pid file in place: something is still there, and forgetting it
	// would strand an orphan mabo-ctl can no longer name.
	err = fmt.Errorf("%s: pid %d survived SIGKILL", name, pid)
	emit(ev, Event{Service: name, Err: err, Msg: err.Error()})
}

// awaitDeath polls until pid is gone or the budget expires, reporting whether
// it died. Polling beats waitpid here because the reaper goroutine owns the
// wait and there is only ever one waiter per child.
func (s *Supervisor) awaitDeath(ctx context.Context, pid int, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	tick := time.NewTicker(deathPollInterval)
	defer tick.Stop()
	for {
		if !state.Alive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return !state.Alive(pid)
		case <-tick.C:
		}
	}
}

// Restart stops then starts the named services.
//
// The two halves select differently, deliberately: the stop takes exactly what
// was named (see Stop), while the start still expands depends_on — so
// restarting `worker` brings its dependency `backend` back if it was down, but
// never stops a dependency that was up.
func (s *Supervisor) Restart(ctx context.Context, names []string, ev chan<- Event) error {
	if err := s.Stop(ctx, names, ev); err != nil {
		return err
	}
	return s.Start(ctx, names, ev)
}

// Reset stops everything, optionally reaps whatever still holds a declared
// port, and removes the state directory.
//
// Reaping by port is the only way to catch an orphan whose pid file went stale
// — the port is ground truth where a pid file is a guess. It is also a
// destructive act against a process mabo-ctl may not have started, which is why
// it is gated behind force and announced by name before it happens.
func (s *Supervisor) Reset(ctx context.Context, force bool, ev chan<- Event) error {
	if err := s.Stop(ctx, nil, ev); err != nil {
		return err
	}

	for _, in := range s.insts {
		if in.Port <= 0 {
			continue
		}
		if err := s.reapPort(ctx, in, force, ev); err != nil {
			return err
		}
	}

	// The wipe takes EVERY per-service lock, and that is not belt-and-braces.
	//
	// reapPort holds one service's lock while it decides whether to kill the
	// port holder; this removes the whole state tree. Between the two, a
	// concurrent Start — another terminal, or the resident console this process
	// is hosting — can spawn a service and write its pid record, and the wipe
	// then deletes that record out from under a live, setsid-detached process.
	// What is left is a running service mabo-ctl can no longer see, stop, or even
	// name: exactly the orphan this tool exists to prevent, produced by the
	// command whose job is to clean orphans up.
	if err := s.withAllServiceLocks(func() error { return s.st.Reset() }); err != nil {
		return fmt.Errorf("clear state directory: %w", err)
	}
	emit(ev, Event{Msg: "state directory cleared"})
	return nil
}

// autostartNames lists the services a bare `mabo-ctl start` should bring up, in
// declaration order.
//
// Returning names rather than instances is deliberate: the result is handed
// straight to SelectLevels, which is the one place dependency expansion lives.
func autostartNames(insts []service.Instance) []string {
	out := make([]string, 0, len(insts))
	for _, in := range insts {
		if in.Autostarts() {
			out = append(out, in.Name)
		}
	}
	return out
}

// withAllServiceLocks runs fn while holding every service's operation lock.
//
// The locks are taken in sorted name order, which is what makes this safe to
// mix with the single-lock callers: every multi-lock acquirer here uses the
// same order, and a single-lock holder cannot participate in a cycle. Sorting
// rather than ranging over the map also makes the order deterministic, so a
// deadlock introduced later fails the same way on every run rather than once a
// week.
func (s *Supervisor) withAllServiceLocks(fn func() error) error {
	names := make([]string, 0, len(s.insts))
	for _, in := range s.insts {
		names = append(names, in.Name)
	}
	sort.Strings(names)

	for _, n := range names {
		m := s.lockService(n)
		m.Lock()
		defer m.Unlock()
	}
	return fn()
}

// reapPort is one service's half of [Supervisor.Reset]: kill whatever still
// holds its declared port.
//
// It holds the SAME per-service lock startOne and stopOne take, for a reason
// that only appears once mabo-ctl is resident. `mabo-ctl serve` and the interactive
// console supervise from a process that stays alive, so a `reset` and a `start`
// can be in flight at once — and the window between Stop returning and the
// port lookup below is long enough for a start to bind that very port. The
// sweep would then find a healthy service mabo-ctl had just spawned, call it a
// process "mabo-ctl did not start", and kill it.
//
// The lock closes the window inside one process. The pid-file check closes what
// the lock cannot see: another mabo-ctl, in another terminal, holds a different
// mutex entirely, so the only shared evidence of ownership is on disk.
func (s *Supervisor) reapPort(ctx context.Context, in service.Instance, force bool, ev chan<- Event) error {
	m := s.lockService(in.Name)
	m.Lock()
	defer m.Unlock()

	// lookupPortHolder, not heldBy: this is the UNCACHED lookup. A cached
	// "free" from two seconds ago would be acted on by killing something, and
	// a cached "held" would name a pid that may already be gone. It is also the
	// package's test seam, which is the only way to exercise this path on a
	// machine whose lsof output no test can arrange.
	h := s.lookupPortHolder(in.Port)
	if h.PID <= 0 {
		return nil
	}

	// Ours, and started since the Stop above — so it is not an orphan at all.
	// Killing it here would be mabo-ctl destroying its own live service and then
	// deleting the record that proved it existed.
	if pid, err := s.livePID(in.Name); err == nil && pid == h.PID {
		emit(ev, Event{Service: in.Name, Msg: fmt.Sprintf(
			"port %d is held by pid %d, which mabo-ctl started while this reset was running — left alone",
			in.Port, h.PID)})
		return nil
	}

	if !force {
		emit(ev, Event{Service: in.Name, Msg: fmt.Sprintf(
			"port %d still held by pid %d (%s) — mabo-ctl did not start it, so it was left alone; "+
				"re-run with --force to kill it, or inspect with: %s",
			in.Port, h.PID, h.Command, LsofCommand(in.Port))})
		return nil
	}

	// Signal the LISTENER ITSELF, not its process group.
	//
	// This deliberately does not go through verifyGroup, and the reason is
	// the whole point of reaping by port. verifyGroup demands pgid == pid,
	// which holds for processes mabo-ctl spawned (Setsid makes them group
	// leaders) and is the right guard against a stale pid file. But an
	// orphan holding a port is almost never a group leader: the canonical
	// case is `npm run dev` as the leader and its node child actually
	// bound to the port. Running the lsof pid through verifyGroup therefore
	// refused to reap exactly the orphans this code exists to reap — and
	// blamed it on a "stale pid file" that was never involved, because lsof
	// is ground truth and no pid file was read.
	//
	// Signalling the single pid is also the safer half of the trade: this
	// process is one mabo-ctl did not start, so its group may be the user's
	// own shell pipeline, and killing that group would take their terminal
	// with it.
	emit(ev, Event{Service: in.Name, Msg: fmt.Sprintf(
		"killing pid %d (%s) holding port %d — mabo-ctl did not start it",
		h.PID, h.Command, in.Port)})

	if err := signalPID(h.PID, termSignal); err != nil {
		emit(ev, Event{Service: in.Name, Err: err, Msg: err.Error()})
		return nil
	}
	if s.awaitDeath(ctx, h.PID, reapGrace) {
		return nil
	}
	if err := signalPID(h.PID, killSignal); err != nil {
		emit(ev, Event{Service: in.Name, Err: err, Msg: err.Error()})
		return nil
	}
	if !s.awaitDeath(ctx, h.PID, killGrace) {
		emit(ev, Event{Service: in.Name, Msg: fmt.Sprintf(
			"pid %d still holds port %d after SIGKILL", h.PID, in.Port)})
	}
	return nil
}
