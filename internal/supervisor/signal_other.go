//go:build !unix

package supervisor

import (
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// errPlatform explains why this build cannot supervise anything.
//
// mabo-ctl is a Unix tool by design. Detached spawning, process groups, signal
// escalation and conda/nvm interpreter resolution all differ enough on Windows
// that half-supporting them would produce a supervisor that reports things
// which are not true — the exact failure class the whole project exists to
// avoid. The spec declares Windows out of scope explicitly; these stubs keep
// the module BUILDING everywhere so `go vet ./...` and editor tooling work,
// while making it impossible to run.
var errPlatform = fmt.Errorf("mabo-ctl supervises processes on Unix only (macOS and Linux); this platform is unsupported")

func processGroup(int) (int, error) { return 0, errPlatform }

func verifyGroup(int, time.Time) (int, error) { return 0, errPlatform }

func signalGroup(int, syscall.Signal) error { return errPlatform }

func signalPID(int, syscall.Signal) error { return errPlatform }

var (
	termSignal syscall.Signal
	killSignal syscall.Signal
)

func setDetached(*exec.Cmd) {}

// exitStatus reports that the wait status was never observed. Nothing on this
// platform ever spawns a child — startOne fails at checkPlatform long before —
// so there is no status to decode and no signal to name.
func exitStatus(error) (int, string) { return -1, "" }
