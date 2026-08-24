//go:build !unix

package service

import (
	"errors"
	"io/fs"
	"runtime"
)

// checkPlatform reports that this build cannot resolve service runtimes.
// mabo-ctl supervises processes with process groups, POSIX signals and
// unix-shaped interpreter layouts (<base>/envs/<env>/bin, <nvm>/versions/node);
// none of that has a meaningful equivalent here, so the package refuses rather
// than half-working. The module still builds so a cross-compile does not fail
// mysteriously.
func checkPlatform() error {
	return errors.New("unsupported platform: mabo-ctl supports macOS and Linux only, not " + runtime.GOOS)
}

// executableMode always reports false on an unsupported platform; checkPlatform
// fails before it is consulted.
func executableMode(fs.FileMode) bool { return false }
