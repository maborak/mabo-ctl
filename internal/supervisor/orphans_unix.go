//go:build unix

package supervisor

import (
	"bufio"
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
	ctx, cancel := context.WithTimeout(context.Background(), orphanLookupTimeout)
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
