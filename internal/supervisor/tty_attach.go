//go:build darwin || linux

package supervisor

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
)

// ErrNoRelay reports that no terminal broker is listening for this service:
// it has not been started since boot, has died and been cleaned up, or was
// never declared with tty: at all.
var ErrNoRelay = errors.New("no terminal relay is running for this service")

// detachFilterCopy copies src to dst until EOF, consuming [ttyDetachByte]
// instead of relaying it and returning nil when seen. The detach key belongs
// to mabo-ctl, never to the attached program: handing Ctrl-Q through would let
// one byte end two programs' idea of a session.
func detachFilterCopy(dst io.Writer, src io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		n, rerr := src.Read(buf)
		for i := 0; i < n; i++ {
			if buf[i] == ttyDetachByte {
				// Keystrokes typed before the detach still belong to the
				// service: flush the prefix, then keep the key private.
				if i > 0 {
					if _, werr := dst.Write(buf[:i]); werr != nil {
						return werr
					}
				}
				return nil
			}
		}
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr != nil {
			return rerr
		}
	}
}

// AttachTTY connects to svc's relay socket and shuttles between it and the
// caller's terminal until the far side closes, this side hangs up, or
// ttyDetachByte arrives on stdin. When stdin is a real terminal its line
// discipline is put into raw mode for the session and restored on every exit
// path — including the far end dying mid-keystroke.
//
// stdoutErroneous writes are direct: the hub already mirrors everything to the
// service's log, so nothing here needs tee-ing again.
func AttachTTY(sockPath string, stdin *os.File, stdout io.Writer) error {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return fmt.Errorf("%w (%s); start the service first", ErrNoRelay, sockPath)
		}
		return fmt.Errorf("attach %s: %w", sockPath, err)
	}
	defer func() { _ = conn.Close() }()

	var saved *syscall.Termios
	if stdin != nil { // nil-guarded so pipe-backed stdins (tests) stay usable
		if t, terr := termiosGet(stdin.Fd()); terr == nil {
			saved = t
			raw := *t
			makeRaw(&raw)
			if serr := termiosSet(stdin.Fd(), &raw); serr != nil {
				saved = nil // could not switch; do not pretend to restore
			}
		}
	}
	if saved != nil {
		defer func() { _ = termiosSet(stdin.Fd(), saved) }()
	}

	done := make(chan error, 2)
	go func() {
		_, cerr := io.Copy(stdout, conn)
		done <- cerr
	}()
	go func() {
		cerr := detachFilterCopy(conn, stdin)
		done <- cerr
	}()

	err = <-done
	_ = conn.Close()
	<-done // unblock the other pump; its next read/write fails on close
	return err
}
