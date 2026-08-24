//go:build unix

package supervisor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
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
// Two checks make that unrepresentable:
//
//   - pgid must be greater than 1. Signalling group 0 means "my own group",
//     which is mabo-ctl itself, and group 1 is init.
//   - pgid must EQUAL pid. Every process we spawn is a group leader by
//     construction (see [detachAttr]), so a live pid whose group id differs is
//     definitionally not ours — it is a recycled pid, and we must not touch it.
func verifyGroup(pid int) (int, error) {
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
	return pgid, nil
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
