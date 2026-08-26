//go:build !unix

package supervisor

import (
	"errors"

	"github.com/maborak/mabo-ctl/internal/service"
)

// On platforms without the unix terminal plumbing, tty: is a start-time
// refusal naming the platform, never a silent fallback to /dev/null stdin —
// a service that expects a pty must not half-work.
func (s *Supervisor) spawnTTY(_ service.Instance, _, _ string) (int, error) {
	return 0, errors.New("tty: terminals are supported on macOS and Linux only")
}
