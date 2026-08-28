//go:build unix

package supervisor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// detachAttr returns the spawn attributes that make a supervised child outlive
// mabo-ctl.
//
// Setsid puts the child in a new session, which makes it both a session leader
// and a process-group leader. Two things follow, and the rest of this package
// depends on both:
//
//   - The child does not receive the terminal's SIGHUP or SIGINT, so quitting
//     mabo-ctl — or closing the terminal it was started from — leaves the service
//     running. That is the whole point of a detached supervisor.
//   - The child's process-group id EQUALS its pid. [verifyGroup] relies on this
//     to tell a live process we started from an unrelated process that has
//     since inherited a recycled pid.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// processGroup returns the process-group id of pid.
func processGroup(pid int) (int, error) {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return 0, fmt.Errorf("getpgid %d: %w", pid, err)
	}
	return pgid, nil
}

// verifyGroup resolves pid's process group and refuses to return one that is
// not safe to signal.
//
// This is the guard against the most destructive bug a supervisor can have.
// Signalling a process GROUP is necessary — `npm run dev` spawns a child that
// survives a bare pid kill and keeps the port bound — but a group signal
// derived from a stale pid file is a blast radius, not a fix: pids are
// recycled, so yesterday's pid may today belong to something the user cares
// about, and killing its group takes the whole tree down.
//
// Three checks make that unrepresentable:
//
//   - pid must be greater than 1. Signalling group 0 means "my own group",
//     which is mabo-ctl itself, and group 1 is init.
//   - pgid must EQUAL pid. Every process we spawn is a group leader by
//     construction (see [detachAttr]), so a live pid whose group id differs is
//     definitionally not ours. The converse does NOT hold — every setsid
//     process is its own group leader, ours or not — which is why the third
//     check exists.
//   - startedAt must match the process's REAL start time, as the kernel
//     reports it, within [startSkew]. A group-leader pid alone is satisfied by
//     every tmux pane and container init on the machine; only the spawn time
//     recorded when mabo-ctl actually forked the child distinguishes ours from
//     a recycled pid that merely looks like one. A zero startedAt (a legacy
//     pid file that predates recorded spawn times) skips this check rather
//     than fail every stop of an already-running stack after an upgrade.
//
// When the real start time cannot be read at all, the check refuses: a stop we
// decline with a clear message is recoverable, a group we signalled in error
// is not.
func verifyGroup(pid int, startedAt time.Time) (int, error) {
	if pid <= 1 {
		return 0, fmt.Errorf("%w: pid %d", ErrUnsafeSignal, pid)
	}
	pgid, err := processGroup(pid)
	if err != nil {
		return 0, err
	}
	if pgid <= 1 {
		return 0, fmt.Errorf("%w: pid %d resolved to process group %d",
			ErrUnsafeSignal, pid, pgid)
	}
	if pgid != pid {
		return 0, fmt.Errorf(
			"%w: pid %d is in process group %d, but every process mabo-ctl spawns "+
				"is its own group leader — this pid file is stale and the pid has "+
				"been recycled by an unrelated process",
			ErrUnsafeSignal, pid, pgid)
	}
	if !startedAt.IsZero() {
		real, err := processStartTime(pid)
		if err != nil {
			return 0, fmt.Errorf(
				"%w: pid %d is a group leader, but its real start time could not be "+
					"read to confirm the pid record: %v",
				ErrUnsafeSignal, pid, err)
		}
		if diff := real.Sub(startedAt); diff > startSkew || diff < -startSkew {
			return 0, fmt.Errorf(
				"%w: pid %d is a group leader that started at %s, but the pid record "+
					"says mabo-ctl spawned it at %s — the recorded pid was recycled",
				ErrUnsafeSignal, pid,
				real.Format(time.RFC3339), startedAt.Format(time.RFC3339))
		}
	}
	return pgid, nil
}

// startSkew is how far the pid record's spawn time may sit from the start time
// the kernel reports and the identity still be believed. Both sides come from
// the same clock, but the kernel's record is truncated to the second while the
// record's is not, so a spawn at 21:52:51.9 can be reported as 21:52:51.
const startSkew = 2 * time.Second

// processStartTime asks the kernel when pid was started, via
// `ps -o lstart= -p PID` with a pinned C locale. It is the one portable
// channel for the answer on both macOS and Linux; parsing `ps` is pinned to a
// single format by forcing LC_ALL=C, because the default lstart layout is
// locale-dependent on both platforms.
//
// The two layouts differ between platforms — macOS prints the day of month
// before the month, Linux prints the month first — so both are tried.
func processStartTime(pid int) (time.Time, error) {
	ps, err := psPath()
	if err != nil {
		return time.Time{}, err
	}
	cmd := exec.Command(ps, "-o", "lstart=", "-p", strconv.Itoa(pid))
	// No inherited environment: LC_ALL alone pins the format, so a caller's
	// locale cannot change what this has to parse.
	cmd.Env = []string{"LC_ALL=C"}
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, fmt.Errorf("ps -o lstart= -p %d: %w", pid, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 5 {
		return time.Time{}, fmt.Errorf("ps -o lstart= -p %d printed %q", pid, strings.TrimSpace(string(out)))
	}
	var layouts [2]struct {
		layout string
		value  string
	}
	if _, convErr := strconv.Atoi(fields[1]); convErr == nil {
		// macOS: Thu 27 Aug 20:42:32 2026
		layouts[0].layout, layouts[0].value = "2 Jan 15:04:05 2006",
			fields[1]+" "+fields[2]+" "+fields[3]+" "+fields[4]
	} else {
		// Linux: Thu Aug 27 20:42:32 2026
		layouts[0].layout, layouts[0].value = "Jan 2 15:04:05 2006",
			fields[1]+" "+fields[2]+" "+fields[3]+" "+fields[4]
	}
	for _, l := range layouts {
		if l.layout == "" {
			continue
		}
		// ParseInLocation, not Parse: ps prints LOCAL time and carries no
		// zone, and Parse's UTC assumption would offset every comparison by
		// the machine's UTC difference.
		if t, perr := time.ParseInLocation(l.layout, l.value, time.Local); perr == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("ps -o lstart= -p %d printed %q, which matches neither known lstart layout",
		pid, strings.Join(fields, " "))
}

// psPath resolves the ps binary once per process. It is resolved explicitly
// rather than left to exec.Command's implicit lookup so a missing ps fails
// this check loudly instead of surfacing as an unrelated exec error.
var psPath = sync.OnceValues(func() (string, error) {
	return exec.LookPath("ps")
})

// CheckIdentity reports whether a live pid looks like one mabo-ctl spawned:
// its own process-group leader, started when the pid record says, not init,
// not privileged. It is the same test signalling runs before every kill,
// exported for read-only diagnostics — `mabo-ctl doctor` asks "is this pid
// file still honest?" without ever signalling anything. startedAt is the pid
// record's spawn time; zero means "not recorded" and skips that comparison.
func CheckIdentity(pid int, startedAt time.Time) error {
	_, err := verifyGroup(pid, startedAt)
	return err
}

// signalPID sends sig to exactly ONE process, never to a group.
//
// This is the counterpart to [signalGroup], and choosing between them is a
// safety decision rather than a style one. Use signalGroup when the pid came
// from OUR pid file, because we spawned that process as a group leader and the
// group is precisely its descendants. Use signalPID when the pid came from
// lsof — that is, from `reset` reaping whatever holds a declared port. There,
// the process is one we did not start, so its process group is not ours to
// reason about: a listener running inside the user's own shell pipeline shares
// the SHELL's group, and killing that group would kill the user's terminal
// session along with it.
//
// Refusing pid <= 1 keeps us off init, and refusing our own pid keeps a
// misparsed lsof line from making mabo-ctl kill itself.
func signalPID(pid int, sig syscall.Signal) error {
	if pid <= 1 {
		return fmt.Errorf("%w: refusing to signal pid %d", ErrUnsafeSignal, pid)
	}
	if pid == os.Getpid() {
		return fmt.Errorf("%w: refusing to signal mabo-ctl itself (pid %d)", ErrUnsafeSignal, pid)
	}
	if err := syscall.Kill(pid, sig); err != nil {
		return fmt.Errorf("signal %v to pid %d: %w", sig, pid, err)
	}
	return nil
}

// signalGroup sends sig to every process in pgid by negating the id, which is
// the kernel's convention for "the whole group".
func signalGroup(pgid int, sig syscall.Signal) error {
	if pgid <= 1 {
		return fmt.Errorf("%w: refusing to signal process group %d",
			ErrUnsafeSignal, pgid)
	}
	if err := syscall.Kill(-pgid, sig); err != nil {
		return fmt.Errorf("signal %v to group %d: %w", sig, pgid, err)
	}
	return nil
}

// termSignal and killSignal name the two stages of the stop escalation.
var (
	termSignal = syscall.SIGTERM
	killSignal = syscall.SIGKILL
)

// setDetached applies detachAttr to cmd.
func setDetached(cmd *exec.Cmd) { cmd.SysProcAttr = detachAttr() }

// signalNames spells the signals a supervised dev process realistically dies
// from the way an operator writes them.
//
// syscall.Signal.String() renders SIGKILL as "killed" and SIGSEGV as
// "segmentation fault", which are the wrong tokens to put in a record someone
// will grep, and are not the names any documentation of the process uses.
var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP:  "SIGHUP",
	syscall.SIGINT:  "SIGINT",
	syscall.SIGQUIT: "SIGQUIT",
	syscall.SIGILL:  "SIGILL",
	syscall.SIGABRT: "SIGABRT",
	syscall.SIGFPE:  "SIGFPE",
	syscall.SIGKILL: "SIGKILL",
	syscall.SIGBUS:  "SIGBUS",
	syscall.SIGSEGV: "SIGSEGV",
	syscall.SIGPIPE: "SIGPIPE",
	syscall.SIGALRM: "SIGALRM",
	syscall.SIGTERM: "SIGTERM",
	syscall.SIGUSR1: "SIGUSR1",
	syscall.SIGUSR2: "SIGUSR2",
}

// signalName returns the conventional name of sig, falling back to its number
// so an unusual signal is still recorded rather than dropped.
func signalName(sig syscall.Signal) string {
	if name, ok := signalNames[sig]; ok {
		return name
	}
	return fmt.Sprintf("signal %d", int(sig))
}

// exitStatus reduces the error cmd.Wait returned to the two things an exit
// record holds: the exit code, and the name of the signal that killed the
// process.
//
// A signalled child has no exit status at all, so it reports -1 and the signal
// name; a child that exited on its own reports its code and no signal. A nil
// error is a clean exit 0 — which is still a death worth recording, because a
// dev server that exits 0 on its own has still stopped serving.
//
// An error that is not an *exec.ExitError means the wait itself failed and the
// status was never observed: -1 with no signal, which exitDetail renders as
// "exit status unknown" rather than inventing a code.
func exitStatus(err error) (code int, signal string) {
	if err == nil {
		return 0, ""
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return -1, ""
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -1, signalName(ws.Signal())
	}
	return ee.ExitCode(), ""
}
