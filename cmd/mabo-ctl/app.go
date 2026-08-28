package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/state"
	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/ui"
)

// lifecycle is the part of *supervisor.Supervisor the one-shot commands drive.
//
// It is an interface for one reason: a test can then observe which services a
// command selected, and what exit code the resulting statuses produce, without
// spawning a real process. Production always uses the concrete supervisor, and
// the interactive console is always handed the concrete supervisor too, because
// it needs the whole type.
type lifecycle interface {
	// Start starts names (empty means all) and reports progress on ev.
	Start(ctx context.Context, names []string, ev chan<- supervisor.Event) error
	// Stop stops names (empty means all) and reports progress on ev.
	Stop(ctx context.Context, names []string, ev chan<- supervisor.Event) error
	// Restart stops then starts names (empty means all).
	Restart(ctx context.Context, names []string, ev chan<- supervisor.Event) error
	// Status probes every service and returns one Status each.
	Status(ctx context.Context) []supervisor.Status
	// StatusNoPorts is Status without the port-holder lookup, for pollers that
	// read only the phase and never render DETAIL.
	StatusNoPorts(ctx context.Context) []supervisor.Status
	// Reset stops everything, removes the state directory, and — only when
	// force is true — kills whatever still holds a declared port.
	Reset(ctx context.Context, force bool, ev chan<- supervisor.Event) error
	// Tail streams svc's log on out. follow=false returns the last n lines.
	Tail(ctx context.Context, svc string, n int, follow bool, out chan<- string) error
}

// app holds everything the commands share: the environment, the loaded config,
// the captured caller ports, and the lazily built supervisor.
//
// One app exists per process. Its config is loaded at most once and its ports
// are captured at most once, both before any command body runs.
type app struct {
	env *Env

	// configPath is the value of the global --config flag, or "" for discovery.
	configPath string
	// profilesArg is the parsed --profile flag value, or nil when the flag was
	// not given (MABO_PROFILE then applies, then the empty set).
	profilesArg []string

	cfg    *config.Config
	cfgErr error
	loaded bool

	// captured holds the caller's <NAME>_PORT variables, already removed from
	// the process environment by service.CaptureEnv.
	captured     map[string]string
	capturedOnce bool

	// announcedCfg records that the discovered config path has been named on
	// stderr, so a session that re-loads (the interactive prompt, a --config
	// reconcile) says it once rather than on every command.
	announcedCfg bool

	// viaLegacyCfg records that discovery found the config under the pre-rename
	// spelling (config.LegacyFileName). The config still loads — an old stack
	// must keep working — but announceDiscovery tells the operator to rename,
	// because a fallback that stays silent forever is just the old name living
	// on indefinitely.
	viaLegacyCfg bool

	// ports holds the parsed --ports slots of the executing command.
	ports portsFlag

	// portOverrides holds the parsed --port SERVICE=PORT overrides, when any
	// were given. They outrank every other level; --ports and --port are
	// rejected together in adoptPorts.
	portOverrides map[string]int

	// refreshPorts records the global --refresh-ports flag: re-resolve every
	// port from the declared defaults, ignoring the persisted .dev/run.env
	// level, and rewrite the file so later invocations agree. It is the
	// non-interactive form of the drift prompt [app.reconcilePorts] asks.
	refreshPorts bool

	// jsonContract records that this command emits the stable machine contract
	// on stdout (`status --json`). Nothing human-facing may be interpolated into
	// such a run, including the port-drift prompt.
	jsonContract bool

	// allowOrigins holds --allow-origin: extra browser origins the web console
	// accepts, for a console reached through a tunnel or a port forward.
	allowOrigins []string

	// inREPL reports that the interactive console is running and dispatching
	// into this same command tree. It is what stops `repl` inside `repl` from
	// giving two loops one stdin.
	inREPL bool

	rend *ui.Renderer

	st  *state.Dir
	sup *supervisor.Supervisor
	lc  lifecycle

	insts []service.Instance
	// origins explains where each resolved port came from. [app.resolve]
	// already computes it to print the override notice, and it is kept because
	// the answer to "why is this service on 7999?" is worth more than one
	// warning line: the web console renders it on /api/config.
	origins  []service.Origin
	resolved bool
}

// newApp returns an app bound to e. e must already have been passed through
// [normalize].
func newApp(e *Env) *app { return &app{env: e} }

// bootstrap loads the config and captures the caller's <NAME>_PORT variables.
//
// It runs before cobra parses anything, on the main goroutine, and it is the
// only place [service.CaptureEnv] is called. Capture must happen before any
// child is spawned, and every command that can spawn one runs after this.
//
// A config that fails to load is remembered, not reported: `mabo-ctl --help` and
// `mabo-ctl completion bash` work in a directory with no mabo-ctl.yaml. The error
// surfaces from [app.config] the moment a command actually needs the file, and
// nothing can spawn without it, so skipping the capture is safe.
func (a *app) bootstrap() {
	a.configPath = peekConfig(a.env.Args)
	a.profilesArg = peekProfile(a.env.Args)
	a.load()
	a.capture()
}

// load reads the config once, from --config when given and by walking up from
// the working directory otherwise. Repeated calls are no-ops.
func (a *app) load() {
	if a.loaded {
		return
	}
	a.loaded = true

	var (
		cfg       *config.Config
		viaLegacy bool
		err       error
	)
	if a.configPath != "" {
		cfg, err = config.Load(a.configPath)
	} else {
		cfg, viaLegacy, err = config.Discover(a.env.Wd)
		a.viaLegacyCfg = viaLegacy
	}
	if err != nil {
		a.cfgErr = withCode(exitConfig, err)
		return
	}
	a.cfg = cfg
	if err := cfg.ApplyProfiles(a.activeProfiles()); err != nil {
		a.cfgErr = withCode(exitConfig, err)
		return
	}
	a.announceDiscovery()
}

// activeProfiles resolves the active profile set: the --profile flag wins over
// MABO_PROFILE, and both being absent means the empty set, under which every
// declared service is visible — the pre-profiles behaviour, byte for byte.
func (a *app) activeProfiles() []string {
	if a.profilesArg != nil {
		return a.profilesArg
	}
	if env := os.Getenv("MABO_PROFILE"); env != "" {
		return parseProfiles(env)
	}
	return nil
}

// parseProfiles splits a comma-separated profile list, trimming whitespace
// and dropping empties so "a,, b" means the same thing as "a,b".
func parseProfiles(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// announceDiscovery names the config file when it was found somewhere OTHER
// than the working directory.
//
// Every command loads a config and most of them execute what it declares, so
// "which file did these commands come from" is not a detail — it is the whole
// trust decision. `mabo-ctl config` always answered it; `mabo-ctl start` answered
// it nowhere, so a bare mabo-ctl in a subdirectory ran a parent's services
// without ever naming the file.
//
// It is deliberately quiet in the ordinary case. A mabo-ctl.yaml in the directory
// the user is standing in is the file they obviously meant, and a line saying so
// on every command would be noise that teaches people to stop reading. It goes
// to stderr so it can never corrupt --json on stdout.
func (a *app) announceDiscovery() {
	if a.configPath != "" || a.cfg == nil || a.announcedCfg {
		return
	}
	a.announcedCfg = true

	// env.Wd is empty whenever the caller did not inject one, which is the
	// normal production path — Discover then walks from the real working
	// directory. Resolving it here is what keeps the comparison below honest;
	// without it the message printed a blank path AND fired for a mabo-ctl.yaml
	// sitting in the directory the user was already standing in.
	wd := a.env.Wd
	if wd == "" {
		var err error
		if wd, err = os.Getwd(); err != nil {
			return
		}
	}

	// The legacy-name notice comes FIRST and deliberately ignores where the
	// file was found: a devctl.yaml in the directory the user stands in loads
	// quietly as far as discovery is concerned, but it is still a file that
	// needs renaming, and this line is the one thing that says so.
	if a.viaLegacyCfg {
		fmt.Fprintf(a.env.Stderr,
			"mabo-ctl: note: %s uses the legacy name — rename it to mabo-ctl.yaml "+
				"(the old spelling keeps working for now)\n", a.cfg.Path)
		return
	}

	if filepath.Dir(a.cfg.Path) == wd {
		return
	}
	fmt.Fprintf(a.env.Stderr, "mabo-ctl: using %s (found by walking up from %s)\n", a.cfg.Path, wd)
}

// capture takes the caller's <NAME>_PORT variables out of the environment,
// exactly once per config, and only once a config exists to say which names to
// look for.
//
// The result is MERGED rather than assigned, because [app.reconcileConfig] can
// legitimately run it a second time against a different config. CaptureEnv
// unsets what it reads, so a variable taken under the first config is already
// gone from the environment by then; overwriting the map would drop that value
// on the floor while leaving the port unresolvable from anywhere else. An
// earlier capture therefore wins, and only names it never saw are added.
func (a *app) capture() {
	if a.capturedOnce || a.cfg == nil {
		return
	}
	a.capturedOnce = true
	got := service.CaptureEnv(a.cfg.Names())
	if a.captured == nil {
		a.captured = got
		return
	}
	for k, v := range got {
		if _, seen := a.captured[k]; !seen {
			a.captured[k] = v
		}
	}
}

// reconcileConfig re-reads the config when cobra's parse of --config disagrees
// with the raw-argument peek [bootstrap] used.
//
// It runs from the root command's PersistentPreRunE, still before any command
// body, so a corrected config is loaded and its ports captured before anything
// can spawn.
func (a *app) reconcileConfig(parsed string) {
	if parsed == a.configPath {
		a.capture()
		return
	}
	a.configPath = parsed
	a.loaded, a.cfg, a.cfgErr = false, nil, nil
	a.resolved, a.sup, a.lc, a.insts, a.st, a.origins = false, nil, nil, nil, nil, nil

	// Re-read and re-capture, which the reset above alone did not do. Without
	// it this branch silently dropped an entire precedence level: the corrected
	// config declares the service names, and nothing had asked the environment
	// about them, so <NAME>_PORT was skipped for the whole invocation. Worse, a
	// variable that is never captured is never UNSET either, so it stayed in the
	// environment forwarded to every child — a service told it is on one port
	// while mabo-ctl supervises it on another, which is the exact failure
	// service.CaptureEnv exists to prevent.
	a.capturedOnce = false
	a.load()
	a.capture()
}

// invalidateResolution drops the memoised resolution so the next call re-reads
// the ports, re-expands the templates and rebuilds the supervisor.
//
// The interactive console needs this: it resolves once before the prompt opens,
// so without it a --ports typed at the prompt parsed correctly, set the flag,
// and then changed nothing at all.
func (a *app) invalidateResolution() {
	a.resolved, a.sup, a.lc, a.insts, a.st, a.origins = false, nil, nil, nil, nil, nil
	a.load()
	a.capture()
}

// config returns the loaded config, or the error that prevented loading it,
// already tagged with exit code 3.
func (a *app) config() (*config.Config, error) {
	a.load()
	if a.cfgErr != nil {
		return nil, a.cfgErr
	}
	return a.cfg, nil
}

// renderer returns the output renderer, building it on first use from the
// caller's stdout so NO_COLOR, TERM=dumb and a piped stdout are all honoured.
// A test may supply its own through [Env.Renderer].
func (a *app) renderer() *ui.Renderer {
	if a.rend != nil {
		return a.rend
	}
	switch {
	case a.env.Renderer != nil:
		a.rend = a.env.Renderer
	default:
		a.rend = ui.New(asFile(a.env.Stdout))
	}
	return a.rend
}

// asFile returns w as an *os.File when it is one, and nil otherwise, so the
// renderer can inspect a real terminal without the rest of the CLI having to
// know whether it is writing to one. ui.New treats a nil file as "not a
// terminal" and renders plain text.
func asFile(w io.Writer) *os.File {
	f, _ := w.(*os.File)
	return f
}

// resolve loads the config, creates the state directory, resolves every port
// and expands every template, exactly once per process.
//
// It hands the port-override conversation to [app.reconcilePorts], which prints
// the drift notice to stderr — a persisted .dev/run.env value outranking a
// changed default is the trap this tool exists to stop being silent — and, on
// an interactive terminal, offers to adopt the declared ports. The notice goes
// to stderr so `status --json` stays a clean machine contract on stdout.
func (a *app) resolve() ([]service.Instance, error) {
	if a.resolved {
		if a.cfgErr != nil {
			return nil, a.cfgErr
		}
		return a.insts, nil
	}

	cfg, err := a.config()
	if err != nil {
		return nil, err
	}
	st, err := state.New(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("mabo-ctl: prepare state directory: %w", err)
	}

	insts, origins, err := service.Resolve(cfg, st, service.Options{
		Ports:         a.ports.values,
		PortOverrides: a.portOverrides,
		EnvVars:       a.captured,
		IgnoreRunEnv:  a.refreshPorts,
	})
	if err != nil {
		return nil, err
	}
	insts, origins, err = a.reconcilePorts(cfg, st, insts, origins)
	if err != nil {
		return nil, err
	}

	a.resolved = true
	a.st = st
	a.insts = insts
	a.origins = origins
	a.renderer().SetInstanceColors(insts)
	return insts, nil
}

// supervisor returns the lifecycle backend and the resolved instances,
// constructing them on first use.
func (a *app) supervisor() (lifecycle, []service.Instance, error) {
	insts, err := a.resolve()
	if err != nil {
		return nil, nil, err
	}
	if a.lc == nil {
		cfg, err := a.config()
		if err != nil {
			return nil, nil, err
		}
		a.sup = supervisor.New(cfg, a.st, insts)
		a.lc = a.env.NewSupervisor(a.sup)
	}
	return a.lc, insts, nil
}

// realSupervisor returns the concrete supervisor, which the interactive console
// needs in full rather than through the [lifecycle] interface.
func (a *app) realSupervisor() (*supervisor.Supervisor, error) {
	if _, _, err := a.supervisor(); err != nil {
		return nil, err
	}
	return a.sup, nil
}

// instance returns the resolved instance named n.
func (a *app) instance(cmd *cobra.Command, n string) (service.Instance, error) {
	insts, err := a.resolve()
	if err != nil {
		return service.Instance{}, err
	}
	for _, in := range insts {
		if in.Name == n {
			// Every caller of instance is about to run something in this
			// service's environment. A runtime that did not resolve means that
			// environment is incomplete — the runtime's bin directory is not on
			// the child's PATH — so `mabo-ctl exec backend pytest` would silently
			// run whatever pytest the ambient PATH offers instead of the
			// declared runtime's. Refusing is the whole point of runtime:.
			if err := in.Runnable(); err != nil {
				return service.Instance{}, err
			}
			return in, nil
		}
	}
	return service.Instance{}, a.unknownService(cmd, n)
}

// selection turns positional arguments and --all into the service list a
// lifecycle call takes. An empty result means every service.
//
// An unknown name is a usage error, exit code 2, naming every declared service:
// a typo must never be reported as a service that mysteriously does nothing,
// which is exactly how the shell predecessor's silent fall-through behaved.
func (a *app) selection(cmd *cobra.Command, args []string) ([]string, error) {
	all := false
	if f := cmd.Flags().Lookup("all"); f != nil {
		all = f.Value.String() == "true"
	}
	if all && len(args) > 0 {
		return nil, usageErrorf("--all cannot be combined with service names (%s)", strings.Join(args, ", "))
	}
	// --all and "named nothing" are DIFFERENT, and collapsing them was a bug.
	//
	// Naming nothing is a default, and `autostart: false` is how an operator
	// changes that default. --all is an instruction, in the same class as typing
	// the names out — so it means every declared service, autostart or not. A
	// stack where every service opts out turned "Start all" into a button that
	// did nothing and said "no service has autostart enabled", which is a true
	// sentence and a broken control.
	if all {
		cfg, err := a.config()
		if err != nil {
			return nil, err
		}
		return cfg.Names(), nil
	}
	if len(args) == 0 {
		return nil, nil
	}
	if err := a.validateNames(cmd, args); err != nil {
		return nil, err
	}
	return args, nil
}

// validateNames reports the first argument that is not a declared service.
func (a *app) validateNames(cmd *cobra.Command, names []string) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}
	declared := make(map[string]bool, len(cfg.Services))
	for _, n := range cfg.Names() {
		declared[n] = true
	}
	for _, n := range names {
		if !declared[n] {
			return a.unknownService(cmd, n)
		}
	}
	return nil
}

// unknownService builds the usage error for a name that is not a declared
// service. It lists every valid name, and adds a command suggestion when the
// argument looks like a mistyped subcommand — `mabo-ctl staus` should not be
// reported only as an unknown service.
func (a *app) unknownService(cmd *cobra.Command, name string) error {
	valid := "none — mabo-ctl.yaml declares no services"
	if cfg, err := a.config(); err == nil && len(cfg.Services) > 0 {
		valid = strings.Join(cfg.Names(), ", ")
	}
	msg := fmt.Sprintf("unknown service %q; declared services are: %s", name, valid)
	if cmd != nil {
		if s := cmd.Root().SuggestionsFor(name); len(s) > 0 {
			msg += fmt.Sprintf("\n(%q is not a mabo-ctl command either; did you mean %q?)", name, s[0])
		}
	}
	return usageError(fmt.Errorf("%s", msg))
}

// pumpEvents starts a goroutine that renders supervisor events to stdout as
// they arrive, and returns the channel to hand to the supervisor plus a wait
// function.
//
// The caller MUST call wait exactly once, normally with defer, after the
// supervisor call returns: it closes the channel and blocks until every queued
// event has been printed. Sending on the channel after wait returns panics, so
// the supervisor must not retain it.
func (a *app) pumpEvents() (ev chan supervisor.Event, wait func()) {
	ch := make(chan supervisor.Event, 64)
	done := make(chan struct{})
	r := a.renderer()
	go func() {
		defer close(done)
		for e := range ch {
			if line := r.Event(e); line != "" {
				fmt.Fprintln(a.env.Stdout, line)
			}
		}
	}()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			close(ch)
			<-done
		})
	}
}

// printStatus writes a status block to stdout, or nothing when there is nothing
// to show.
func (a *app) printStatus(sts []supervisor.Status) {
	if block := a.renderer().StatusBlock(sts); block != "" {
		fmt.Fprintln(a.env.Stdout, block)
	}
}

// filterStatus keeps only the statuses of names, in the order the supervisor
// returned them. An empty names keeps everything.
func filterStatus(sts []supervisor.Status, names []string) []supervisor.Status {
	if len(names) == 0 {
		return sts
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	out := make([]supervisor.Status, 0, len(names))
	for _, st := range sts {
		if want[st.Name] {
			out = append(out, st)
		}
	}
	return out
}

// notReady names every status that failed to become ready: a process that died
// while starting, one that is alive but still not answering, one that has given
// up any claim to be still starting, and one that came up and then died. All of
// them are exit code 4 — the question `mabo-ctl start` answers is "can I use this
// stack now?", and "not yet" is not success in any of its four spellings.
func notReady(sts []supervisor.Status) []string {
	var bad []string
	for _, st := range sts {
		switch st.Phase {
		case supervisor.PhaseFailed, supervisor.PhaseSlow,
			supervisor.PhaseDegraded, supervisor.PhaseExited:
			bad = append(bad, st.Name)
		}
	}
	return bad
}

// namedPortsFlag is the --port <service>=<number> flag, repeatable, so an
// override can name what it means instead of counting empty slots.
type namedPortsFlag struct {
	values map[string]int
	raw    []string
}

// String returns the flag as last written, for help output.
func (p *namedPortsFlag) String() string { return strings.Join(p.raw, ", ") }

// Type names the flag's value in help output.
func (p *namedPortsFlag) Type() string { return "SERVICE=PORT" }

// Set parses one occurrence. Occurrences ACCUMULATE — that is the point of
// naming services — and a service named twice is rejected rather than resolved
// by order of appearance, because two overrides for one port is a typo either
// way.
func (p *namedPortsFlag) Set(s string) error {
	name, num, ok := strings.Cut(s, "=")
	name = strings.TrimSpace(name)
	num = strings.TrimSpace(num)
	if !ok || name == "" || num == "" {
		return fmt.Errorf("--port must be SERVICE=PORT, got %q", s)
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("--port %s: %q is not a port; use a number in 1..65535", name, num)
	}
	if p.values == nil {
		p.values = make(map[string]int)
	}
	if _, dup := p.values[name]; dup {
		return fmt.Errorf("--port names %q more than once", name)
	}
	p.values[name] = n
	p.raw = append(p.raw, s)
	return nil
}

// portsFlag is the --ports=A,B,C,D flag: positional port overrides, one slot
// per service that declares a port, in declaration order.
type portsFlag struct {
	values []int
	raw    string
}

// String returns the flag as the user wrote it, which is what cobra shows as
// the current value.
func (p *portsFlag) String() string { return p.raw }

// Type names the flag's value in help output.
func (p *portsFlag) Type() string { return "A,B,C,D" }

// Set parses a --ports value. The last occurrence wins; the flag does not
// accumulate, because the slots are positional and merging two lists of
// positions would be ambiguous.
func (p *portsFlag) Set(s string) error {
	values, err := parsePorts(s)
	if err != nil {
		return err
	}
	p.values, p.raw = values, s
	return nil
}

// parsePorts parses the positional --ports list.
//
// Slot i applies to the i-th service that DECLARES a port, in declaration
// order. An EMPTY SLOT keeps that service's declared default, so
// `--ports=,,7999` overrides only the third ported service. A literal 0 and a
// "-" mean the same thing as an empty slot. Trailing empty slots are dropped,
// so `--ports=,,7999,` addresses three services rather than four and does not
// trip the "more values than services" check.
//
// It returns an error naming the offending slot when a value is not a decimal
// number or falls outside 1..65535. An empty or all-empty list returns nil,
// meaning "no overrides at all".
func parsePorts(s string) ([]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	fields := strings.Split(s, ",")
	out := make([]int, 0, len(fields))
	for i, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" || f == "-" || f == "0" {
			out = append(out, 0)
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf(
				"--ports slot %d: %q is not a number; give a port in 1..65535, or leave the slot empty to keep the declared default", i+1, f)
		}
		if n < 1 || n > 65535 {
			return nil, fmt.Errorf("--ports slot %d: port %d is out of range; a port must be in 1..65535", i+1, n)
		}
		out = append(out, n)
	}
	for len(out) > 0 && out[len(out)-1] == 0 {
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
