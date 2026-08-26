package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

// nameRE is the set of legal service names. It is deliberately narrow: a name
// composes .dev/logs/<name>.log and .dev/pids/<name>.pid, so anything that can
// change a path element ("/", "..", a leading dot) is rejected outright.
var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// runtimeArgRE is the set of legal conda environment names and node versions.
// Same reasoning as nameRE: internal/service composes a filesystem path from
// this value, so a "/" or ".." in it would escape the runtime root.
var runtimeArgRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// ownPortRE matches a zero-argument {{.Port}} action, i.e. a reference to THIS
// service's port. It deliberately does not match {{.Port "other"}}, which is
// another service's port and is legal in a portless service.
var ownPortRE = regexp.MustCompile(`\{\{-?\s*\.Port\s*-?\}\}`)

// ValidationError reports every problem found while validating a mabo-ctl.yaml.
//
// Validation never stops at the first problem: a developer fixing a config
// wants the whole list, so Problems holds one message per violation in file
// order. Retrieve it from a Load or Discover error with errors.As.
type ValidationError struct {
	// Path is the absolute path of the offending mabo-ctl.yaml.
	Path string
	// Problems holds one human-readable message per violation, in file order.
	Problems []string
}

// Error renders the path and every problem, one per line.
func (e *ValidationError) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	noun := "problems"
	if len(e.Problems) == 1 {
		noun = "problem"
	}
	fmt.Fprintf(&b, "%s is invalid (%d %s):", e.Path, len(e.Problems), noun)
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}
	return b.String()
}

// Messages returns a copy of the problem list.
func (e *ValidationError) Messages() []string {
	if e == nil {
		return nil
	}
	return append([]string(nil), e.Problems...)
}

// validate applies every rule and returns a *ValidationError listing all
// violations, or nil. It stats each declared dir, which is the only filesystem
// access validation performs.
func (c *Config) validate() error {
	v := &validator{cfg: c}

	v.checkDurations()
	v.checkServices()
	v.checkDependencies()
	v.checkPortCollisions()
	v.checkChecks()
	v.checkShells()

	if len(v.problems) == 0 {
		return nil
	}
	return &ValidationError{Path: c.Path, Problems: v.problems}
}

// validator accumulates problems across every rule.
type validator struct {
	cfg      *Config
	problems []string
	// declared is the set of syntactically valid, non-duplicate service names.
	declared map[string]bool
	// order is the declaration order of the names in declared.
	order []string
}

func (v *validator) addf(format string, args ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, args...))
}

// label identifies a service in a message: index plus name when there is one.
func label(i int, name string) string {
	if name == "" {
		return fmt.Sprintf("services[%d]", i)
	}
	return fmt.Sprintf("services[%d] %q", i, name)
}

func (v *validator) checkDurations() {
	if v.cfg.StopGrace <= 0 {
		v.addf("stop_grace must be positive, got %s", v.cfg.StopGrace)
	}
	if v.cfg.ReadyTimeout <= 0 {
		v.addf("ready_timeout must be positive, got %s", v.cfg.ReadyTimeout)
	}
}

// checkServices applies rules 1, 2, 3, 4, 6 and 7 plus template syntax and env
// key sanity, and records the set of usable names for the dependency pass.
func (v *validator) checkServices() {
	v.declared = make(map[string]bool, len(v.cfg.Services))

	// Rule 1: at least one service.
	if len(v.cfg.Services) == 0 {
		v.addf("no services declared: mabo-ctl.yaml must declare at least one entry under `services:`")
		return
	}

	seen := make(map[string]int, len(v.cfg.Services))
	for i, s := range v.cfg.Services {
		id := label(i, s.Name)

		// Rule 2: the name is a path component, so it is a security control.
		switch {
		case s.Name == "":
			v.addf("%s: name is required", id)
		case !nameRE.MatchString(s.Name):
			v.addf("%s: invalid name %q: must match %s. The name composes "+
				".dev/logs/<name>.log and .dev/pids/<name>.pid, so a name containing %q, %q "+
				"or a leading %q is a path traversal that would write outside .dev/",
				id, s.Name, nameRE.String(), "/", "..", ".")
		default:
			// Rule 1: duplicate names.
			if first, dup := seen[s.Name]; dup {
				v.addf("%s: duplicate service name %q, already declared as services[%d]; names must be unique",
					id, s.Name, first)
			} else {
				seen[s.Name] = i
				v.declared[s.Name] = true
				v.order = append(v.order, s.Name)
			}
		}

		v.checkDir(id, s)

		// Rule 4: an empty cmd must be unrepresentable, not a silent no-op.
		// A service that falls through to nothing is dev.sh bug #2: the
		// supervisor reported "process died" over an empty log.
		switch {
		case len(s.Cmd) == 0:
			v.addf("%s: cmd is empty; declare the command to run, e.g. cmd: [npm, run, dev]", id)
		case strings.TrimSpace(s.Cmd[0]) == "":
			v.addf("%s: cmd[0] is empty; the first element must be the program to run", id)
		}

		// Rule 6: port range.
		if s.Port < 0 || s.Port > 65535 {
			v.addf("%s: port %d is out of range; use 0 for a service with no port, or 1..65535", id, s.Port)
		}

		// Rule 6: a health template may only reference its own {{.Port}} when
		// the service actually declares one. {{.Port "other"}} is fine.
		if !s.Health.Zero() && s.Port == 0 && ownPortRE.MatchString(s.Health.Raw()) {
			v.addf("%s: health %q references {{.Port}} but the service declares no port; "+
				"give it a port, or probe another service with {{.Port \"name\"}}", id, s.Health.String())
		}

		v.checkRuntime(id, s.Runtime)
		v.checkHealth(id, s)
		v.checkTemplates(id, s)
		v.checkEnvKeys(id, s)
		v.checkEnvFile(id, s)
		if s.ReadyTimeout < 0 {
			v.addf("%s: ready_timeout %s is negative; use a positive duration, or leave the key out to inherit the global", id, s.ReadyTimeout)
		}
	}
}

// checkDir applies rule 3. This is dev.sh bug #1: a declared dir that never
// existed and could only ever fail later, at cd time.
func (v *validator) checkDir(id string, s Spec) {
	dir := strings.TrimSpace(s.Dir)

	abs := v.cfg.Root
	if dir != "" {
		if filepath.IsAbs(dir) {
			abs = filepath.Clean(dir)
		} else {
			abs = filepath.Clean(filepath.Join(v.cfg.Root, dir))
		}
	}

	// Escaping the root is a traversal, not a typo: refuse it before touching
	// the filesystem.
	if !within(v.cfg.Root, abs) {
		v.addf("%s: dir %q resolves to %s, which is outside the project root %s; "+
			"a service directory must stay inside the directory containing mabo-ctl.yaml",
			id, s.Dir, abs, v.cfg.Root)
		return
	}
	// A symlink inside the root that points outside it escapes just as well.
	if realRoot, err := filepath.EvalSymlinks(v.cfg.Root); err == nil {
		if realDir, err := filepath.EvalSymlinks(abs); err == nil && !within(realRoot, realDir) {
			v.addf("%s: dir %q resolves through a symlink to %s, which is outside the project root %s",
				id, s.Dir, realDir, realRoot)
			return
		}
	}

	fi, err := os.Stat(abs)
	switch {
	case os.IsNotExist(err):
		v.addf("%s: dir %q does not exist (resolved to %s); a declared directory that is "+
			"missing can only ever fail later, when the service is spawned", id, s.Dir, abs)
	case err != nil:
		v.addf("%s: dir %q (resolved to %s) cannot be read: %v", id, s.Dir, abs, err)
	case !fi.IsDir():
		v.addf("%s: dir %q (resolved to %s) exists but is not a directory", id, s.Dir, abs)
	}
}

// within reports whether path is root itself or lies beneath it. Both
// arguments must already be absolute and clean.
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

// checkRuntime applies rule 7. Never inherit an ambient interpreter: that is
// dev.sh bug #5.
func (v *validator) checkRuntime(id, runtime string) {
	if runtime == "" || runtime == "system" {
		return
	}
	kind, arg, ok := strings.Cut(runtime, ":")
	if !ok {
		hint := ""
		switch kind {
		case "conda":
			hint = `; did you mean "conda:<env>"?`
		case "node":
			hint = `; did you mean "node:<version>"?`
		}
		v.addf("%s: invalid runtime %q; must be \"\", \"system\", \"conda:<env>\" or \"node:<version>\"%s",
			id, runtime, hint)
		return
	}
	if kind != "conda" && kind != "node" {
		v.addf("%s: invalid runtime %q; must be \"\", \"system\", \"conda:<env>\" or \"node:<version>\"", id, runtime)
		return
	}
	if !runtimeArgRE.MatchString(arg) {
		what := "conda environment name"
		if kind == "node" {
			what = "node version"
		}
		v.addf("%s: invalid runtime %q: the %s must match %s; it is used to build a "+
			"filesystem path, so %q or %q in it would escape the runtime root",
			id, runtime, what, runtimeArgRE.String(), "/", "..")
	}
}

// checkHealth holds a declared probe to the shape of its kind. The decoder
// already guarantees exactly one of http/tcp/exec; this re-checks it for any
// Health built programmatically and applies the per-kind rules that decoding
// cannot: a tcp target must be host:port with a port in range, and an exec
// argv must have a program to run.
//
// The http URL is deliberately NOT parsed here. Templates are raw at load
// time — `http://localhost:{{.Port}}/healthz` is not yet a URL, and parsing
// what is still a template would reject configs that resolve perfectly.
func (v *validator) checkHealth(id string, s Spec) {
	h := s.Health
	if h.Zero() {
		return
	}
	switch h.Kind {
	case HealthHTTP:
		if strings.TrimSpace(h.HTTP) == "" {
			v.addf("%s: health http is empty", id)
		}
	case HealthTCP:
		// Unlike the checks: block, a tcp probe may carry {{.Port}} templates,
		// exactly as an http URL may — so the port is range-checked only when
		// it is already literal.
		host, port, ok := cutTCPAddr(h.Addr)
		switch {
		case !ok || host == "" || port == "":
			v.addf("%s: tcp %q must be host:port, e.g. localhost:5432", id, h.Addr)
		case !strings.Contains(port, "{{"):
			if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				v.addf("%s: tcp %q has an invalid port %q; expected 1..65535", id, h.Addr, port)
			}
		}
	case HealthExec:
		if len(h.Argv) == 0 {
			v.addf("%s: health exec is empty; declare the command to run", id)
			return
		}
		if strings.TrimSpace(h.Argv[0]) == "" {
			v.addf("%s: health exec[0] is empty; the first element must be the program to run", id)
		}
	default:
		v.addf("%s: invalid health kind %q; want %q, %q or %q", id, h.Kind, HealthHTTP, HealthTCP, HealthExec)
	}
}

// checkTemplates rejects a Cmd, Env value or Health part that is not parseable
// as a text/template, so a broken action is a load-time error rather than a
// failure at spawn time. It does not execute anything.
func (v *validator) checkTemplates(id string, s Spec) {
	check := func(what, text string) {
		if !strings.Contains(text, "{{") {
			return
		}
		if _, err := template.New("t").Parse(text); err != nil {
			v.addf("%s: %s is not a valid template: %v", id, what, err)
		}
	}
	check(fmt.Sprintf("health %q", s.Health.String()), s.Health.Raw())
	for i, a := range s.Cmd {
		check(fmt.Sprintf("cmd[%d] %q", i, a), a)
	}
	for _, k := range sortedKeys(s.Env) {
		check(fmt.Sprintf("env[%q] %q", k, s.Env[k]), s.Env[k])
	}
}

// checkEnvKeys rejects environment keys that cannot survive being encoded as
// "KEY=VALUE" for exec.Cmd.Env.
func (v *validator) checkEnvKeys(id string, s Spec) {
	for _, k := range sortedKeys(s.Env) {
		v.checkEnvKeyName(id, "env", k)
	}
}

// checkEnvKeyName applies the env-key rules to one variable name, labelled by
// where it came from ("env" or "env_file"), so a file entry is held to the
// same standard as an inline one.
func (v *validator) checkEnvKeyName(id, source, k string) {
	switch {
	case strings.TrimSpace(k) == "":
		v.addf("%s: %s has an empty variable name", id, source)
	case strings.ContainsAny(k, "=\x00"):
		v.addf("%s: invalid env variable name %q; a name may not contain %q or a NUL byte, "+
			"because the variable is passed to the child as \"KEY=VALUE\"", id, k, "=")
	}
}

// checkEnvFile validates a service's env_file: the path must resolve inside
// the project root, the file must parse as KEY=VALUE lines, and every key and
// template value in it is held to the same rules as an inline env entry. The
// file is read here AND again at resolve time — the load-time read turns a
// broken file into a listed validation problem, the resolve-time read picks up
// edits without demanding a reload.
func (v *validator) checkEnvFile(id string, s Spec) {
	if s.EnvFile == "" {
		return
	}
	path := s.EnvFilePath(v.cfg.Root)
	if !within(v.cfg.Root, path) {
		v.addf("%s: env_file %q resolves to %s, which is outside the project root %s; "+
			"an env file further up belongs to something else", id, s.EnvFile, path, v.cfg.Root)
		return
	}
	fileEnv, err := ParseEnvFile(path)
	if err != nil {
		v.addf("%s: %v", id, err)
		return
	}
	check := func(what, text string) {
		if !strings.Contains(text, "{{") {
			return
		}
		if _, err := template.New("t").Parse(text); err != nil {
			v.addf("%s: %s is not a valid template: %v", id, what, err)
		}
	}
	for _, k := range sortedKeys(fileEnv) {
		v.checkEnvKeyName(id, "env_file", k)
		check(fmt.Sprintf("env_file[%q] %q", k, fileEnv[k]), fileEnv[k])
	}
}

// checkDependencies applies rule 5: every depends_on entry names a declared
// service, and the graph is acyclic.
func (v *validator) checkDependencies() {
	for i, s := range v.cfg.Services {
		id := label(i, s.Name)
		seen := make(map[string]bool, len(s.DependsOn))
		for _, dep := range s.DependsOn {
			if dep == s.Name {
				continue // reported as a cycle below, with the path
			}
			if !v.declared[dep] {
				v.addf("%s: depends_on names unknown service %q; declared services are: %s",
					id, dep, v.declaredList())
				continue
			}
			if seen[dep] {
				v.addf("%s: depends_on lists %q more than once", id, dep)
			}
			seen[dep] = true
		}
	}
	for _, cycle := range v.findCycles() {
		v.addf("dependency cycle: %s", strings.Join(cycle, " -> "))
	}
}

// declaredList names every usable service for an error message. A service
// whose own name was rejected is not usable, so it is not listed.
func (v *validator) declaredList() string {
	if len(v.order) == 0 {
		return "(none — no service has a valid name)"
	}
	return strings.Join(v.order, ", ")
}

// findCycles returns every distinct depends_on cycle as a path that starts and
// ends on the same service. Only declared names participate.
func (v *validator) findCycles() [][]string {
	deps := make(map[string][]string, len(v.declared))
	for _, s := range v.cfg.Services {
		if !v.declared[s.Name] {
			continue
		}
		if _, dup := deps[s.Name]; dup {
			continue
		}
		var list []string
		for _, d := range s.DependsOn {
			if v.declared[d] {
				list = append(list, d)
			}
		}
		deps[s.Name] = list
	}

	const (
		white = 0 // unvisited
		grey  = 1 // on the current stack
		black = 2 // fully explored
	)
	colour := make(map[string]int, len(deps))
	var stack []string
	var cycles [][]string
	reported := make(map[string]bool)

	var visit func(string)
	visit = func(n string) {
		colour[n] = grey
		stack = append(stack, n)
		for _, d := range deps[n] {
			switch colour[d] {
			case white:
				visit(d)
			case grey:
				start := 0
				for i, s := range stack {
					if s == d {
						start = i
						break
					}
				}
				cycle := append(append([]string(nil), stack[start:]...), d)
				if key := cycleKey(cycle); !reported[key] {
					reported[key] = true
					cycles = append(cycles, cycle)
				}
			}
		}
		stack = stack[:len(stack)-1]
		colour[n] = black
	}

	for _, n := range v.order {
		if colour[n] == white {
			visit(n)
		}
	}
	return cycles
}

// cycleKey canonicalises a cycle path so the same loop found from two entry
// points is reported once.
func cycleKey(cycle []string) string {
	nodes := append([]string(nil), cycle[:len(cycle)-1]...)
	sort.Strings(nodes)
	return strings.Join(nodes, "\x00")
}

// checkPortCollisions applies rule 8. It is computed pairwise over a
// port -> services map, never as a hand-written comparison list: three
// hardcoded comparisons for three services left three of six pairs unchecked
// when a fourth service arrived.
func (v *validator) checkPortCollisions() {
	byPort := make(map[int][]string, len(v.cfg.Services))
	for i, s := range v.cfg.Services {
		if s.Port <= 0 || s.Port > 65535 {
			continue
		}
		byPort[s.Port] = append(byPort[s.Port], label(i, s.Name))
	}
	ports := make([]int, 0, len(byPort))
	for p, owners := range byPort {
		if len(owners) > 1 {
			ports = append(ports, p)
		}
	}
	sort.Ints(ports)
	for _, p := range ports {
		v.addf("port %d is declared by more than one service: %s", p, strings.Join(byPort[p], ", "))
	}
}

// checkChecks validates the preflight block: a named probe with exactly one of
// command or tcp.
func (v *validator) checkChecks() {
	seen := make(map[string]int, len(v.cfg.Checks))
	for i, ck := range v.cfg.Checks {
		id := fmt.Sprintf("checks[%d]", i)
		if ck.Name != "" {
			id = fmt.Sprintf("checks[%d] %q", i, ck.Name)
		}

		if ck.Name == "" {
			v.addf("%s: name is required", id)
		} else if first, dup := seen[ck.Name]; dup {
			v.addf("%s: duplicate check name %q, already declared as checks[%d]", id, ck.Name, first)
		} else {
			seen[ck.Name] = i
		}

		hasCmd := len(ck.Command) > 0
		hasTCP := strings.TrimSpace(ck.TCP) != ""
		switch {
		case hasCmd && hasTCP:
			v.addf("%s: set exactly one of command or tcp, not both", id)
		case !hasCmd && !hasTCP:
			v.addf("%s: set exactly one of command or tcp", id)
		case hasCmd && strings.TrimSpace(ck.Command[0]) == "":
			v.addf("%s: command[0] is empty", id)
		case hasTCP:
			v.checkTCPAddr(id, ck.TCP)
		}
	}
}

// checkTCPAddr validates a "host:port" preflight target without resolving it.
func (v *validator) checkTCPAddr(id, addr string) {
	host, port, ok := cutTCPAddr(addr)
	if !ok || host == "" || port == "" {
		v.addf("%s: tcp %q must be host:port, e.g. localhost:5432", id, addr)
		return
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		v.addf("%s: tcp %q has an invalid port %q; expected 1..65535", id, addr, port)
	}
}

// cutTCPAddr splits "host:port" the same way for every caller of
// checkTCPAddr-like rules, bracketed IPv6 literals included.
func cutTCPAddr(addr string) (host, port string, ok bool) {
	host, port, ok = strings.Cut(addr, ":")
	// An IPv6 literal must be bracketed; take the text after the last colon.
	if strings.HasPrefix(addr, "[") {
		if end := strings.LastIndex(addr, "]:"); end >= 0 {
			host, port, ok = addr[1:end], addr[end+2:], true
		}
	}
	return host, port, ok
}

// checkShells validates the shells block: a named command, optionally bound to
// a declared service whose directory and environment it reuses.
func (v *validator) checkShells() {
	seen := make(map[string]int, len(v.cfg.Shells))
	for i, sh := range v.cfg.Shells {
		id := fmt.Sprintf("shells[%d]", i)
		if sh.Name != "" {
			id = fmt.Sprintf("shells[%d] %q", i, sh.Name)
		}

		if sh.Name == "" {
			v.addf("%s: name is required", id)
		} else if first, dup := seen[sh.Name]; dup {
			v.addf("%s: duplicate shell name %q, already declared as shells[%d]", id, sh.Name, first)
		} else {
			seen[sh.Name] = i
		}

		switch {
		case len(sh.Command) == 0:
			v.addf("%s: command is empty; declare the command to run, e.g. command: [python]", id)
		case strings.TrimSpace(sh.Command[0]) == "":
			v.addf("%s: command[0] is empty; the first element must be the program to run", id)
		}

		if sh.Service != "" && !v.declared[sh.Service] {
			v.addf("%s: service %q is not a declared service; declared services are: %s",
				id, sh.Service, v.declaredList())
		}
	}
}

// sortedKeys returns m's keys in sorted order so messages are deterministic.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
