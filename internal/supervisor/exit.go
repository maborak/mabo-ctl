package supervisor

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/maborak/mabo-ctl/internal/state"
)

// reapWait bounds how long startOne waits for the reaper to hand over the wait
// status of a child it has just seen die.
//
// In practice the value is always already there. state.Alive reports a
// not-yet-reaped zombie as ALIVE, so the death branch in startOne cannot be
// reached until the reaper's waitpid has returned; only the handover across the
// channel is left, which is microseconds. The bound exists so a lost wakeup
// costs a pause instead of a wedged start.
const reapWait = 2 * time.Second

// exitInfo is what waiting on a child taught the reaper: the two facts the
// kernel reports exactly once, and when it reported them.
type exitInfo struct {
	code   int
	signal string
	at     time.Time
}

// awaitReaped takes the wait status the reaper published, or gives up after
// [reapWait] and reports that it was never observed.
//
// Giving up reports code -1 with no signal, which renders as "exit status
// unknown". That is the honest answer and it is deliberately not 0: a supervisor
// that reports a clean exit it did not observe is inventing the one fact the
// record exists to carry.
func awaitReaped(status <-chan exitInfo) exitInfo {
	timer := time.NewTimer(reapWait)
	defer timer.Stop()
	select {
	case info := <-status:
		return info
	case <-timer.C:
		return exitInfo{code: -1, at: time.Now()}
	}
}

// reapChild waits on a spawned child, publishes what it learned on status, and then
// records the death.
//
// The wait result is the whole point. cmd.Wait has always been called here and
// its result has always been thrown away — and it is the only place the exit
// status of a supervised process exists, because the kernel hands it to
// whoever waits, once, and after that it is gone. Persisting it is what lets a
// `mabo-ctl status` in a different terminal, minutes later, say "exit code 1, 4m
// ago" instead of reporting a crashed service as one that was never started.
//
// status is buffered and is read only by the startOne that spawned this child,
// and only when that startOne watches the process die during startup. Nothing
// blocks on it.
func (s *Supervisor) reapChild(svc string, pid int, startedAt time.Time, cmd *exec.Cmd, status chan<- exitInfo) {
	code, signal := exitStatus(cmd.Wait())
	info := exitInfo{code: code, signal: signal, at: time.Now()}
	status <- info
	s.recordExit(svc, pid, startedAt, info)
}

// recordExit writes the exit record for a child that has just been reaped.
//
// It takes the per-service lock, so it is serialised against startOne and
// stopOne and never races either of them. Both of those hold the lock across
// their whole body, which is what makes these guards decisive rather than
// best-effort:
//
//   - a deliberate stop is not a crash, and cmd.Wait cannot tell the two apart:
//     a child killed by our own SIGTERM and one that segfaulted return the same
//     shape of error;
//   - a pid file that no longer names this pid belongs to a LATER run, and
//     writing a record for a superseded one would report a death the next start
//     already put behind it;
//   - a record already written for this same pid is startOne's, and startOne's
//     is better: it explains an EMPTY log, which is the single most confusing
//     failure a supervisor can report, and it carries the same wait status
//     because it got it from this goroutine.
func (s *Supervisor) recordExit(svc string, pid int, startedAt time.Time, info exitInfo) {
	lk := s.lockService(svc)
	lk.Lock()
	defer lk.Unlock()

	if s.wasStopped(svc) {
		return
	}
	if cur, err := s.st.ReadPIDRecord(svc); err != nil || cur.PID != pid {
		return
	}
	if prev, ok, err := s.st.ReadExit(svc); err == nil && ok && prev.PID == pid {
		return
	}

	// Nowhere to report a failure to: this runs in a detached reaper goroutine
	// long after the command that spawned it returned, and there is no event
	// channel guaranteed to still have a reader. A missing record degrades to
	// the previous behaviour — the service reads as stopped — which is exactly
	// what a failed write should degrade to.
	_ = s.st.WriteExit(svc, state.ExitRecord{
		PID:       pid,
		ExitCode:  info.code,
		Signal:    info.signal,
		StartedAt: startedAt,
		EndedAt:   info.at,
		LogTail:   s.logTail(svc, failLogLines),
	})
}

// recordStartupDeath writes the exit record for a service that died while
// startOne was waiting for it to become ready.
//
// What startOne has and the reaper does not is the detail it already computed
// in order to print it: the log tail, including the explicit "the log is empty"
// text for a process that could not even be executed. Writing it here is what
// makes the PERSISTENT artifact of a failed start — the status block, and every
// `mabo-ctl status` after it — agree with the transient event the command printed
// three lines earlier, instead of reporting a service that was never started.
//
// The record is marked Startup, which is the only place that flag is ever set:
// this is by construction the one caller that KNOWS the service never became
// ready, because it is the code that was waiting for it. That is what lets
// `mabo-ctl status`, minutes later and in another terminal, report `failed`
// rather than `exited` — "it has never come up" and "it ran and then died" are
// different problems and the record is the only thing that still remembers
// which one happened.
//
// It must be called with the service's lock held, which startOne holds.
func (s *Supervisor) recordStartupDeath(svc string, pid int, startedAt time.Time, info exitInfo, detail string) {
	_ = s.st.WriteExit(svc, state.ExitRecord{
		PID:       pid,
		ExitCode:  info.code,
		Signal:    info.signal,
		StartedAt: startedAt,
		EndedAt:   info.at,
		LogTail:   detail,
		Startup:   true,
	})
}

// exitDetail renders an exit record as the one-line explanation the status
// block shows, with the log tail underneath it when there is one.
//
// The line answers the two questions an operator asks on seeing a service gone:
// how it died, and how long ago. "4m ago" beats a timestamp here because the
// question is recency — whether this happened during what you were just doing.
func exitDetail(rec state.ExitRecord) string {
	var head string
	switch {
	case rec.Signal != "":
		head = "killed by " + rec.Signal
	case rec.ExitCode < 0:
		head = "exit status unknown"
	default:
		head = fmt.Sprintf("exit code %d", rec.ExitCode)
	}
	if !rec.EndedAt.IsZero() {
		head += ", " + ago(time.Since(rec.EndedAt)) + " ago"
	}
	if tail := strings.TrimRight(rec.LogTail, "\n"); tail != "" {
		head += "\n" + tail
	}
	return head
}

// ago renders an elapsed duration at one unit of precision, which is all a
// "how long ago" reading is ever worth. A negative duration — a clock that
// moved backwards between the write and the read — reads as "0s" rather than
// as a death in the future.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		if d < 0 {
			d = 0
		}
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}
