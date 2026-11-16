package supervisor

import (
	"strconv"
	"strings"
)

// Usage is what one service's process group costs right now.
type Usage struct {
	// CPU is the summed %CPU of the group, as ps reports it: on Linux the
	// average over each process's lifetime, on macOS a decaying recent
	// average. Either way 100 is one full core.
	CPU float64
	// RSS is the summed resident memory in bytes.
	RSS int64
}

// parseGroupUsage sums `pgid pcpu rss(KiB)` lines per process group. Lines
// that do not parse are skipped.
func parseGroupUsage(out string) map[int]Usage {
	usage := make(map[int]Usage)
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		pgid, err1 := strconv.Atoi(f[0])
		cpu, err2 := strconv.ParseFloat(f[1], 64)
		rss, err3 := strconv.ParseInt(f[2], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || pgid <= 0 {
			continue
		}
		u := usage[pgid]
		u.CPU += cpu
		u.RSS += rss * 1024
		usage[pgid] = u
	}
	return usage
}
