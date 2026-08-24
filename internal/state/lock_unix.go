//go:build unix

package state

import (
	"fmt"
	"os"
	"syscall"
)

// withFileLock runs fn while holding an exclusive advisory lock on path.
//
// It exists for one read-modify-write: run.env is read, merged with the ports
// the caller resolved, and written back. Two mabo-ctl invocations in two
// terminals — or a `mabo-ctl serve` and a one-shot `mabo-ctl start` — interleave
// those steps freely, and the second write then rebuilds the file from a
// snapshot taken before the first one landed. The lost update is silent and
// outlives both processes: a later `status --json` reads a port nothing is
// listening on and reports a healthy service as slow, which is exactly the
// state-versus-truth divergence the state package exists to prevent.
//
// The lock is advisory and taken on a SEPARATE .lock file rather than on
// run.env itself, because run.env is replaced by an atomic rename: a lock held
// on the old inode says nothing about the new one, so every writer would be
// locking a file that is about to stop being the file.
//
// It is best-effort by design. A lock file that cannot be created — a read-only
// filesystem, a directory that vanished — runs fn anyway rather than failing the
// operation, because refusing to record a port mabo-ctl has already bound would
// turn a rare race into a common outage. Flock on a local filesystem is the
// case that matters and the case that works.
func withFileLock(path string, fn func() error) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return fn()
	}
	defer func() { _ = f.Close() }()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fn()
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	if err := fn(); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}
