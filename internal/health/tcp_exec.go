package health

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"time"
)

// ProbeTCP dials addr once and reports whether the connection succeeded.
//
// A connected socket IS readiness for this probe: it is closed immediately and
// nothing is ever written to or read from it, because a supervisor asking "is
// the port open?" must not send bytes a non-HTTP protocol would have to parse.
// Any dial error — refused, timeout, unresolvable host — is not ready, and
// Err carries the dial failure verbatim.
//
// The network is always "tcp", so Happy Eyeballs tries every resolved address;
// see [newClient] for why the address family is never forced.
func ProbeTCP(ctx context.Context, addr string) Result {
	start := time.Now()
	if addr == "" {
		return Result{Elapsed: time.Since(start), Err: fmt.Errorf("health: empty tcp address")}
	}

	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return Result{Elapsed: time.Since(start), Err: fmt.Errorf("health: tcp %s: %w", addr, err)}
	}
	if err := conn.Close(); err != nil {
		return Result{OK: true, Elapsed: time.Since(start),
			Err: fmt.Errorf("health: close tcp %s: %w", addr, err)}
	}
	return Result{OK: true, Elapsed: time.Since(start)}
}

// WaitTCP polls [ProbeTCP] until the socket connects, the supervised process
// dies, or ctx expires. It shares [Wait]'s loop, liveness discipline and error
// wrapping; describe names the target in errors as declared ("tcp:…" display
// form), while addr is what is actually dialled — the two differ by exactly the
// prefix, and probing the display form would dial a host named "tcp".
func WaitTCP(ctx context.Context, describe, addr string, alive func() bool) Result {
	return wait(ctx, describe, func(c context.Context) Result { return ProbeTCP(c, addr) }, alive)
}

// maxExecDiagBytes caps how much of a failed exec probe's output is kept as a
// diagnostic. The exit code is the verdict; output is evidence for a human,
// never something mabo-ctl interprets.
const maxExecDiagBytes = 512

// ProbeExec runs argv once in dir with env and reports whether it exited 0.
//
// This is deliberately the WHOLE contract. There is no shell, no chaining, no
// output parsing, no success/failure thresholds: one process, one hard timeout
// of [ProbeTimeout], exit code 0 means ready and anything else does not. An
// exec probe is one step from "not a task runner", and staying on this side of
// that line is what keeps it a probe.
//
// argv comes from mabo-ctl.yaml, which is arbitrary code execution BY DESIGN —
// the same standing fact as every service's cmd. Output is captured only up to
// [maxExecDiagBytes] and only so a failing probe can say what it saw.
func ProbeExec(ctx context.Context, dir string, env []string, argv []string) Result {
	start := time.Now()
	switch {
	case len(argv) == 0:
		return Result{Elapsed: time.Since(start), Err: fmt.Errorf("health: exec is empty")}
	case argv[0] == "":
		return Result{Elapsed: time.Since(start), Err: fmt.Errorf("health: exec[0] is empty")}
	}

	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 -- argv comes from mabo-ctl.yaml, which is arbitrary code execution BY DESIGN; see THREAT_MODEL.md
	cmd.Dir = dir
	cmd.Env = env

	var out cappedBuffer
	out.Limit = maxExecDiagBytes
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		return Result{Elapsed: time.Since(start), Err: fmt.Errorf("health: exec %s: %w", argv[0], err)}
	}
	err := cmd.Wait()
	elapsed := time.Since(start)
	switch {
	case err == nil:
		return Result{OK: true, Elapsed: elapsed}

	case cmd.ProcessState != nil && cmd.ProcessState.Exited():
		// The child RAN TO COMPLETION — just slower than the budget. Its exit
		// status and captured output are real observations; reporting them as
		// "timed out" would throw away the one diagnostic this probe exists
		// to deliver, exactly under the load where they matter most.
		msg := fmt.Sprintf("exit status %d", cmd.ProcessState.ExitCode())
		if out.Len() > 0 {
			msg += " (output: " + out.String() + ")"
		}
		return Result{Elapsed: elapsed,
			Err: fmt.Errorf("health: exec %s overran %s: %s", argv[0], ProbeTimeout, msg)}

	case ctx.Err() != nil:
		return Result{Elapsed: elapsed, Err: fmt.Errorf(
			"health: exec %s timed out after %s (output: %s)", argv[0], ProbeTimeout, out.String())}

	default:
		msg := fmt.Sprintf("exit status %d", cmd.ProcessState.ExitCode())
		if out.Len() > 0 {
			msg += " (output: " + out.String() + ")"
		}
		return Result{Elapsed: elapsed, Err: fmt.Errorf("health: exec %s: %s", argv[0], msg)}
	}
}

// WaitExec polls [ProbeExec] until it exits 0, the supervised process dies, or
// ctx expires. Each attempt gets a fresh process under its own hard timeout;
// nothing survives between attempts.
func WaitExec(ctx context.Context, describe string, dir string, env []string, argv []string, alive func() bool) Result {
	return wait(ctx, describe, func(c context.Context) Result {
		return ProbeExec(c, dir, env, argv)
	}, alive)
}

// cappedBuffer is an io.Writer that keeps at most Limit bytes and discards the
// rest, so a chatty probe command cannot grow memory without bound.
type cappedBuffer struct {
	buf   bytes.Buffer
	Limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.Limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		b.buf.Write(p)
	}
	return len(p), nil // the discard is deliberate; the child must not block
}

func (b *cappedBuffer) Len() int       { return b.buf.Len() }
func (b *cappedBuffer) String() string { return b.buf.String() }
