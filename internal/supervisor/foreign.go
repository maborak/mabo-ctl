package supervisor

import (
	"bufio"
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
//
// Known limit: a service whose dir: lies outside its own checkout, or that
// changes directory after starting, has no .mabo-ctl above its cwd and is not
// recognised. The guard can only ever spare a process, never cause a kill, so
// a missed match is the old behaviour, not a new risk.
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

// processCwd returns pid's working directory, or "" when it cannot be read.
func processCwd(pid int) string {
	if pid <= 0 {
		return ""
	}
	return processCwds([]int{pid})[pid]
}

// processCwds reports each pid's working directory, from one
// `lsof -a -d cwd -p PID,PID… -Fpn`. A pid lsof cannot read is simply absent;
// a missing lsof yields an empty map.
func processCwds(pids []int) map[int]string {
	lsof, err := lsofPath()
	if err != nil || len(pids) == 0 {
		return nil
	}
	list := make([]string, len(pids))
	for i, p := range pids {
		list[i] = strconv.Itoa(p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), lsofTimeout)
	defer cancel()
	// lsof exits 1 when any listed pid is unreadable, yet still prints the
	// rest, so the output is used whatever the exit status.
	out, _ := exec.CommandContext(ctx, lsof, "-a", "-d", "cwd", "-p", strings.Join(list, ","), "-Fpn").Output()
	return parseCwds(string(out))
}

// parseCwds reads lsof's -Fpn field output: a "p<pid>" line starts a process,
// and the "n<path>" line after it is that process's cwd.
func parseCwds(out string) map[int]string {
	cwds := map[int]string{}
	pid := 0
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "n") && pid > 0:
			cwds[pid] = line[1:]
		}
	}
	return cwds
}

// sameDir reports whether a and b name the same directory, through symlinks
// (macOS's /tmp is /private/tmp).
func sameDir(a, b string) bool { return realPath(a) == realPath(b) }
