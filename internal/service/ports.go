package service

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/state"
)

// PortEnvVar returns the caller-environment variable that overrides the port of
// the service named n: the name upper-cased with "-" replaced by "_", followed
// by "_PORT". A service named "browser-service" is overridden by
// BROWSER_SERVICE_PORT.
func PortEnvVar(n string) string {
	return strings.ToUpper(strings.ReplaceAll(n, "-", "_")) + "_PORT"
}

// CaptureEnv reads and UNSETS the caller's <NAME>_PORT variables, returning
// what it took, keyed by variable name. It MUST be called before any child is
// spawned.
//
// The unset is the point. Reading BACKEND_PORT without removing it leaves the
// variable in the environment every child inherits, so a service whose port
// resolved to something else still sees the caller's value and binds it — the
// supervisor then probes a port nobody is listening on. Resolve puts the
// authoritative value back into each child's environment; see [buildEnv].
//
// CaptureEnv mutates the process environment, so call it exactly once, early,
// on the main goroutine: os.Unsetenv is not safe to race against another
// goroutine reading the environment. A variable that is set but blank is taken
// and reported as blank; Resolve treats a blank value as "not set" and falls
// through to the next precedence level. Duplicate names are collapsed. The
// returned map is never nil.
func CaptureEnv(names []string) map[string]string {
	out := make(map[string]string, len(names))
	for _, n := range names {
		key := PortEnvVar(n)
		if _, done := out[key]; done {
			continue
		}
		v, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		out[key] = v
		// An Unsetenv failure would mean the variable is still visible to
		// children, which is the bug this function exists to prevent; there is
		// nothing to recover, but it must not be reported as captured.
		if err := os.Unsetenv(key); err != nil {
			delete(out, key)
		}
	}
	return out
}

// PortSlotNames returns the services a --ports=A,B,C,D flag addresses: every
// service that DECLARES a port, in declaration order. Slot i belongs to
// PortSlotNames(cfg)[i]. The CLI uses it for help text and error messages.
func PortSlotNames(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	names := make([]string, 0, len(cfg.Services))
	for _, s := range cfg.Services {
		if s.Port > 0 {
			names = append(names, s.Name)
		}
	}
	return names
}

// Persist writes the resolved ports of insts to .dev/run.env so the next
// invocation, from any terminal, resolves the same ports.
//
// It expects the FULL instance list returned by Resolve, not a selection from
// [Select]: run.env is rewritten from what it is given, so persisting a subset
// would drop the other services' ports. Portless services are not recorded.
// Keys that state does not own are preserved by state.WriteRunEnv.
//
// It returns an error when st is nil or the file cannot be written.
func Persist(st *state.Dir, insts []Instance) error {
	if st == nil {
		return errors.New("service: Persist: nil state dir")
	}
	re := &state.RunEnv{Ports: make(map[string]int, len(insts))}
	for _, in := range insts {
		if in.Port > 0 {
			re.SetPort(in.Name, in.Port)
		}
	}
	if err := st.WriteRunEnv(re); err != nil {
		return fmt.Errorf("service: persist resolved ports: %w", err)
	}
	return nil
}

// resolvePorts applies the precedence chain to every declared service and
// returns one Origin per service, in declaration order.
//
// Precedence, highest first: --ports slot, caller <NAME>_PORT, persisted
// run.env, declared default.
func resolvePorts(cfg *config.Config, st *state.Dir, opt Options) ([]Origin, error) {
	slots, err := portSlots(cfg, opt.Ports)
	if err != nil {
		return nil, err
	}

	var persisted *state.RunEnv
	if st != nil && !opt.IgnoreRunEnv {
		persisted, err = st.ReadRunEnv()
		if err != nil {
			return nil, fmt.Errorf("service: read persisted ports: %w", err)
		}
	}

	origins := make([]Origin, 0, len(cfg.Services))
	for _, s := range cfg.Services {
		o := Origin{Service: s.Name, Port: s.Port, Source: FromDefault, Declared: s.Port}

		// A level is only consulted when every higher level declined, so a flag
		// shadows a caller variable completely — including its validation.
		if p, ok := slots[s.Name]; ok && p > 0 {
			o.Port, o.Source = p, FromFlag
			origins = append(origins, o)
			continue
		}
		// A caller variable may not invent a port either, for the same reason
		// stale state may not: a service declared portless stays portless. The
		// guard is not symmetry for its own sake. Without it a stray WORKER_PORT
		// in the developer's shell gave a portless worker a port, which then
		// reached the --json contract, armed the start port-guard against a
		// service that binds nothing, and — worst — told `reset --force` to kill
		// whoever held that port, so mabo-ctl signalled a process it never started
		// on a port nobody declared. The variable is ignored rather than
		// rejected: erroring would make mabo-ctl unusable in a shell that happens
		// to export the name for something else, which is precisely how this is
		// triggered by accident.
		if s.Port > 0 {
			if p, ok, err := callerPort(s.Name, opt.EnvVars); err != nil {
				return nil, err
			} else if ok {
				o.Port, o.Source = p, FromEnv
				origins = append(origins, o)
				continue
			}
		}
		// Stale state may not invent a port the config no longer declares: a
		// service declared portless stays portless.
		if p, ok := persisted.Port(s.Name); ok && p > 0 && s.Port > 0 {
			o.Port, o.Source = p, FromRunEnv
			// The reason Origin exists: a persisted port outranking a default
			// that has since changed must be visible, never silent.
			o.Override = p != o.Declared
		}
		origins = append(origins, o)
	}
	return origins, nil
}

// callerPort reads the <NAME>_PORT value the caller captured. A missing or
// blank value means "not set" and falls through to the next precedence level; a
// value that is not a port in 1..65535 is an error, because silently ignoring
// an explicit BACKEND_PORT=abc is exactly the kind of untrue supervisor state
// mabo-ctl exists to avoid.
func callerPort(name string, vars map[string]string) (int, bool, error) {
	key := PortEnvVar(name)
	raw, ok := vars[key]
	if !ok || strings.TrimSpace(raw) == "" {
		return 0, false, nil
	}
	p, err := parsePort(strings.TrimSpace(raw))
	if err != nil {
		return 0, false, fmt.Errorf("service %q: %s=%q: %w", name, key, raw, err)
	}
	return p, true, nil
}

// portSlots maps the positional --ports values onto service names. Slot i
// belongs to the i-th service that declares a port; a 0 keeps that service's
// default.
func portSlots(cfg *config.Config, ports []int) (map[string]int, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	names := PortSlotNames(cfg)
	if len(ports) > len(names) {
		listed := "none — no service declares a port"
		if len(names) > 0 {
			listed = strings.Join(names, ", ")
		}
		return nil, fmt.Errorf("service: --ports has %d values but only %d service(s) declare a port (%s)",
			len(ports), len(names), listed)
	}
	out := make(map[string]int, len(ports))
	for i, p := range ports {
		if p == 0 {
			continue // an empty slot keeps the default
		}
		if _, err := parsePort(strconv.Itoa(p)); err != nil {
			return nil, fmt.Errorf("service: --ports slot %d (service %q): %d: %w", i+1, names[i], p, err)
		}
		out[names[i]] = p
	}
	return out, nil
}

// parsePort accepts a decimal port in 1..65535.
func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, errors.New("not a number; a port must be a decimal number in 1..65535")
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("port %d is out of range; a port must be in 1..65535", n)
	}
	return n, nil
}

// Collision is one resolved port claimed by more than one service.
type Collision struct {
	// Port is the contended port.
	Port int
	// Services names every service that resolved to Port, in declaration order.
	Services []string
	// Origins holds the full Origin of each service in Services, so a caller can
	// show which precedence level produced each claim.
	Origins []Origin
}

// CollisionError reports every resolved-port collision at once.
//
// It is computed pairwise over a port -> services map rather than as a list of
// hand-written comparisons: the shell predecessor compared three services with
// three explicit tests, and when a fourth service arrived three of the six pairs
// went unchecked. Retrieve it with errors.As.
type CollisionError struct {
	// Collisions holds one entry per contended port, ordered by port.
	Collisions []Collision
}

// Error names every contended port and BOTH (or all) of the services claiming
// it, with the precedence level each came from.
func (e *CollisionError) Error() string {
	if e == nil || len(e.Collisions) == 0 {
		return "service: resolved port collision"
	}
	parts := make([]string, 0, len(e.Collisions))
	for _, c := range e.Collisions {
		claims := make([]string, 0, len(c.Origins))
		for _, o := range c.Origins {
			claims = append(claims, fmt.Sprintf("%s (%s)", o.Service, o.Source))
		}
		parts = append(parts, fmt.Sprintf("port %d is claimed by %s", c.Port, joinAnd(claims)))
	}
	return "service: resolved port collision: " + strings.Join(parts, "; ")
}

// checkCollisions reports every resolved port claimed by more than one service.
// Portless services (port 0) never collide.
func checkCollisions(origins []Origin) error {
	byPort := make(map[int][]Origin, len(origins))
	for _, o := range origins {
		if o.Port <= 0 {
			continue
		}
		byPort[o.Port] = append(byPort[o.Port], o)
	}
	ports := make([]int, 0, len(byPort))
	for p, claims := range byPort {
		if len(claims) > 1 {
			ports = append(ports, p)
		}
	}
	if len(ports) == 0 {
		return nil
	}
	sort.Ints(ports)

	err := &CollisionError{Collisions: make([]Collision, 0, len(ports))}
	for _, p := range ports {
		claims := byPort[p]
		names := make([]string, 0, len(claims))
		for _, o := range claims {
			names = append(names, o.Service)
		}
		err.Collisions = append(err.Collisions, Collision{Port: p, Services: names, Origins: claims})
	}
	return err
}

// joinAnd renders a list as "a and b" or "a, b and c".
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}
