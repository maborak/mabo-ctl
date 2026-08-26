package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/redact"
	"github.com/maborak/mabo-ctl/internal/service"
)

// ConfigView is the answer to "why is this service on 7999, and what is it
// actually running?" — the loaded mabo-ctl.yaml after port precedence, template
// expansion and runtime resolution have all had their say.
//
// It is built once, by [BuildConfigView], and rendered two ways: as text by
// [Renderer.ConfigBlock] and as JSON by [ConfigJSON]. Deriving it once is the
// point. Every value in it — the port, the source that won, the absolute
// interpreter — already exists inside service.Resolve's output, and a front end
// that re-derived the precedence chain to display it would be free to disagree
// with the supervisor that acted on it.
//
// Everything in it is ALREADY REDACTED. There is no unredacted variant and no
// flag to ask for one: `mabo-ctl config --raw` prints the file on disk when the
// operator wants the literal bytes, and every other path through this type is
// safe to put on a web page or in a terminal recording.
type ConfigView struct {
	// Source describes the file that was loaded and the settings that are not
	// per-service.
	Source ConfigSource `json:"source"`
	// Services holds one entry per declared service, in declaration order.
	Services []ConfigService `json:"services"`
}

// ConfigSource is where the configuration came from.
//
// The path is here because config discovery WALKS UP the directory tree: the
// mabo-ctl.yaml that won may belong to a parent repository the operator was not
// thinking of, and no other command prints which file it read.
type ConfigSource struct {
	// Path is the absolute path of the mabo-ctl.yaml that was loaded.
	Path string `json:"path"`
	// Root is the absolute directory that Path sits in, which every service
	// dir is resolved against.
	Root string `json:"root"`
	// StateDir is the absolute path of the state directory, `.dev`.
	StateDir string `json:"state_dir"`
	// Explicit reports that Path came from --config rather than from walking up
	// from the working directory.
	Explicit bool `json:"explicit"`
	// StopGraceMS is how long stop waits after SIGTERM before SIGKILL.
	StopGraceMS int64 `json:"stop_grace_ms"`
	// ReadyTimeoutMS is how long a readiness probe polls before a service is
	// reported slow, and past which it is reported degraded.
	ReadyTimeoutMS int64 `json:"ready_timeout_ms"`
}

// ConfigService is one service as it resolved, not as it was declared.
type ConfigService struct {
	// Name is the declared service name.
	Name string `json:"name"`
	// Dir is the resolved absolute working directory.
	Dir string `json:"dir"`
	// Port is the resolved port, 0 for a portless service.
	Port int `json:"port"`
	// PortSource is the precedence level that produced Port: "flag", "env",
	// "run.env" or "default". It is the field this whole view exists for.
	PortSource string `json:"port_source"`
	// PortDeclared is the port mabo-ctl.yaml declares, which is NOT necessarily
	// the port in use.
	PortDeclared int `json:"port_declared"`
	// PortOverride reports that a persisted .dev/run.env port is outranking a
	// declared default that has since changed — the documented trap, where
	// editing mabo-ctl.yaml appears to do nothing.
	PortOverride bool `json:"port_override"`
	// Runtime is the declared runtime string ("", "system", "conda:<env>" or
	// "node:<version>") that chose the interpreter.
	Runtime string `json:"runtime"`
	// Autostart reports whether a bare `mabo-ctl start` includes this service.
	// It is rendered because a service that quietly does not start is otherwise
	// indistinguishable from one that failed to.
	Autostart bool `json:"autostart"`
	// Cmd is the expanded argv with credentials redacted. Cmd[0] is the
	// absolute interpreter path unless CmdError is set.
	Cmd []string `json:"cmd"`
	// CmdLine is Cmd rendered as one shell-quoted line, for copying.
	CmdLine string `json:"cmd_line"`
	// CmdError is why Cmd[0] could not be resolved against Runtime, when it
	// could not be. The service is displayable but not startable in that state,
	// and Cmd[0] is the UNRESOLVED name.
	CmdError string `json:"cmd_error,omitempty"`
	// Health is the expanded readiness URL with credentials redacted, "" when
	// the service declares no probe.
	Health string `json:"health"`
	// Env is the DECLARED environment, values redacted. It is never the
	// resolved environment, which is the caller's entire environment; see
	// redact.Env.
	Env []redact.Var `json:"env"`
	// EnvFile is the service's declared env_file path as written, "" when it
	// declares none. The file's values are merged into Env at resolve time and
	// redacted there; this row exists so the reader knows the file exists.
	EnvFile string `json:"env_file,omitempty"`
	// ReadyTimeout is the service's own readiness window, 0 when it inherits
	// the global ready_timeout. Shown only when set, for the same reason the
	// autostart row is.
	ReadyTimeout time.Duration `json:"ready_timeout,omitempty"`
	// Open is the expanded `open:` target, "" when the service declares none.
	Open string `json:"open,omitempty"`
	// DependsOn lists the services that start first.
	DependsOn []string `json:"depends_on"`
	// Color is the label colour declared in mabo-ctl.yaml, "" when none.
	Color string `json:"color"`
}

// ConfigInput is everything [BuildConfigView] needs. It is a struct rather than
// a parameter list so a field can be added without breaking every caller.
type ConfigInput struct {
	// Config is the loaded mabo-ctl.yaml. A nil Config yields an empty view.
	Config *config.Config
	// Instances are the resolved services from service.Resolve.
	Instances []service.Instance
	// Origins are the port origins from the SAME service.Resolve call. They are
	// taken rather than recomputed: the precedence chain has one implementation
	// and this is a reader of it.
	Origins []service.Origin
	// StateDir is the absolute path of `.dev`, or "" when it does not exist.
	StateDir string
	// Explicit reports that the config path came from --config.
	Explicit bool
}

// BuildConfigView assembles the resolved view, redacting as it goes.
//
// Only the services named in in.Instances appear, so a caller narrowing to one
// service passes one instance. Origins are matched by name; a service with no
// Origin — which service.Resolve does not produce — reports its declared port
// with an empty source rather than inventing one.
func BuildConfigView(in ConfigInput) ConfigView {
	view := ConfigView{Services: make([]ConfigService, 0, len(in.Instances))}
	if in.Config == nil {
		return view
	}

	view.Source = ConfigSource{
		Path:           in.Config.Path,
		Root:           in.Config.Root,
		StateDir:       in.StateDir,
		Explicit:       in.Explicit,
		StopGraceMS:    in.Config.StopGrace.Milliseconds(),
		ReadyTimeoutMS: in.Config.ReadyTimeout.Milliseconds(),
	}

	origins := make(map[string]service.Origin, len(in.Origins))
	for _, o := range in.Origins {
		origins[o.Service] = o
	}

	for _, inst := range in.Instances {
		cmd := redact.Args(inst.Cmd)
		svc := ConfigService{
			Name:      inst.Name,
			Dir:       inst.Dir,
			Port:      inst.Port,
			Runtime:   inst.Runtime,
			Autostart: inst.Autostarts(),
			Cmd:       cmd,
			CmdLine:   ShellLine(cmd),
			Health:    redact.URL(inst.Health),
			Open:      redact.URL(inst.Open),
			Env:       []redact.Var{},
			DependsOn: append([]string{}, inst.DependsOn...),
			Color:     inst.Color,
		}
		if o, ok := origins[inst.Name]; ok {
			svc.PortSource = string(o.Source)
			svc.PortDeclared = o.Declared
			svc.PortOverride = o.Override
		}
		if inst.CmdErr != nil {
			svc.CmdError = inst.CmdErr.Error()
		}
		// The declared environment comes from the config, never from
		// Instance.Env, which is the whole environment mabo-ctl was started with.
		if spec, ok := in.Config.Service(inst.Name); ok {
			svc.Env = redact.Env(spec.Env)
			svc.EnvFile = spec.EnvFile
			svc.ReadyTimeout = spec.ReadyTimeout
		}
		view.Services = append(view.Services, svc)
	}
	return view
}

// ConfigJSON renders v as the body of `mabo-ctl config --json`.
//
// Unlike [StatusJSON] this is NOT a frozen contract — `mabo-ctl status --json` is
// the documented integration point and this is a debugging aid — but it is
// deterministic, indented, and HTML-unescaped so a health URL containing & or ?
// survives verbatim. The result has no trailing newline.
func ConfigJSON(v ConfigView) ([]byte, error) {
	if v.Services == nil {
		v.Services = []ConfigService{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("ui: encoding config JSON: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// configFieldWidth is the width the per-service field labels are padded to, so
// the values line up in a column the eye can run down.
const configFieldWidth = 9

// ConfigBlock renders the resolved configuration for a human: where it was
// loaded from, what every service resolved to, and — when raw is non-empty —
// the file itself underneath.
//
// The port line always names the SOURCE beside the number, because the four
// precedence levels are invisible everywhere else in the tool and "why is this
// service on 7999?" has no other answer short of reading mabo-ctl.yaml, the
// environment and .dev/run.env together. An override is called out in the same
// line rather than in a footnote: a persisted port beating a changed default is
// the trap that cost a real debugging round.
//
// raw is expected to have been through redact.YAML — this package renders, it
// does not decide what is a secret. The result has no trailing newline.
func (r *Renderer) ConfigBlock(v ConfigView, raw string) string {
	var b strings.Builder

	b.WriteString(r.configSource(v.Source))
	for _, svc := range v.Services {
		b.WriteString("\n\n")
		b.WriteString(r.configService(svc))
	}
	if raw != "" {
		b.WriteString("\n\n")
		b.WriteString(r.paint(style{"1"}, v.Source.Path))
		b.WriteString("\n")
		b.WriteString(r.paint(style{"2"},
			"credential-shaped values are redacted; `mabo-ctl config --raw` prints the file verbatim"))
		b.WriteString("\n")
		b.WriteString(strings.TrimRight(raw, "\n"))
	}
	return b.String()
}

// configSource renders the first section: which file won, and the settings that
// belong to no single service.
func (r *Renderer) configSource(s ConfigSource) string {
	discovery := "found by walking up from the working directory"
	if s.Explicit {
		discovery = "given with --config"
	}
	lines := []string{
		r.configField("config", s.Path+"  "+r.paint(style{"2"}, "("+discovery+")")),
		r.configField("root", s.Root),
	}
	if s.StateDir != "" {
		lines = append(lines, r.configField("state", s.StateDir))
	}
	lines = append(lines, r.configField("timeouts", fmt.Sprintf(
		"stop_grace %s   ready_timeout %s",
		formatDuration(time.Duration(s.StopGraceMS)*time.Millisecond),
		formatDuration(time.Duration(s.ReadyTimeoutMS)*time.Millisecond))))
	return strings.Join(lines, "\n")
}

// configService renders one service's resolved values.
func (r *Renderer) configService(s ConfigService) string {
	lines := []string{r.paint(r.serviceStyle(s.Name), s.Name)}

	lines = append(lines, r.configField("port", r.configPort(s)))
	lines = append(lines, r.configField("dir", s.Dir))

	cmd := s.CmdLine
	if s.CmdError != "" {
		cmd += "  " + r.paint(style{"31"}, "(unresolved)")
	}
	lines = append(lines, r.configField("cmd", cmd))
	if s.CmdError != "" {
		lines = append(lines, r.configField("error", r.paint(style{"31"}, s.CmdError)))
	}

	runtime := s.Runtime
	if runtime == "" {
		runtime = "system" + "  " + r.paint(style{"2"}, "(none declared)")
	}
	if len(s.Cmd) > 0 && s.CmdError == "" {
		runtime += "  →  " + s.Cmd[0]
	}
	lines = append(lines, r.configField("runtime", runtime))

	// Only shown when it is false. A line saying "autostart yes" on every
	// service would be noise on the setting nobody changed; the whole value of
	// printing this is telling the reader why a service they expected to be
	// running is not.
	if !s.Autostart {
		lines = append(lines, r.configField("autostart",
			"no"+"  "+r.paint(style{"2"}, "(a bare `mabo-ctl start` skips it; name it to start it)")))
	}

	if s.Health != "" {
		lines = append(lines, r.configField("health", s.Health))
	} else {
		lines = append(lines, r.configField("health", r.paint(style{"2"}, "no readiness probe declared")))
	}
	if len(s.DependsOn) > 0 {
		lines = append(lines, r.configField("depends", strings.Join(s.DependsOn, ", ")))
	}
	if s.Open != "" {
		lines = append(lines, r.configField("open", s.Open))
	}
	if s.EnvFile != "" {
		lines = append(lines, r.configField("env_file", s.EnvFile))
	}
	if s.ReadyTimeout > 0 {
		lines = append(lines, r.configField("ready_timeout", s.ReadyTimeout.String()+
			"  "+r.paint(style{"2"}, "(overrides the global)")))
	}
	for i, e := range s.Env {
		label := ""
		if i == 0 {
			label = "env"
		}
		value := e.Value
		if e.Redacted {
			value = r.paint(style{"2"}, e.Value)
		}
		lines = append(lines, r.configField(label, e.Key+"="+value))
	}
	return strings.Join(lines, "\n")
}

// configPort renders the port line: the number, the precedence level that
// produced it, and the declared value when the two differ.
func (r *Renderer) configPort(s ConfigService) string {
	if s.Port == 0 {
		return missing + "  " + r.paint(style{"2"}, "(portless)")
	}
	source := s.PortSource
	if source == "" {
		source = "unknown"
	}
	out := fmt.Sprintf("%d  from %s", s.Port, source)
	switch {
	case s.PortOverride:
		out += "  " + r.paint(style{"1;33"}, fmt.Sprintf(
			"(OVERRIDES the declared %d — adopt it with `mabo-ctl --refresh-ports`, or clear it with `mabo-ctl reset`)",
			s.PortDeclared))
	case s.PortDeclared != 0 && s.PortDeclared != s.Port:
		out += "  " + r.paint(style{"2"}, fmt.Sprintf("(declared %d)", s.PortDeclared))
	}
	return out
}

// configField renders one padded "label  value" line. An empty label produces
// the indentation alone, which is how a multi-value field such as env keeps its
// continuation lines in the value column.
func (r *Renderer) configField(label, value string) string {
	if label != "" {
		label = r.paint(style{"2"}, label)
	}
	return "  " + pad(label, configFieldWidth) + colGap + value
}
