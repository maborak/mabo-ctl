package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// imported is one service read from another tool's process file.
type imported struct {
	name      string
	source    string // the name as the source file spelled it, for the comment
	command   string // a shell command line; run through `sh -c`
	dir       string
	env       map[string]string
	port      int
	health    string
	deps      []string
	readyDeps []string
	disabled  bool
	notes     []string // TODO comments for what could not be carried over
}

// procfileLine is `name: command`, the only shape a Procfile has.
var procfileLine = regexp.MustCompile(`^([A-Za-z0-9_-]+):\s*(.+)$`)

// envKey is a variable name mabo-ctl will pass through.
var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// importServices reads path as a Procfile or a process-compose file, picked by
// its name, and returns its services in file order.
func importServices(path string) ([]imported, string, error) {
	base := strings.ToLower(filepath.Base(path))
	switch {
	case base == "procfile" || strings.HasPrefix(base, "procfile."):
		svcs, err := importProcfile(path)
		return svcs, "Procfile", err
	case strings.HasPrefix(base, "process-compose") && (strings.HasSuffix(base, ".yaml") || strings.HasSuffix(base, ".yml")):
		svcs, err := importProcessCompose(path)
		return svcs, "process-compose", err
	default:
		return nil, "", usageErrorf("mabo-ctl init --from: %s is neither a Procfile nor a process-compose.yaml; "+
			"name it Procfile, Procfile.<env> or process-compose[.<x>].yaml", path)
	}
}

// importProcfile reads `name: command` lines. A command that mentions $PORT is
// given a port the way foreman assigns them — 5000, then +100 per process — so
// mabo-ctl sets PORT for it exactly as foreman did.
func importProcfile(path string) ([]imported, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("mabo-ctl init --from: %w", err)
	}
	defer func() { _ = f.Close() }()

	var out []imported
	sc := bufio.NewScanner(f)
	for n := 0; sc.Scan(); {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := procfileLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("mabo-ctl init --from: %s: %q is not a `name: command` line", path, line)
		}
		svc := imported{source: m[1], command: strings.TrimSpace(m[2])}
		if strings.Contains(svc.command, "$PORT") || strings.Contains(svc.command, "${PORT}") {
			svc.port = 5000 + 100*n
		}
		svc.notes = append(svc.notes, "health: TODO — a URL that answers once it is ready")
		out = append(out, svc)
		n++
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("mabo-ctl init --from: read %s: %w", path, err)
	}
	return out, nil
}

// processCompose is the part of a process-compose file that maps onto
// mabo-ctl.yaml.
type processCompose struct {
	Environment yaml.Node `yaml:"environment"`
	Processes   yaml.Node `yaml:"processes"`
}

type pcProcess struct {
	Command     string    `yaml:"command"`
	WorkingDir  string    `yaml:"working_dir"`
	Environment yaml.Node `yaml:"environment"`
	Disabled    bool      `yaml:"disabled"`
	DependsOn   map[string]struct {
		Condition string `yaml:"condition"`
	} `yaml:"depends_on"`
	ReadinessProbe struct {
		HTTPGet struct {
			Host   string `yaml:"host"`
			Scheme string `yaml:"scheme"`
			Path   string `yaml:"path"`
			Port   int    `yaml:"port"`
		} `yaml:"http_get"`
	} `yaml:"readiness_probe"`
}

// importProcessCompose reads processes in document order. A dependency on a
// healthy process becomes depends_ready_on; an HTTP readiness probe becomes
// the health URL, and its port the service's port.
func importProcessCompose(path string) ([]imported, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mabo-ctl init --from: %w", err)
	}
	var doc processCompose
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("mabo-ctl init --from: parse %s: %w", path, err)
	}
	if doc.Processes.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("mabo-ctl init --from: %s has no processes: mapping", path)
	}
	global, err := pcEnv(&doc.Environment)
	if err != nil {
		return nil, fmt.Errorf("mabo-ctl init --from: %s: environment: %w", path, err)
	}

	var out []imported
	for i := 0; i+1 < len(doc.Processes.Content); i += 2 {
		name := doc.Processes.Content[i].Value
		var p pcProcess
		if err := doc.Processes.Content[i+1].Decode(&p); err != nil {
			return nil, fmt.Errorf("mabo-ctl init --from: %s: process %q: %w", path, name, err)
		}
		env, err := pcEnv(&p.Environment)
		if err != nil {
			return nil, fmt.Errorf("mabo-ctl init --from: %s: process %q: environment: %w", path, name, err)
		}
		merged := make(map[string]string, len(global)+len(env))
		for k, v := range global {
			merged[k] = v
		}
		for k, v := range env {
			merged[k] = v
		}
		svc := imported{source: name, command: strings.TrimSpace(p.Command), dir: p.WorkingDir, env: merged, disabled: p.Disabled}
		if svc.command == "" {
			svc.notes = append(svc.notes, "cmd: TODO — the process had no command:")
		}
		deps := make([]string, 0, len(p.DependsOn))
		for d := range p.DependsOn {
			deps = append(deps, d)
		}
		sort.Strings(deps)
		for _, d := range deps {
			svc.deps = append(svc.deps, d)
			if p.DependsOn[d].Condition == "process_healthy" {
				svc.readyDeps = append(svc.readyDeps, d)
			}
		}
		if h := p.ReadinessProbe.HTTPGet; h.Port > 0 {
			scheme, host := h.Scheme, h.Host
			if scheme == "" {
				scheme = "http"
			}
			if host == "" {
				host = "localhost"
			}
			svc.port = h.Port
			svc.health = fmt.Sprintf("%s://%s:{{.Port}}/%s", scheme, host, strings.TrimPrefix(h.Path, "/"))
		} else {
			svc.notes = append(svc.notes, "health: TODO — process-compose had no http_get readiness probe")
		}
		out = append(out, svc)
	}
	return out, nil
}

// pcEnv reads process-compose's environment, a list of KEY=VALUE strings.
func pcEnv(n *yaml.Node) (map[string]string, error) {
	out := map[string]string{}
	if n.Kind == 0 {
		return out, nil
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return nil, err
	}
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("%q is not KEY=VALUE", kv)
		}
		out[k] = v
	}
	return out, nil
}

// renderImported writes the imported services as ACTIVE mabo-ctl.yaml entries.
//
// Unlike detection guesses, these are commands the operator already runs and
// explicitly asked to import, so they are written live. Every string is
// emitted as a JSON-quoted scalar — valid YAML that no newline, colon or quote
// in the source can break out of — and names are reduced to legal service
// names. A service whose text holds a template opener `{{` (which mabo-ctl
// would expand) is written commented out with the reason, as is anything that
// cannot be carried over safely.
func renderImported(svcs []imported, kind, from string) string {
	names := make(map[string]string, len(svcs)) // source name -> service name
	used := map[string]bool{}
	for i := range svcs {
		n := sanitizeName(svcs[i].source)
		for base, k := n, 2; used[n]; k++ {
			n = base + "-" + strconv.Itoa(k)
		}
		used[n] = true
		svcs[i].name = n
		names[svcs[i].source] = n
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# mabo-ctl.yaml — imported by `mabo-ctl init --from` from %s (%s).\n", yamlComment(from), kind)
	b.WriteString("# The commands are the ones that file runs, each through `sh -c`. Review the\n")
	b.WriteString("# TODOs, then `mabo-ctl preflight` and `mabo-ctl start`.\n")
	b.WriteString("# Annotated reference: examples/mabo-ctl.yaml in the mabo-ctl repository.\n\nservices:\n")
	for _, s := range svcs {
		lines := importedLines(s, names)
		if why := importBlocker(s); why != "" {
			fmt.Fprintf(&b, "\n  # %s: not imported live — %s\n", yamlComment(s.source), why)
			for _, l := range lines {
				fmt.Fprintf(&b, "  # %s\n", yamlComment(strings.TrimPrefix(l, "  ")))
			}
			continue
		}
		fmt.Fprintf(&b, "\n  # from %s %q\n", kind, yamlComment(s.source))
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

// importBlocker says why s must be written commented out, or "".
func importBlocker(s imported) string {
	texts := []string{s.command, s.dir}
	for k, v := range s.env {
		texts = append(texts, k, v)
	}
	for _, t := range texts {
		if strings.Contains(t, "{{") {
			return "its text contains {{, which mabo-ctl.yaml expands as a template"
		}
	}
	for k := range s.env {
		if !envKey.MatchString(k) {
			return fmt.Sprintf("environment key %q is not a valid variable name", yamlComment(k))
		}
	}
	if s.command == "" {
		return "it has no command"
	}
	return ""
}

// importedLines renders one service's YAML lines, two-space indented.
func importedLines(s imported, names map[string]string) []string {
	q := func(v string) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	lines := []string{"  - name: " + s.name, "    cmd: [sh, -c, " + q(s.command) + "]"}
	if s.dir != "" {
		lines = append(lines, "    dir: "+q(s.dir))
	}
	if s.port > 0 {
		lines = append(lines, "    port: "+strconv.Itoa(s.port))
	}
	if s.health != "" {
		lines = append(lines, "    health: "+q(s.health))
	}
	if s.disabled {
		lines = append(lines, "    autostart: false  # disabled: true in the source")
	}
	listOf := func(key string, src []string) {
		if len(src) == 0 {
			return
		}
		out := make([]string, 0, len(src))
		for _, d := range src {
			if n, ok := names[d]; ok {
				out = append(out, n)
			}
		}
		if len(out) > 0 {
			lines = append(lines, "    "+key+": ["+strings.Join(out, ", ")+"]")
		}
	}
	listOf("depends_on", s.deps)
	listOf("depends_ready_on", s.readyDeps)
	if len(s.env) > 0 {
		keys := make([]string, 0, len(s.env))
		for k := range s.env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		lines = append(lines, "    env:")
		for _, k := range keys {
			lines = append(lines, "      "+k+": "+q(s.env[k]))
		}
	}
	for _, n := range s.notes {
		lines = append(lines, "    # "+yamlComment(n))
	}
	return lines
}
