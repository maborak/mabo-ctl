package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/maborak/mabo-ctl/internal/web"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// This file is the machine-readable self-description of mabo-ctl:
// `mabo-ctl schema --commands`. It exists because an agent integrating with this
// tool has to learn three things that prose help does not answer in a form a
// program can branch on: what every command and flag is, which exit codes mean
// what, and what the web console's HTTP surface looks like.
//
// The catalogue is GENERATED, not transcribed. Command names, summaries, usage
// lines and flags come from the live cobra tree — the same tree `--help` renders
// — so a flag added next release cannot be missing here without a test failing.
// What is hand-curated is per-command metadata an argument parser cannot derive
// (does it mutate state? what are its notable exits?), held in [commandMetas];
// buildCatalog REFUSES to emit a command lacking metadata, so adding a subcommand
// without documenting it breaks `schema --commands` loudly instead of quietly
// shipping an undocumented command.

// catalogMeta is the hand-curated part of one command's documentation — the part
// that cannot be derived from the cobra tree.
type catalogMeta struct {
	// Args describes the positional arguments beyond the usage line.
	Args string
	// Mutates reports whether the command changes supervised state or the
	// filesystem: spawning processes, writing .dev/, editing files. Read-only
	// commands set false even when they run probes.
	Mutates bool
	// SideEffects lists observable effects a wrapper or sandbox may care about,
	// including the read-only ones (probes, listeners).
	SideEffects []string
	// ExitCodes notes exits beyond the global table; empty means the global
	// table says it all.
	ExitCodes string
}

// commandMetas keys [catalogMeta] by command name, "mabo-ctl" naming the root.
var commandMetas = map[string]catalogMeta{
	"mabo-ctl": {
		Args:        "one or more service names; each name is shorthand for start <name>",
		Mutates:     true,
		SideEffects: []string{"with no arguments on a terminal: opens the interactive console"},
	},
	"start": {
		Args:        "zero or more service names; none means every service with autostart enabled, --all forces every declared one",
		Mutates:     true,
		SideEffects: []string{"spawns service processes", "writes .dev/ logs, pid files and exit records", "captures AND unsets caller <NAME>_PORT variables before spawning"},
		ExitCodes:   "adds 4 when any selected service did not become ready inside ready_timeout",
	},
	"stop": {
		Args:        "zero or more service names; none means all",
		Mutates:     true,
		SideEffects: []string{"signals each named process GROUP: SIGTERM, grace period, then SIGKILL", "writes .dev/ exit records marked deliberate"},
	},
	"restart": {
		Args:        "zero or more service names; none means all",
		Mutates:     true,
		SideEffects: []string{"stops then starts the named services"},
		ExitCodes:   "adds 4 when a restarted service did not become ready inside ready_timeout",
	},
	"status": {
		Args: "zero or more service names, filtering the report",
	},
	"health": {
		Args:        "zero or more service names; none probes everything with a health URL",
		SideEffects: []string{"dials each declared health URL / TCP address in parallel"},
	},
	"config": {},
	"logs": {
		Args:        "<service> [n]; shows the last n lines, -f follows until interrupted",
		SideEffects: []string{"reads .dev/logs/ only"},
	},
	"reset": {
		Args:        "--force reaps whatever still holds a declared port",
		Mutates:     true,
		SideEffects: []string{"stops every service", "deletes the whole .dev/ directory", "may kill unknown processes holding declared ports"},
	},
	"preflight": {
		Mutates:     true,
		SideEffects: []string{"runs the commands and dials the TCP endpoints declared under checks:"},
		ExitCodes:   "1 also when any declared check fails",
	},
	"exec": {
		Args:        "<service> -- <argv...>; runs argv inside the service's resolved environment and working directory",
		Mutates:     true,
		SideEffects: []string{"runs an arbitrary command as a child"},
		ExitCodes:   "forwards the CHILD's exit code verbatim instead of any of the global codes",
	},
	"shell": {
		Args:        "[service]; opens a shell inside a service's resolved environment and directory",
		Mutates:     true,
		SideEffects: []string{"runs an interactive shell as a child"},
	},
	"open": {
		SideEffects: []string{"hands every running service's URL to the default browser"},
	},
	"serve": {
		Mutates:     true,
		SideEffects: []string{"binds a loopback HTTP listener for the web console", "prints its URL with its session token to stdout"},
	},
	"repl": {
		Mutates:     true,
		SideEffects: []string{"interactive prompt dispatching other commands, with all of their effects"},
	},
	"completion": {
		Args: "[bash|zsh|fish]",
	},
	"upgrade": {
		Mutates:     true,
		SideEffects: []string{"fetches the latest published release over the network", "replaces this binary on disk after verification"},
	},
	"schema": {},
	"doctor": {
		SideEffects: []string{"inspects runtimes, pid files, port holders and .dev/ permissions; read-only plus lsof"},
	},
	"init": {
		Mutates:     true,
		SideEffects: []string{"writes a commented-out mabo-ctl.yaml scaffold into the current directory"},
	},
	"help": {
		Args: "[command]",
	},
}

// catalogFlag documents one flag as programs see it.
type catalogFlag struct {
	Name    string `json:"name"`
	Short   string `json:"short,omitempty"`
	Type    string `json:"type"`
	Default string `json:"default"`
	Desc    string `json:"desc"`
}

// catalogCommand is one entry of the commands array.
type catalogCommand struct {
	Name        string        `json:"name"`
	Summary     string        `json:"summary"`
	Usage       string        `json:"usage"`
	Args        string        `json:"args,omitempty"`
	Mutates     bool          `json:"mutates"`
	SideEffects []string      `json:"side_effects,omitempty"`
	ExitCodes   string        `json:"exit_codes,omitempty"`
	Flags       []catalogFlag `json:"flags"`
	Inherited   []catalogFlag `json:"inherited_flags,omitempty"`
}

// codeEntry keeps exit-code semantics ordered, which map[string]string cannot.
type codeEntry struct {
	Code    string `json:"code"`
	Meaning string `json:"meaning"`
}

// contractEntry is one machine-consumable stdout surface.
type contractEntry struct {
	Command string `json:"command"`
	Output  string `json:"output"`
	Stable  bool   `json:"stable"`
}

// httpRouteDoc mirrors one web.Route for agents.
type httpRouteDoc struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Auth   string `json:"auth"`
	Desc   string `json:"desc"`
	Stream bool   `json:"stream,omitempty"`
}

// bareDoc records what the binary DOES with no subcommand at all — the first
// thing an integrator tries and the easiest thing to document wrongly.
type bareDoc struct {
	Terminal string `json:"terminal"`
	Piped    string `json:"piped"`
	WithName string `json:"service_name_argument"`
}

// catalogBehavior groups non-per-command facts agents need.
type catalogBehavior struct {
	ConfigDiscovery string   `json:"config_discovery"`
	StateDirectory  string   `json:"state_directory"`
	PortPrecedence  []string `json:"port_precedence"`
	BareInvocation  bareDoc  `json:"bare_invocation"`
}

// httpAPIDoc describes the web console's HTTP surface.
type httpAPIDoc struct {
	Base   string         `json:"base"`
	Rules  []string       `json:"rules"`
	Routes []httpRouteDoc `json:"routes"`
}

// catalogDoc is the whole stdout document of `schema --commands`.
type catalogDoc struct {
	Tool             string           `json:"tool"`
	Release          string           `json:"release"`
	Commit           string           `json:"commit"`
	StabilityNote    string           `json:"stability_note"`
	Behavior         catalogBehavior  `json:"behavior"`
	ExitCodes        []codeEntry      `json:"exit_codes"`
	MachineContracts []contractEntry  `json:"machine_contracts"`
	HTTPAPI          httpAPIDoc       `json:"http_api"`
	Commands         []catalogCommand `json:"commands"`
}

// routeAuth translates a guarding level into what a client must actually send.
func routeAuth(k web.RouteKind) string {
	switch k {
	case web.RouteIndex:
		return "none: page asks for the token itself"
	case web.RouteRead:
		return "session required: X-Mabo-Ctl-Token header, ?token=, or the cookie minted by opening the printed URL"
	case web.RouteMutate:
		return "X-Mabo-Ctl-Token header required; POST-only"
	default:
		return "unknown guard"
	}
}

// flagsOf renders a pflag set into catalogFlag slice order, skipping hidden
// flags.
func flagsOf(set *pflag.FlagSet) []catalogFlag {
	if set == nil || !set.HasFlags() {
		return nil
	}
	out := make([]catalogFlag, 0, 8)
	set.VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		out = append(out, catalogFlag{
			Name:    f.Name,
			Short:   f.Shorthand,
			Type:    f.Value.Type(),
			Default: f.DefValue,
			Desc:    f.Usage,
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) == 0 {
		return nil
	}
	return out
}

// localFlags returns the flags a command defines itself, its own persistent ones
// included — what --help prints as "Flags".
func localFlags(c *cobra.Command) *pflag.FlagSet { return c.LocalFlags() }

// inheritedFlags returns the persistent flags visible from parent commands —
// what --help prints under each subcommand's own set.
func inheritedFlags(c *cobra.Command) *pflag.FlagSet {
	if c.Parent() == nil {
		return nil
	}
	return c.InheritedFlags()
}

// buildCatalog walks root's live command tree and returns the indented JSON
// catalogue. It errors when a visible command carries no [commandMetas] entry:
// an undocumented command must fail HERE, where someone adding it will notice,
// not downstream where an integrator finds a hole.
func buildCatalog(root *cobra.Command) ([]byte, error) {
	var missing []string
	doc := &catalogDoc{
		Tool:    root.Name(),
		Release: theBuild().Version,
		Commit:  theBuild().Commit,
		StabilityNote: "the shape of THIS document is provisional across releases; " +
			"`mabo-ctl status --json` and the exit-code table are the stable parts",
		Behavior: catalogBehavior{
			ConfigDiscovery: "walks UP from the working directory, bounded by the repository marker or $HOME; " +
				"--config skips the search; devctl.yaml is still accepted under its legacy spelling",
			StateDirectory: ".dev/ under the config root: logs, pid files, run.env persisted ports, exit records",
			PortPrecedence: []string{
				"1. --ports A,B,C,D positional slots, or repeatable --port SERVICE=PORT",
				"2. caller environment <NAME>_PORT, captured AND unset before anything spawns",
				"3. .dev/run.env, persisted from the previous run",
				"4. the default declared in mabo-ctl.yaml",
			},
			BareInvocation: bareDoc{
				Terminal: "opens the full-screen console",
				Piped:    "prints the human status block (NOT the JSON contract; use status --json)",
				WithName: "starts that service, as if `start` had been typed",
			},
		},
		ExitCodes: []codeEntry{
			{Code: "0", Meaning: "success"},
			{Code: "1", Meaning: "a runtime failure"},
			{Code: "2", Meaning: "a usage error, such as an unknown service or an unknown flag"},
			{Code: "3", Meaning: "mabo-ctl.yaml is missing, unreadable or invalid"},
			{Code: "4", Meaning: "a service failed to become ready inside ready_timeout"},
			{Code: "exec", Meaning: "`exec` forwards the child's exit code verbatim instead"},
		},
		MachineContracts: []contractEntry{
			{Command: "status --json", Output: "one status object per service; THE stable machine contract", Stable: true},
			{Command: "config --json", Output: "the resolved configuration view as JSON", Stable: false},
			{Command: "config --raw", Output: "mabo-ctl.yaml byte for byte, unredacted", Stable: false},
			{Command: "schema", Output: "draft-07 JSON Schema describing mabo-ctl.yaml", Stable: true},
			{Command: "schema --commands", Output: "this catalogue", Stable: false},
			{Command: "GET /api/status", Output: "same JSON as status --json, over HTTP from `serve`", Stable: true},
		},
	}
	doc.HTTPAPI = httpAPIDoc{
		Base: "http://127.0.0.1:<port>, the URL `serve` prints; the URL carries ?token=…",
		Rules: []string{
			"every mutation is POST-only; a wrong verb answers 405",
			"host and Origin are validated; binds loopback only unless --i-know-this-is-dangerous",
			"SSE routes stream events; consume incrementally or disconnect",
		},
	}
	for _, r := range web.Routes() {
		doc.HTTPAPI.Routes = append(doc.HTTPAPI.Routes, httpRouteDoc{
			Method: r.Method,
			Path:   r.Path,
			Auth:   routeAuth(r.Kind),
			Desc:   r.Desc,
			Stream: r.Stream,
		})
	}

	commands := []*cobra.Command{root}
	for _, c := range root.Commands() {
		if !c.IsAvailableCommand() {
			continue
		}
		commands = append(commands, c)
	}
	doc.Commands = make([]catalogCommand, 0, len(commands))
	for _, c := range commands {
		meta, ok := commandMetas[c.Name()]
		if !ok {
			missing = append(missing, c.Name())
			continue
		}
		entry := catalogCommand{
			Name:        c.Name(),
			Summary:     c.Short,
			Usage:       strings.TrimSpace(c.UseLine()),
			Args:        meta.Args,
			Mutates:     meta.Mutates,
			SideEffects: meta.SideEffects,
			ExitCodes:   meta.ExitCodes,
			Flags:       flagsOf(localFlags(c)),
			Inherited:   flagsOf(inheritedFlags(c)),
		}
		doc.Commands = append(doc.Commands, entry)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("mabo-ctl: schema --commands refuses undocumented command(s) %s — "+
			"add them to commandMetas in cmd/mabo-ctl/catalog.go", strings.Join(missing, ", "))
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("mabo-ctl: encode catalogue: %w", err)
	}
	return b, nil
}
