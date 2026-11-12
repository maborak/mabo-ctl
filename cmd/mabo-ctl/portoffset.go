package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/state"
	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// Port offsets `--port-offset auto` tries, in order. Steps of 100 keep a
// moved port recognisable (3000 → 3100) and clear of the neighbouring ports a
// stack usually declares.
const (
	autoOffsetStep = 100
	autoOffsetMax  = 1000
	maxPortOffset  = state.MaxPortOffset
)

// applyPortOffset handles --port-offset: it validates the value, picks one for
// "auto", and saves it in run.env. It reports whether the checkout's offset
// changed, in which case the caller re-resolves without the ports persisted
// under the old one.
func (a *app) applyPortOffset(cfg *config.Config, st *state.Dir) (bool, error) {
	if a.portOffset == "" {
		return false, nil
	}
	re, err := st.ReadRunEnv()
	if err != nil {
		return false, fmt.Errorf("mabo-ctl: read .mabo-ctl/run.env: %w", err)
	}
	current := re.PortOffset()

	var want int
	if strings.EqualFold(a.portOffset, "auto") {
		if want, err = autoPortOffset(cfg, current); err != nil {
			return false, err
		}
	} else {
		want, err = strconv.Atoi(strings.TrimSpace(a.portOffset))
		if err != nil || want < 0 || want > maxPortOffset {
			return false, usageErrorf("--port-offset must be \"auto\" or a whole number from 0 to %d, got %q",
				maxPortOffset, a.portOffset)
		}
	}
	a.portOffset = "" // applied; a second resolve in this process must not redo it
	if want == current {
		return false, nil
	}
	if err := st.SetPortOffset(want); err != nil {
		return false, fmt.Errorf("mabo-ctl: save the port offset: %w", err)
	}
	return true, nil
}

// autoPortOffset keeps the checkout's current offset when it has one, and
// otherwise returns the first multiple of autoOffsetStep at which every
// declared port is free.
func autoPortOffset(cfg *config.Config, current int) (int, error) {
	if current != 0 {
		return current, nil
	}
	var ports []int
	for _, s := range cfg.Services {
		if s.Port > 0 {
			ports = append(ports, s.Port)
		}
	}
	if len(ports) == 0 {
		return 0, usageErrorf("--port-offset auto: no service in %s declares a port, so there is nothing to move", cfg.Path)
	}
	for off := autoOffsetStep; off <= autoOffsetMax; off += autoOffsetStep {
		free := true
		for _, p := range ports {
			if p+off > 65535 || portInUse(p+off) {
				free = false
				break
			}
		}
		if free {
			return off, nil
		}
	}
	return 0, fmt.Errorf("mabo-ctl: --port-offset auto found no offset from +%d to +%d where every declared port is free; give one with --port-offset N",
		autoOffsetStep, autoOffsetMax)
}

// portInUse reports whether something listens on port: the lsof lookup the
// start guard uses, and a loopback dial for machines without lsof.
func portInUse(port int) bool {
	if supervisor.PortHolder(port).PID > 0 {
		return true
	}
	for _, host := range []string{"127.0.0.1", "::1"} {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 150*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return true
		}
	}
	return false
}

// announcePortOffset says on stderr what the new offset did, so stdout stays a
// clean machine contract for `status --json`.
func (a *app) announcePortOffset(st *state.Dir, origins []service.Origin) {
	re, err := st.ReadRunEnv()
	if err != nil {
		return
	}
	off := re.PortOffset()
	if off == 0 {
		fmt.Fprintln(a.env.Stderr, "port offset cleared for this checkout: declared ports apply (.mabo-ctl/run.env updated)")
		return
	}
	var moved []string
	for _, o := range origins {
		if o.Source == service.FromOffset {
			moved = append(moved, fmt.Sprintf("%s %d", o.Service, o.Port))
		}
	}
	fmt.Fprintf(a.env.Stderr, "port offset %+d for this checkout (saved in .mabo-ctl/run.env): %s\n",
		off, strings.Join(moved, ", "))
}
