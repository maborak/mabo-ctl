//go:build unix

package state

import (
	"errors"
	"syscall"
)

// Alive reports whether a process with the given pid currently exists.
//
// It sends signal 0, which performs the kernel's permission and existence
// checks without delivering anything. A pid owned by another user reports true:
// EPERM means "it exists but is not yours", which is exactly the case mabo-ctl
// must not mistake for "not running" when deciding whether a port holder is a
// live process.
//
// A non-positive pid always reports false and is never passed to the kernel:
// signal 0 to pid 0 addresses the caller's entire process group and to a
// negative pid addresses another group, so a stale or corrupt pid file must not
// reach this call. Alive does not distinguish a zombie from a running process;
// a not-yet-reaped child still reports true.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return true
	case errors.Is(err, syscall.EPERM):
		return true
	default:
		return false
	}
}
