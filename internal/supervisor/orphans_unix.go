//go:build unix

package supervisor

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// orphanLookupTimeout bounds each ps and lsof call made while deciding whether
// a leaderless group is ours.
const orphanLookupTimeout = 5 * time.Second

// groupMembers lists the pids in process group pgid, from one
// `ps -A -o pid=,pgid=` with a pinned C locale. A failed lookup returns the
// error: the caller refuses to signal rather than guess.
func groupMembers(pgid int) ([]int, error) {
	ps, err := psPath()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), orphanLookupTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ps, "-A", "-o", "pid=,pgid=")
	cmd.Env = []string{"LC_ALL=C"}
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseGroupMembers(string(out), pgid), nil
}

// parseGroupMembers picks the pids whose group is pgid out of `ps -o pid=,pgid=`
// output. Lines that do not parse are skipped.
func parseGroupMembers(out string, pgid int) []int {
	var pids []int
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		g, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil && g == pgid && pid > 1 {
			pids = append(pids, pid)
		}
	}
	return pids
}
