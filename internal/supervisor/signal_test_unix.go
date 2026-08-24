//go:build unix

package supervisor

import (
	"os/signal"
	"syscall"
)

// signalIgnoreTERM makes the calling process ignore SIGTERM.
//
// It exists only for the helper process in supervisor_test.go, which must
// survive the polite stage of the stop escalation so the test can prove that
// SIGKILL actually follows. It lives in a build-tagged file rather than the
// test file so that the test file itself stays platform-neutral.
func signalIgnoreTERM() { signal.Ignore(syscall.SIGTERM) }
