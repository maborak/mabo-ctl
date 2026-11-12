package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/maborak/mabo-ctl/internal/state"
)

// otherCheckout returns the root of ANOTHER mabo-ctl checkout whose pid
// records claim the process holding a port — pid itself, or the process group
// it belongs to — or "" when there is none or it cannot be told.
//
// It exists for reset's port reaping. Two checkouts of one repository (git
// worktrees, a second clone) declare the same ports, so the orphan `reset
// --force` finds on a declared port can be the other checkout's live service,
// which that mabo-ctl started and still tracks. Killing it would be this
// mabo-ctl destroying a service it does not own and leaving the other one's
// records pointing at nothing. The holder's working directory leads to the
// nearest .mabo-ctl above it, and only a pid record there counts as a claim:
// a directory that merely sits inside some repository proves nothing.
func (s *Supervisor) otherCheckout(pid int) string {
	cwd := processCwd(pid)
	if cwd == "" {
		return ""
	}
	pgid, _ := processGroup(pid)
	for dir := cwd; ; {
		if fi, err := os.Stat(filepath.Join(dir, ".mabo-ctl", "pids")); err == nil && fi.IsDir() {
			if s.cfg != nil && sameDir(dir, s.cfg.Root) {
				return ""
			}
			if recordsClaim(dir, pid, pgid) {
				return dir
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// otherCheckoutFn is [Supervisor.otherCheckout] unless a test replaced it.
func (s *Supervisor) otherCheckoutFn(pid int) string {
	if s.checkoutLookup != nil {
		return s.checkoutLookup(pid)
	}
	return s.otherCheckout(pid)
}

// recordsClaim reports whether a pid record of the checkout at root names pid
// or the group leader pgid. Reading goes through internal/state, which owns
// the record format; nothing is written.
func recordsClaim(root string, pid, pgid int) bool {
	d := &state.Dir{Root: root}
	entries, err := os.ReadDir(d.PIDsDir())
	if err != nil {
		return false
	}
	for _, e := range entries {
		svc, ok := strings.CutSuffix(e.Name(), ".pid")
		if !ok || e.IsDir() {
			continue
		}
		rec, err := d.ReadPIDRecord(svc)
		if err != nil {
			continue
		}
		if rec.PID == pid || (pgid > 0 && rec.PID == pgid) {
			return true
		}
	}
	return false
}

// processCwd returns pid's working directory via lsof, the tool the port
// guard already depends on, or "" when it cannot be read.
func processCwd(pid int) string {
	lsof, err := lsofPath()
	if err != nil || pid <= 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), lsofTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, lsof, "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(line, "n"); ok && filepath.IsAbs(p) {
			return p
		}
	}
	return ""
}

// sameDir reports whether a and b name the same directory, through symlinks
// (macOS's /tmp is /private/tmp).
func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}
