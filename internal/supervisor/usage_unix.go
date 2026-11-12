//go:build unix

package supervisor

import (
	"context"
	"os/exec"
	"time"
)

// usageTimeout bounds the one ps a status read runs for CPU and memory.
const usageTimeout = 2 * time.Second

// groupUsage reads CPU and resident memory for every process on the machine
// with one `ps -A -o pgid=,pcpu=,rss=` and sums them per process group. A
// service's group is the whole tree it spawned — `npm run dev` and the node
// child doing the work — so the sum is what the service costs.
//
// It returns nil when ps cannot run: usage is observation, and a missing
// number must never fail a status read. A cancelled ctx — a browser that
// stopped waiting for the status — stops ps too: unlike the port guard, a
// usage figure nobody reads is safe to abandon.
func groupUsage(ctx context.Context) map[int]Usage {
	ps, err := psPath()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, usageTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ps, "-A", "-o", "pgid=,pcpu=,rss=")
	cmd.Env = []string{"LC_ALL=C"} // a decimal point, whatever the caller's locale
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseGroupUsage(string(out))
}
