// Package config parses and validates mabo-ctl.yaml, the declarative service
// registry that makes the mabo-ctl binary repo-agnostic.
//
// The package is pure: it reads files and validates their contents. It never
// spawns a process, writes to disk, mutates the environment, or expands the
// {{.Port}} templates it validates — templates are held RAW here and expanded
// later by internal/service once every port is known.
//
// Every problem a mabo-ctl.yaml can have is a LOAD-TIME error, never a runtime
// surprise, and every problem is reported at once through a [ValidationError]
// rather than first-error-wins. Two of the rules are security controls and are
// documented as such: a service name composes .dev/logs/<name>.log and
// .dev/pids/<name>.pid, and a service dir must stay inside the project root.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FileName is the file Discover looks for while walking up the directory tree.
const FileName = "mabo-ctl.yaml"

// LegacyFileName is the pre-rename spelling of FileName, still discovered as a
// fallback for stacks written before mabo-ctl existed under its own name.
//
// It is looked for ONLY where FileName is absent: a directory holding both
// prefers the new name, and --config never consults it because an explicit
// path is already an explicit decision. Discovery reports the hit so the CLI
// can tell the operator to rename — working silently forever would mean the
// old name living in every user's repository indefinitely.
const LegacyFileName = "devctl.yaml"

// DefaultStopGrace is how long Stop waits after SIGTERM before it escalates to
// SIGKILL, when mabo-ctl.yaml does not set stop_grace.
const DefaultStopGrace = 10 * time.Second

// DefaultReadyTimeout is how long a readiness probe polls before a service is
// reported slow, when mabo-ctl.yaml does not set ready_timeout.
const DefaultReadyTimeout = 30 * time.Second

// ErrNotFound reports that no mabo-ctl.yaml exists at or above the starting
// directory. Test for it with errors.Is.
var ErrNotFound = errors.New("mabo-ctl.yaml not found")

// Config is a parsed, validated mabo-ctl.yaml.
type Config struct {
	Root         string // absolute dir containing mabo-ctl.yaml
	Path         string // absolute path to mabo-ctl.yaml
	Services     []Spec
	Checks       []Check
	Shells       []Shell
	StopGrace    time.Duration // default 10s
	ReadyTimeout time.Duration // default 30s
}

// Spec is a service exactly as declared. Cmd/Env and the parts of Health hold
// RAW templates; they are expanded later by service.Resolve once every port is
// known.
type Spec struct {
	Name      string            `yaml:"name"`
	Dir       string            `yaml:"dir"`
	Port      int               `yaml:"port"` // 0 = no port, no health
	Health    Health            `yaml:"health"`
	Cmd       []string          `yaml:"cmd"`
	Env       map[string]string `yaml:"env"`
	EnvFile   string            `yaml:"env_file"` // KEY=VALUE file; env: overrides it
	Runtime   string            `yaml:"runtime"`  // "", "system", "conda:<env>", "node:<ver>"
	DependsOn []string          `yaml:"depends_on"`
	Color     string            `yaml:"color"`
	// Open is the URL or path `mabo-ctl open` prefers over the derived
	// origin: "/docs" joins against the service's origin,
	// "http://host/page" is used as-is. RAW template; expanded by Resolve.
	Open string `yaml:"open"`

	// Autostart reports whether a bare `mabo-ctl start` includes this service.
	//
	// It is a POINTER so that "absent" and "false" are different things: the
	// zero value of a bool is false, and a plain bool field would have made
	// every service that did not mention autostart opt out of starting. nil
	// means the operator said nothing, which means yes.
	//
	// It gates ONLY the implicit selection. Naming the service explicitly —
	// `mabo-ctl start heavy` — always starts it, and so does being the dependency
	// of something that was named: a service must not come up against a
	// dependency that is not there.
	Autostart *bool `yaml:"autostart"`

	// ReadyTimeout is this service's readiness window, overriding the global
	// ready_timeout when set. It is a POINTER for the same reason Autostart is:
	// absent must mean "use the global", not "no wait at all".
	ReadyTimeout time.Duration
}

// Autostarts reports whether a bare `mabo-ctl start` should include this service.
// An unset autostart field means yes, which is what every config written before
// the field existed means.
func (s Spec) Autostarts() bool { return s.Autostart == nil || *s.Autostart }

// EnvFilePath resolves the service's env_file against the project root, the
// same anchor as dir. An empty EnvFile yields "". The path is cleaned but NOT
// checked against the root — that is validation's job at load time; callers
// that arrive without a load (there are none today) must not assume it.
func (s Spec) EnvFilePath(root string) string {
	if s.EnvFile == "" {
		return ""
	}
	if filepath.IsAbs(s.EnvFile) {
		return filepath.Clean(s.EnvFile)
	}
	return filepath.Clean(filepath.Join(root, s.EnvFile))
}

// Check is a preflight probe: exactly one of Command or TCP is set.
type Check struct {
	Name    string   `yaml:"name"`
	Command []string `yaml:"command"`
	TCP     string   `yaml:"tcp"` // "host:port"
}

// Shell is a named interactive command, e.g. a DB shell.
type Shell struct {
	Name    string   `yaml:"name"`
	Service string   `yaml:"service"` // env/dir to reuse; may be ""
	Command []string `yaml:"command"`
}

// repoMarkers are the entries that mark a directory as the top of a project.
//
// Discover checks the directory that holds one and then STOPS. They are not
// git-specific by accident: the question being asked is "where does this
// project end", and every one of these answers it for the tool that created it.
var repoMarkers = []string{".git", ".hg", ".svn"}

// isRepoRoot reports whether dir looks like the top of a project.
func isRepoRoot(dir string) bool {
	for _, m := range repoMarkers {
		if _, err := os.Lstat(filepath.Join(dir, m)); err == nil {
			return true
		}
	}
	return false
}

// Discover walks up from start looking for mabo-ctl.yaml and loads it.
// Returns ErrNotFound if the search reaches a boundary without finding one.
//
// An empty start means the current working directory. If start names a file
// rather than a directory, the search begins in its parent directory. Walking
// up is what makes mabo-ctl usable from a subdirectory, exactly as git is.
//
// # The walk is BOUNDED, and that is a security property
//
// It used to run to the filesystem root. Every command loads a config and most
// of them EXECUTE what it declares, so an unrelated mabo-ctl.yaml sitting in any
// ancestor — $HOME, /tmp, a shared parent of several checkouts — became the
// config for every project underneath it, and running a bare `mabo-ctl` in a deep
// subdirectory ran its commands without ever naming the file it got them from.
// The trust boundary mabo-ctl accepts is "whoever writes THIS repo's mabo-ctl.yaml
// can run code as you"; silently reaching outside the repo to find one is not
// that boundary, it is a way around it.
//
// Two limits now stop the walk, whichever comes first:
//
//   - a directory that holds a repo marker (see repoMarkers) is the last one
//     checked — the project ends there, so a config above it belongs to
//     something else;
//   - the user's home directory is the last one checked — outside a repo, that
//     is the outermost place a config can plausibly be the user's own.
//
// Both are checked AFTER looking in the directory itself, so a mabo-ctl.yaml that
// sits beside .git, or directly in $HOME, is still found. --config is the escape
// hatch and is never bounded: an explicit path is an explicit decision.
func Discover(start string) (*Config, bool, error) {
	path, viaLegacy, err := DiscoverPath(start)
	if err != nil {
		return nil, false, err
	}
	cfg, err := Load(path)
	return cfg, viaLegacy, err
}

// DiscoverPath performs [Discover]'s walk and returns the path it would load,
// WITHOUT parsing or validating it, plus whether that path was found under the
// legacy name (see [LegacyFileName]).
//
// It is separate so that `mabo-ctl config --raw` can print a mabo-ctl.yaml that does
// not parse — the moment an operator most needs to look at the file is the
// moment it is broken — and so that both callers walk the same bounded path.
// Two independent walks would eventually disagree, and then mabo-ctl would name
// one file and execute another.
//
// Within one directory FileName is checked before LegacyFileName; the first
// directory holding either wins. So a stack carrying both files loads the new
// one, and a stack still on the old name keeps working until it renames.
func DiscoverPath(start string) (path string, viaLegacy bool, err error) {
	dir := start
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", false, fmt.Errorf("config: resolve working directory: %w", err)
		}
		dir = wd
	}

	dir, absErr := filepath.Abs(dir)
	if absErr != nil {
		return "", false, fmt.Errorf("config: resolve %q: %w", start, absErr)
	}
	if fi, statErr := os.Stat(dir); statErr == nil && !fi.IsDir() {
		dir = filepath.Dir(dir)
	}

	// Two limits, and they answer two different questions.
	//
	// The BOUNDARY (a repo marker, or $HOME) answers "where does this project
	// end" and stops the climb there. It handles the common shapes and it is
	// why a mabo-ctl.yaml beside .git is found from any subdirectory.
	//
	// It is not enough on its own. A tree with neither marker nor $HOME above
	// it — /tmp, /opt, /srv, a mounted volume, a CI checkout without .git — has
	// no boundary, so the climb runs to the filesystem root. Refusing to climb
	// at all there was the first thing tried and it broke the feature: a
	// project unpacked into /opt/myapp is not a git repository and still wants
	// `mabo-ctl` to work from /opt/myapp/backend.
	//
	// So the second limit is about TRUST rather than extent: a config found by
	// CLIMBING must live in a directory only its owner can write. That is what
	// actually separates "my project's root" from "a file somebody else dropped
	// in a shared parent" — the case that makes an unbounded climb dangerous.
	// The starting directory is exempt: standing in a directory is a decision,
	// the same one `make` or `npm run` acts on.
	home, _ := os.UserHomeDir()
	from, stoppedAt, boundary := dir, dir, ""
	for {
		// FileName before LegacyFileName in every directory: where one carries
		// both spellings — mid-rename, or an operator keeping both out of
		// caution — the new name must win. The first directory holding either
		// stops the climb, so preference is proximity first, new-over-legacy
		// within it.
		for _, name := range []string{FileName, LegacyFileName} {
			candidate := filepath.Join(dir, name)
			if fi, statErr := os.Stat(candidate); statErr == nil && !fi.IsDir() {
				if dir != from {
					if err := climbableDir(dir); err != nil {
						return "", false, fmt.Errorf(
							"%w: found %s but refused it: %v. mabo-ctl only trusts a config it had to "+
								"climb to when the directory holding it is writable by its owner alone — "+
								"otherwise anyone who can write there chooses what mabo-ctl runs. "+
								"Pass --config to use it anyway",
							ErrNotFound, candidate, err)
					}
				}
				return candidate, name == LegacyFileName, nil
			}
		}
		stoppedAt = dir

		// The boundary checks come after the lookup, so the marker directory
		// itself is always searched — a mabo-ctl.yaml beside .git is the normal
		// case, not an edge one.
		if isRepoRoot(dir) {
			boundary = "the top of this repository"
			break
		}
		if home != "" && dir == home {
			boundary = "your home directory"
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	if boundary != "" {
		return "", false, fmt.Errorf(
			"%w: searched %s and every parent up to %s, which is %s, without finding a %s "+
				"(or its legacy spelling %s); mabo-ctl does not look outside it, because a config "+
				"further up belongs to something else — pass --config to use one anyway, "+
				"or run 'mabo-ctl init' in the starting directory to create one",
			ErrNotFound, from, stoppedAt, boundary, FileName, LegacyFileName)
	}
	return "", false, fmt.Errorf("%w: searched %s and every parent directory up to %s "+
		"without finding a %s (or its legacy spelling %s); pass --config to use one anyway, "+
		"or run 'mabo-ctl init' in the starting directory to create one",
		ErrNotFound, from, stoppedAt, FileName, LegacyFileName)
}

// Load reads and validates an explicit mabo-ctl.yaml path.
//
// The returned Config always carries an absolute Root and Path. Load returns a
// *ValidationError (retrievable with errors.As) when the file parses but
// violates one or more rules; every violation is listed, not just the first. A
// missing file wraps fs.ErrNotExist; a syntactically invalid file wraps the
// YAML decoder's error. Unknown keys are rejected rather than ignored, so a
// misspelled field is a load-time error instead of a silent default.
func Load(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config: resolve %q: %w", path, err)
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", abs, err)
	}

	var doc fileDoc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: parse %s: %w", abs, err)
	}
	// A second document would otherwise be silently ignored.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("config: parse %s: the file holds more than one YAML document; mabo-ctl.yaml must be a single document", abs)
	}

	cfg := &Config{
		Root:         filepath.Dir(abs),
		Path:         abs,
		Services:     make([]Spec, 0, len(doc.Services)),
		Checks:       doc.Checks,
		Shells:       doc.Shells,
		StopGrace:    DefaultStopGrace,
		ReadyTimeout: DefaultReadyTimeout,
	}
	if doc.StopGrace != nil {
		cfg.StopGrace = time.Duration(*doc.StopGrace)
	}
	if doc.ReadyTimeout != nil {
		cfg.ReadyTimeout = time.Duration(*doc.ReadyTimeout)
	}
	for _, s := range doc.Services {
		cfg.Services = append(cfg.Services, s.spec())
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Service returns the spec named n.
//
// The returned Spec is a deep copy: mutating its Cmd, Env or DependsOn does not
// affect the Config.
func (c *Config) Service(n string) (Spec, bool) {
	if c == nil {
		return Spec{}, false
	}
	for _, s := range c.Services {
		if s.Name == n {
			return s.clone(), true
		}
	}
	return Spec{}, false
}

// Names returns every declared service name in declaration order.
func (c *Config) Names() []string {
	if c == nil {
		return nil
	}
	names := make([]string, 0, len(c.Services))
	for _, s := range c.Services {
		names = append(names, s.Name)
	}
	return names
}

// LookupCheck returns the preflight check named n.
func (c *Config) LookupCheck(n string) (Check, bool) {
	if c == nil {
		return Check{}, false
	}
	for _, ck := range c.Checks {
		if ck.Name == n {
			return ck, true
		}
	}
	return Check{}, false
}

// LookupShell returns the shell named n.
func (c *Config) LookupShell(n string) (Shell, bool) {
	if c == nil {
		return Shell{}, false
	}
	for _, sh := range c.Shells {
		if sh.Name == n {
			return sh, true
		}
	}
	return Shell{}, false
}

// clone returns a copy of s that shares no maps or slices with it.
func (s Spec) clone() Spec {
	out := s
	if s.Cmd != nil {
		out.Cmd = append([]string(nil), s.Cmd...)
	}
	if s.DependsOn != nil {
		out.DependsOn = append([]string(nil), s.DependsOn...)
	}
	if s.Health.Argv != nil {
		out.Health.Argv = append([]string(nil), s.Health.Argv...)
	}
	if s.Env != nil {
		out.Env = make(map[string]string, len(s.Env))
		for k, v := range s.Env {
			out.Env[k] = v
		}
	}
	return out
}

// fileDoc mirrors the on-disk mabo-ctl.yaml document. It exists so the decoder
// can stay strict about unknown keys while still accepting the scalar forms a
// human writes (an unquoted integer env value, "10s" or a bare second count).
type fileDoc struct {
	// Schema carries the editor's `$schema:` reference. It is parsed so the
	// strict decoder accepts the key and IGNORED beyond that: it points an
	// editor at the schema, it says nothing to mabo-ctl.
	Schema       string         `yaml:"$schema"`
	StopGrace    *durationValue `yaml:"stop_grace"`
	ReadyTimeout *durationValue `yaml:"ready_timeout"`
	Services     []specDoc      `yaml:"services"`
	Checks       []Check        `yaml:"checks"`
	Shells       []Shell        `yaml:"shells"`
}

// specDoc mirrors Spec with a lenient env value type.
type specDoc struct {
	Name      string                 `yaml:"name"`
	Dir       string                 `yaml:"dir"`
	Port      int                    `yaml:"port"`
	Health    Health                 `yaml:"health"`
	Cmd       []string               `yaml:"cmd"`
	Env       map[string]scalarValue `yaml:"env"`
	EnvFile   string                 `yaml:"env_file"`
	Runtime   string                 `yaml:"runtime"`
	DependsOn []string               `yaml:"depends_on"`
	Color     string                 `yaml:"color"`
	Open      string                 `yaml:"open"`
	Autostart *bool                  `yaml:"autostart"`
	// ReadyTimeout is a pointer so an absent key means "inherit the global".
	ReadyTimeout *durationValue `yaml:"ready_timeout"`
}

func (d specDoc) spec() Spec {
	s := Spec{
		Name:      d.Name,
		Dir:       d.Dir,
		Port:      d.Port,
		Health:    d.Health,
		Cmd:       d.Cmd,
		EnvFile:   d.EnvFile,
		Runtime:   d.Runtime,
		DependsOn: d.DependsOn,
		Color:     d.Color,
		Open:      d.Open,
		Autostart: d.Autostart,
	}
	if d.ReadyTimeout != nil {
		s.ReadyTimeout = time.Duration(*d.ReadyTimeout)
	}
	if d.Env != nil {
		s.Env = make(map[string]string, len(d.Env))
		for k, v := range d.Env {
			s.Env[k] = string(v)
		}
	}
	return s
}

// scalarValue is a YAML scalar of any tag read as a string, so that
// `env: {WORKERS: 4}` does not have to be quoted to be valid.
type scalarValue string

// UnmarshalYAML decodes any scalar node into a string and rejects
// sequences and mappings.
func (s *scalarValue) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: env values must be scalars, got %s", node.Line, nodeKind(node.Kind))
	}
	if node.Tag == "!!null" {
		*s = ""
		return nil
	}
	*s = scalarValue(node.Value)
	return nil
}

// durationValue is a Go duration string ("10s", "1m30s") or a bare number of
// seconds.
type durationValue time.Duration

// UnmarshalYAML accepts a duration string understood by time.ParseDuration or
// a bare integer, which is read as a count of seconds. It returns an error for
// any other scalar and for non-scalar nodes.
func (d *durationValue) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: expected a duration such as \"10s\", got %s", node.Line, nodeKind(node.Kind))
	}
	raw := strings.TrimSpace(node.Value)
	if v, err := time.ParseDuration(raw); err == nil {
		*d = durationValue(v)
		return nil
	}
	if n, err := strconv.Atoi(raw); err == nil {
		*d = durationValue(time.Duration(n) * time.Second)
		return nil
	}
	return fmt.Errorf("line %d: %q is not a duration; use a Go duration string such as \"10s\", \"500ms\" or \"1m30s\"", node.Line, raw)
}

// nodeKind names a yaml.Node kind for an error message.
func nodeKind(k yaml.Kind) string {
	switch k {
	case yaml.DocumentNode:
		return "a document"
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	case yaml.ScalarNode:
		return "a scalar"
	case yaml.AliasNode:
		return "an alias"
	default:
		return "an unknown node"
	}
}

// ParseEnvFile reads a KEY=VALUE environment file: one variable per line,
// blank lines and `#` comments skipped, the value being everything after the
// first `=` with surrounding spaces trimmed (so `KEY = value` and quoted
// values behave as the file author wrote them). It names the file and line
// number for any line that is none of those, because an env file that half
// parses is worse than one that refuses to.
//
// Later lines override earlier ones: the file is read top to bottom and a map
// cannot express the difference.
func ParseEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read env file %s: %w", path, err)
	}
	out := make(map[string]string)
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("config: %s line %d: %q is not KEY=VALUE; env files hold one VARIABLE=value per line", path, i+1, line)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}
