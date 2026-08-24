//go:build !unix

package state

import "errors"

// ErrUnsupportedPlatform reports that mabo-ctl's process handling requires a Unix
// platform. Process groups, signals and detached spawning all differ elsewhere,
// so mabo-ctl supports macOS and Linux only; this build exists so the module
// still compiles and its pure parts (config parsing, rendering) remain testable.
var ErrUnsupportedPlatform = errors.New("mabo-ctl: process supervision is unsupported on this platform (macOS and Linux only)")

// Alive reports whether a process with the given pid currently exists. On a
// non-Unix platform mabo-ctl cannot supervise processes at all — see
// ErrUnsupportedPlatform — so it always reports false rather than guessing.
func Alive(pid int) bool {
	_ = pid
	return false
}
