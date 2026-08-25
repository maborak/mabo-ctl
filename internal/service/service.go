// Package service turns declared specs into runnable instances: it resolves
// every service's port, expands the {{.Port}} templates that config held raw,
// resolves Cmd[0] to an absolute interpreter path, and builds the exact
// environment the child will run with.
//
// This is the package where the predecessor shell script got things wrong, so
// four behaviours are load-bearing and are not tunable:
//
//   - Port precedence is flag > caller env > .dev/run.env > declared default,
//     and an [Origin] is recorded for EVERY service saying which source won.
//   - [CaptureEnv] reads and UNSETS the caller's <NAME>_PORT variables. Without
//     the unset a child inherits BACKEND_PORT=7102 while the supervisor resolved
//     7999, and the service binds a port nobody is probing.
//   - A persisted port that differs from the declared default sets
//     [Origin.Override] so the CLI can print a visible line. Silently preferring
//     stale state cost a real debugging round during a port migration.
//   - Collisions are computed pairwise over a port -> services map, never as a
//     hand-written comparison list. Three hardcoded comparisons for three
//     services left three of six pairs unchecked when a fourth arrived.
//
// Runtime resolution never falls back to an ambient PATH lookup: a service
// declared `runtime: conda:app-dev` runs that environment's interpreter or it
// fails, naming the exact path it looked for.
package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/state"
)

// environ reads the current process environment. It is a variable so a test can
// substitute a fixed environment; production code never reassigns it.
var environ = os.Environ

// Instance is a service with its port resolved and every template expanded.
// Everything in it is final: the supervisor spawns an Instance without
// consulting the config again.
type Instance struct {
	// Name is the declared service name.
	Name string
	// Dir is the absolute working directory, verified to exist.
	Dir string
	// Port is the resolved port, or 0 for a portless service.
	Port int
	// Health is the expanded readiness URL, or "" for no probe.
	Health string
	// Cmd is the expanded argv. Cmd[0] is an absolute path to the executable
	// chosen by Runtime, never a bare name left to the child's PATH.
	Cmd []string
	// Env is the child environment as "KEY=VALUE" pairs, ready to be assigned to
	// exec.Cmd.Env: the caller's environment, the resolved <NAME>_PORT of every
	// ported service, whatever the runtime needs, and the service's declared env
	// last. It is complete — a caller that appends os.Environ() to it would
	// reintroduce the stale port variables CaptureEnv removed.
	Env []string
	// Color is the label colour for status output.
	Color string
	// DependsOn lists the services that must start first.
	DependsOn []string
	// Runtime is the declared runtime string ("", "system", "conda:<env>" or
	// "node:<version>"), kept for display.
	Runtime string
	// NoAutostart excludes this service from a bare `mabo-ctl start`.
	//
	// It is stored NEGATED so that the zero value is the common answer. A plain
	// `Autostart bool` read the right way round and was wrong in the way that
	// matters: every Instance built by hand — every test, and any future caller
	// composing one directly — got false and silently opted out of starting.
	// Prefer [Instance.Autostarts] at call sites; the negation lives here and
	// nowhere else.
	NoAutostart bool
	// CmdErr is the deferred failure to resolve Cmd[0] against Runtime, or nil
	// when the executable was found. When it is non-nil Cmd[0] is the
	// UNRESOLVED name and this instance must not be spawned; [Runnable] reports
	// exactly that.
	//
	// Resolution failure is carried here instead of aborting [Resolve] because
	// it is a property of ONE service, and the commands that do not execute
	// anything — status, stop, logs, reset — must keep working for the others.
	// Failing the whole call meant a single missing interpreter left already
	// running services with no way to stop or reset them, which is how orphans
	// accumulate; that is the failure mabo-ctl exists to prevent.
	CmdErr error
}

// PortSource records WHERE a port came from. Printing this is a product
// requirement, not a debug aid — see [Origin].
type PortSource string

const (
	// FromFlag means the port came from --ports.
	FromFlag PortSource = "flag"
	// FromEnv means the port came from a <NAME>_PORT variable in the caller's
	// environment, captured by [CaptureEnv].
	FromEnv PortSource = "env"
	// FromRunEnv means the port came from the persisted .dev/run.env cache.
	FromRunEnv PortSource = "run.env"
	// FromDefault means the port is the one declared in mabo-ctl.yaml.
	FromDefault PortSource = "default"
)

// Origin explains one service's resolved port. Resolve returns one for every
// declared service so the CLI can answer "why is backend on 7999?" without
// re-deriving the precedence chain.
type Origin struct {
	// Service is the service name.
	Service string
	// Port is the resolved port, 0 for a portless service.
	Port int
	// Source is the precedence level that won.
	Source PortSource
	// Declared is the port mabo-ctl.yaml declares.
	Declared int
	// Override is true when Source is FromRunEnv AND Port differs from Declared:
	// persisted state is outranking a default that has since changed. The CLI
	// must print this; stale state winning silently is the documented wrong
	// default.
	Override bool
}

// Options carries the caller's port inputs for [Resolve].
type Options struct {
	// Ports holds the positional values of --ports=A,B,C,D. Slot i applies to
	// the i-th service that DECLARES a port, in declaration order; a 0 in a slot
	// keeps that service's default. A service declared portless has no slot, so
	// --ports can never give it one.
	Ports []int
	// EnvVars holds the caller's <NAME>_PORT variables, ALREADY CAPTURED by the
	// caller with [CaptureEnv]. Keys are variable names ("BACKEND_PORT"), not
	// service names. Resolve deliberately does not read the process environment
	// for ports itself: capture must happen once, early, before any spawn.
	EnvVars map[string]string
	// IgnoreRunEnv drops the persisted .dev/run.env level from the precedence
	// chain, so a service falls through to its declared default unless --ports
	// or a caller variable speaks first. It exists so the CLI can offer to
	// adopt ports the yaml has since changed: the run.env level exists to keep
	// ports stable across invocations, and a port kept stable against a yaml
	// that no longer agrees with it is stale, not stable. Resolve still writes
	// nothing; the caller refreshes the file with [Persist].
	IgnoreRunEnv bool
}

// Resolve applies the precedence chain, expands templates, and validates the
// result. Origins is returned for EVERY service, in declaration order.
//
// The order is fixed: every port resolves first, because a template may
// reference another service's port; then collisions are checked over the
// RESOLVED ports; then each service's Dir, Health, Cmd and Env are expanded and
// its interpreter resolved.
//
// st may be nil, in which case the persisted .dev/run.env level is skipped —
// useful before the state directory exists. Options.IgnoreRunEnv skips the same
// level explicitly, for a caller that has a state dir but wants the declared
// defaults to win this once. Resolve itself writes nothing; see [Persist].
//
// Errors: a nil or empty config; a --ports slot that is out of range or has no
// service; a <NAME>_PORT value that is not a port; a resolved-port collision
// (a *CollisionError, with origins still returned so the caller can show where
// each port came from); a template that fails to parse or references an unknown
// service; a directory that does not exist or escapes the project root; and any
// runtime whose interpreter cannot be resolved to an existing executable.
func Resolve(cfg *config.Config, st *state.Dir, opt Options) (insts []Instance, origins []Origin, err error) {
	if cfg == nil {
		return nil, nil, errors.New("service: Resolve: nil config")
	}
	if len(cfg.Services) == 0 {
		return nil, nil, errors.New("service: Resolve: config declares no services")
	}

	origins, err = resolvePorts(cfg, st, opt)
	if err != nil {
		return nil, nil, err
	}
	if err := checkCollisions(origins); err != nil {
		return nil, origins, err
	}

	ports := make(map[string]int, len(origins))
	for _, o := range origins {
		ports[o.Service] = o.Port
	}

	exp := newExpander(cfg.Names(), ports)
	base := environ()

	insts = make([]Instance, 0, len(cfg.Services))
	for _, s := range cfg.Services {
		in, err := build(cfg, s, ports[s.Name], exp, base)
		if err != nil {
			return nil, origins, err
		}
		insts = append(insts, in)
	}
	return insts, origins, nil
}

// build turns one spec into an Instance: directory, expanded templates,
// resolved interpreter and final environment.
func build(cfg *config.Config, s config.Spec, port int, exp *expander, base []string) (Instance, error) {
	if strings.TrimSpace(s.Name) == "" {
		return Instance{}, errors.New("service: a service has an empty name")
	}
	if len(s.Cmd) == 0 {
		return Instance{}, fmt.Errorf("service %q: cmd is empty; there is nothing to run", s.Name)
	}

	dir, err := resolveDir(cfg.Root, s)
	if err != nil {
		return Instance{}, err
	}

	health, err := exp.expand(s.Name, "health", s.Health)
	if err != nil {
		return Instance{}, err
	}

	cmd := make([]string, len(s.Cmd))
	for i, arg := range s.Cmd {
		expanded, err := exp.expand(s.Name, fmt.Sprintf("cmd[%d]", i), arg)
		if err != nil {
			return Instance{}, err
		}
		cmd[i] = expanded
	}
	if strings.TrimSpace(cmd[0]) == "" {
		return Instance{}, fmt.Errorf("service %q: cmd[0] expanded to an empty string", s.Name)
	}

	specEnv := make(map[string]string, len(s.Env))
	for _, k := range sortedKeys(s.Env) {
		expanded, err := exp.expand(s.Name, fmt.Sprintf("env[%q]", k), s.Env[k])
		if err != nil {
			return Instance{}, err
		}
		specEnv[k] = expanded
	}

	// The system runtime searches the PATH the CHILD will run with — the one the
	// service declares if it declares one — not mabo-ctl's own.
	searchPath, ok := specEnv["PATH"]
	if !ok {
		searchPath = lookupEnv(base, "PATH")
	}
	// A runtime declaration that is MALFORMED is wrong on every machine, so it
	// fails the load like any other config error. A runtime that is merely not
	// INSTALLED here is recorded on the instance instead and fails only the
	// service that needs it — see Instance.CmdErr.
	var cmdErr error
	rt, rtErr := resolveRuntime(s.Name, s.Runtime, cmd[0], dir, searchPath)
	switch {
	case rtErr == nil:
		cmd[0] = rt.Path
	case errors.Is(rtErr, errRuntimeUnavailable):
		cmdErr = rtErr
	default:
		return Instance{}, rtErr
	}

	return Instance{
		Name:        s.Name,
		Dir:         dir,
		Port:        port,
		Health:      health,
		Cmd:         cmd,
		Env:         buildEnv(base, exp.names, exp.ports, specEnv, rt),
		Color:       s.Color,
		DependsOn:   append([]string(nil), s.DependsOn...),
		Runtime:     s.Runtime,
		NoAutostart: !s.Autostarts(),
		CmdErr:      cmdErr,
	}, nil
}

// resolveDir returns the absolute working directory for s, verified to exist,
// to be a directory, and to stay inside root.
//
// An empty dir means the directory containing mabo-ctl.yaml. config already
// enforces all of this at load time; it is enforced again here because Resolve
// accepts any *config.Config, and because a declared directory that never
// existed is dev.sh bug #1 — it could only ever fail later, at spawn time.
func resolveDir(root string, s config.Spec) (string, error) {
	dir := strings.TrimSpace(s.Dir)
	abs := root
	if dir != "" {
		if filepath.IsAbs(dir) {
			abs = filepath.Clean(dir)
		} else {
			abs = filepath.Clean(filepath.Join(root, dir))
		}
	}
	if !within(root, abs) {
		return "", fmt.Errorf("service %q: dir %q resolves to %s, which is outside the project root %s",
			s.Name, s.Dir, abs, root)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("service %q: dir %q (resolved to %s) cannot be used: %w", s.Name, s.Dir, abs, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("service %q: dir %q (resolved to %s) is not a directory", s.Name, s.Dir, abs)
	}
	return abs, nil
}

// within reports whether path is root itself or lies beneath it. Both arguments
// must already be absolute and clean.
func within(root, path string) bool {
	if path == root {
		return true
	}
	sep := string(os.PathSeparator)
	if !strings.HasSuffix(root, sep) {
		root += sep
	}
	return strings.HasPrefix(path, root)
}

// buildEnv composes the child environment: the caller's environment, then the
// resolved <NAME>_PORT variables for every ported service, then whatever the
// runtime needs, then the service's own declared env, which wins over all of
// them. A PATH contributed by the runtime is prepended to whichever PATH
// survived that merge.
//
// Re-injecting the resolved port variables is the other half of [CaptureEnv]:
// capture removes the caller's possibly-stale value, and this puts the
// authoritative one back, so a child can never read a port the supervisor did
// not choose.
//
// The result is deterministic: inherited variables keep the order the
// environment gave them, and the overrides follow, sorted by name.
func buildEnv(base, names []string, ports map[string]int, specEnv map[string]string, rt resolvedRuntime) []string {
	over := make(map[string]string, len(names)+len(specEnv)+len(rt.Env)+1)
	for _, n := range names {
		if p := ports[n]; p > 0 {
			over[PortEnvVar(n)] = strconv.Itoa(p)
		}
	}
	for k, v := range rt.Env {
		over[k] = v
	}
	for k, v := range specEnv {
		over[k] = v
	}
	if rt.BinDir != "" {
		path, ok := over["PATH"]
		if !ok {
			path = lookupEnv(base, "PATH")
		}
		over["PATH"] = prependPath(rt.BinDir, path)
	}

	out := make([]string, 0, len(base)+len(over))
	seen := make(map[string]bool, len(base)+len(over))
	for _, entry := range base {
		eq := strings.Index(entry, "=")
		if eq <= 0 {
			continue // not a KEY=VALUE pair; passing it on would confuse exec
		}
		key := entry[:eq]
		if _, overridden := over[key]; overridden || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, entry)
	}
	keys := make([]string, 0, len(over))
	for k := range over {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+over[k])
	}
	return out
}

// lookupEnv returns the value of key in a "KEY=VALUE" slice, or "".
func lookupEnv(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return entry[len(prefix):]
		}
	}
	return ""
}

// prependPath puts bin at the front of a PATH-style list unless it is already
// first, so repeated resolution does not grow the variable without bound.
func prependPath(bin, path string) string {
	if path == "" {
		return bin
	}
	sep := string(os.PathListSeparator)
	if path == bin || strings.HasPrefix(path, bin+sep) {
		return path
	}
	return bin + sep + path
}

// Runnable reports whether in can be spawned, returning the deferred
// runtime-resolution error when it cannot.
//
// Every caller that is about to execute Cmd MUST check this first: when it
// returns a non-nil error, Cmd[0] is an unresolved name and running it would
// either fail obscurely or, worse, find a DIFFERENT program of the same name on
// an ambient PATH — the exact substitution the runtime field exists to prevent.
func (in Instance) Runnable() error { return in.CmdErr }

// Autostarts reports whether a bare `mabo-ctl start` should include this service.
// The default — a zero-valued Instance, and a mabo-ctl.yaml that never mentions
// autostart — is yes.
func (in Instance) Autostarts() bool { return !in.NoAutostart }

// Select returns the instances named in want, expanding dependencies.
// Empty want = all. An unknown name is an error naming the valid ones.
//
// The result is in dependency order: a service always appears after everything
// it depends on, transitively. Duplicates in want collapse. A dependency cycle
// is an error naming the path, even though config rejects cycles at load time —
// Select must terminate on any input, not only a validated one.
func Select(insts []Instance, want []string) ([]Instance, error) {
	if len(insts) == 0 {
		return nil, errors.New("service: Select: no instances to select from")
	}
	byName := make(map[string]Instance, len(insts))
	order := make([]string, 0, len(insts))
	for _, in := range insts {
		if _, dup := byName[in.Name]; dup {
			return nil, fmt.Errorf("service: Select: duplicate instance %q", in.Name)
		}
		byName[in.Name] = in
		order = append(order, in.Name)
	}

	roots := want
	if len(roots) == 0 {
		roots = order
	}
	for _, n := range roots {
		if _, ok := byName[n]; !ok {
			return nil, fmt.Errorf("service: unknown service %q; declared services are: %s",
				n, strings.Join(order, ", "))
		}
	}

	const (
		white = 0
		grey  = 1
		black = 2
	)
	colour := make(map[string]int, len(insts))
	out := make([]Instance, 0, len(insts))

	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch colour[name] {
		case black:
			return nil
		case grey:
			return fmt.Errorf("service: Select: dependency cycle: %s",
				strings.Join(append(append([]string(nil), path...), name), " -> "))
		}
		colour[name] = grey
		here := append(append([]string(nil), path...), name)
		for _, dep := range byName[name].DependsOn {
			if _, ok := byName[dep]; !ok {
				return fmt.Errorf("service %q depends on unknown service %q; declared services are: %s",
					name, dep, strings.Join(order, ", "))
			}
			if err := visit(dep, here); err != nil {
				return err
			}
		}
		colour[name] = black
		out = append(out, byName[name])
		return nil
	}

	for _, n := range roots {
		if err := visit(n, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SelectExact returns the named instances and nothing else: no dependency
// expansion. It exists for verbs whose meaning is "exactly what was named" —
// Stop takes the named services down, not the services they depend on.
// Select expands a name into its dependency closure because Start needs the
// whole subtree up; reusing that set for stop made `stop listener` silently
// kill the backend listener depends on (docs/LANDMINES.md §8). An empty want
// still means every declared service — for stop, naming none has always meant
// everything, and nothing narrows it. Order follows insts' declaration order,
// a repeated name collapses to one entry, and an unknown name is an error.
func SelectExact(insts []Instance, want []string) ([]Instance, error) {
	if len(insts) == 0 {
		return nil, errors.New("service: SelectExact: no instances to select from")
	}
	byName := make(map[string]Instance, len(insts))
	order := make([]string, 0, len(insts))
	for _, in := range insts {
		if _, dup := byName[in.Name]; dup {
			return nil, fmt.Errorf("service: SelectExact: duplicate instance %q", in.Name)
		}
		byName[in.Name] = in
		order = append(order, in.Name)
	}

	names := want
	if len(names) == 0 {
		names = order
	}
	wanted := make(map[string]bool, len(names))
	for _, n := range names {
		if _, ok := byName[n]; !ok {
			return nil, fmt.Errorf("service: unknown service %q; declared services are: %s",
				n, strings.Join(order, ", "))
		}
		wanted[n] = true
	}
	out := make([]Instance, 0, len(wanted))
	for _, n := range order {
		if wanted[n] {
			out = append(out, byName[n])
		}
	}
	return out, nil
}

// sortedKeys returns m's keys in sorted order so expansion errors and generated
// environments are deterministic.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// SelectLevels returns the same instances as [Select], grouped into dependency
// LEVELS: level 0 holds services with no dependency inside the selection, level
// 1 holds services depending only on level 0, and so on.
//
// It exists so a supervisor can start a level concurrently and only wait
// between levels. Select's flat topological order is a valid sequence, but it
// throws away the fact that most services in a normal stack depend on nothing
// at all — so a caller walking it serially makes independent services queue
// behind each other and pays the SUM of their startup times. For a stack of
// three unrelated services that each take three seconds, that is the difference
// between eleven seconds and three.
//
// Within a level the declaration order from insts is preserved, so output that
// iterates a level is stable and diffable rather than dependent on map order.
//
// The error cases are Select's, and identical: an unknown name in want, a
// dependency on an undeclared service, or a cycle (named as a path).
func SelectLevels(insts []Instance, want []string) ([][]Instance, error) {
	flat, err := Select(insts, want)
	if err != nil {
		return nil, err
	}
	if len(flat) == 0 {
		return nil, nil
	}

	// Only dependencies INSIDE the selection constrain ordering. `mabo-ctl start
	// worker` when worker depends on backend selects both, but `mabo-ctl start
	// backend` must not be held up by anything outside the set.
	selected := make(map[string]bool, len(flat))
	for _, in := range flat {
		selected[in.Name] = true
	}

	// Rank each service one past its deepest selected dependency. flat is
	// already topologically ordered, so every dependency has been ranked by the
	// time its dependant is reached and one pass suffices.
	rank := make(map[string]int, len(flat))
	deepest := 0
	for _, in := range flat {
		r := 0
		for _, dep := range in.DependsOn {
			if !selected[dep] {
				continue
			}
			if dr := rank[dep] + 1; dr > r {
				r = dr
			}
		}
		rank[in.Name] = r
		if r > deepest {
			deepest = r
		}
	}

	levels := make([][]Instance, deepest+1)
	for _, in := range flat {
		r := rank[in.Name]
		levels[r] = append(levels[r], in)
	}
	return levels, nil
}
