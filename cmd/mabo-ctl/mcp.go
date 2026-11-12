package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/maborak/mabo-ctl/internal/mcp"
	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/ui"
)

// mcpMaxTail caps the logs tool, the same cap the web console's ?tail= has.
const mcpMaxTail = 5000

// mcpCmd builds `mabo-ctl mcp`.
func (a *app) mcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve this repo's services to an AI assistant over MCP (stdio)",
		Long: `Mcp runs a Model Context Protocol server on stdin/stdout for an AI assistant
(Claude Code, Claude Desktop, any MCP client) that spawns it. It is not a
network listener: the client owns both pipes, and the server ends with them.

Read-only tools are always offered:
  status   the status --json records (health URLs redacted)
  health   the same, with which declared probes are not answering
  logs     the last lines of one service's log, or of all of them
  config   the resolved configuration and start order (redacted)

--allow-mutate also offers start, stop and restart. Without it an assistant
can look but not touch.

  claude mcp add mabo-ctl -- mabo-ctl mcp                  # read-only
  claude mcp add mabo-ctl -- mabo-ctl mcp --allow-mutate   # can start/stop

The configuration is read once, when the server starts; restart the server
after editing mabo-ctl.yaml.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          a.runMCP,
	}
	cmd.Flags().Bool("allow-mutate", false, "also offer start, stop and restart tools")
	return cmd
}

// runMCP serves until the client closes stdin or the process is signalled.
func (a *app) runMCP(cmd *cobra.Command, _ []string) error {
	// stdout is the protocol channel: nothing may prompt on stdin or print to
	// stdout except the server. Port notices already go to stderr.
	a.jsonContract = true
	sup, insts, err := a.supervisor()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// A client that closes its end of stdout must not kill this process with
	// SIGPIPE mid-start: asking for the signal turns the write into an EPIPE
	// error, which Serve treats as the client being gone.
	sigpipe := make(chan os.Signal, 1)
	signal.Notify(sigpipe, syscall.SIGPIPE)
	defer signal.Stop(sigpipe)

	srv := &mcp.Server{
		Name:    "mabo-ctl",
		Version: theBuild().Summary(),
		Instructions: "mabo-ctl supervises this repository's local dev services (mabo-ctl.yaml). " +
			"Use status to see what runs, logs to read a service's output, health to find what is not answering.",
		Tools: a.mcpTools(sup, insts, boolFlag(cmd, "allow-mutate")),
	}
	fmt.Fprintf(a.env.Stderr, "mabo-ctl mcp: serving %d tools on stdio for %s\n", len(srv.Tools), a.cfg.Path)
	stdin := a.env.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	return srv.Serve(ctx, stdin, a.env.Stdout)
}

// mcpTools builds the tool set. Every tool reuses the code path the CLI uses
// for the same answer, and every status passes through ui.RedactStatuses: this
// server answers whatever process spawned it, so it withholds what the web
// console withholds.
func (a *app) mcpTools(sup lifecycle, insts []service.Instance, mutate bool) []mcp.Tool {
	names := make([]string, 0, len(insts))
	for _, in := range insts {
		names = append(names, in.Name)
	}
	servicesArg := map[string]any{
		"type":        "array",
		"items":       map[string]any{"type": "string", "enum": names},
		"description": "services to act on; empty or omitted means every service",
	}
	object := func(props map[string]any, required ...string) map[string]any {
		s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	statusJSON := func(ctx context.Context, want []string) (string, error) {
		sts := filterStatus(sup.Status(ctx), want)
		ui.RedactStatuses(sts)
		b, err := ui.StatusJSON(sts)
		return string(b), err
	}

	tools := []mcp.Tool{
		{
			Name:        "status",
			Description: "Every service's phase (stopped, running, ready, slow, degraded, failed, exited), pid, port, uptime, CPU, memory, last exit and detail, as the status --json records. Runs the readiness probes.",
			InputSchema: object(map[string]any{"services": servicesArg}),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct{ Services []string }
				if err := decodeArgs(raw, &args); err != nil {
					return "", err
				}
				if err := knownServices(args.Services, names); err != nil {
					return "", err
				}
				return statusJSON(ctx, args.Services)
			},
		},
		{
			Name:        "health",
			Description: "Whether every declared readiness probe answers, and which services are down, followed by their status records.",
			InputSchema: object(map[string]any{}),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				if err := decodeArgs(raw, &struct{}{}); err != nil {
					return "", err
				}
				sts := healthStatus(sup.Status(ctx))
				ui.RedactStatuses(sts)
				var down []string
				for _, st := range sts {
					if st.Health != "" && st.Phase != supervisor.PhaseReady {
						down = append(down, st.Name+" ("+string(st.Phase)+")")
					}
				}
				head := "every declared probe answers"
				if len(down) > 0 {
					head = "not answering: " + strings.Join(down, ", ")
				}
				b, err := ui.StatusJSON(sts)
				return head + "\n" + string(b), err
			},
		},
		{
			Name: "logs",
			Description: "The last lines of a service's log (.mabo-ctl/logs/<service>.log), or of every service with \"all\". " +
				"Lines over 256 KiB are cut and marked; at most 5000 lines and 8 MiB per service.",
			InputSchema: object(map[string]any{
				"service": map[string]any{"type": "string", "enum": append([]string{"all"}, names...)},
				"tail":    map[string]any{"type": "integer", "minimum": 1, "maximum": mcpMaxTail, "default": 100},
				"grep":    map[string]any{"type": "string", "description": "keep only lines containing this substring"},
			}, "service"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Service string
					Tail    int
					Grep    string
				}
				if err := decodeArgs(raw, &args); err != nil {
					return "", err
				}
				return mcpLogs(ctx, sup, names, args.Service, args.Tail, args.Grep)
			},
		},
		{
			Name:        "config",
			Description: "The loaded mabo-ctl.yaml as resolved: config path, each service's port and where it came from, command, directory, runtime, health URL, environment (credentials redacted) and the start order.",
			InputSchema: object(map[string]any{}),
			Call: func(_ context.Context, raw json.RawMessage) (string, error) {
				if err := decodeArgs(raw, &struct{}{}); err != nil {
					return "", err
				}
				b, err := ui.ConfigJSON(ui.BuildConfigView(ui.ConfigInput{
					Config: a.cfg, Instances: insts, Origins: a.origins,
					StateDir: a.stateDir(), Explicit: a.configPath != "",
				}))
				return string(b), err
			},
		},
	}
	if !mutate {
		return tools
	}

	op := func(name, verb, desc string, run func(context.Context, []string, chan<- supervisor.Event) error) mcp.Tool {
		return mcp.Tool{
			Name:        name,
			Description: desc,
			InputSchema: object(map[string]any{"services": servicesArg}),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct{ Services []string }
				if err := decodeArgs(raw, &args); err != nil {
					return "", err
				}
				if err := knownServices(args.Services, names); err != nil {
					return "", err
				}
				if verb != "stop" {
					// As `mabo-ctl start` does: the next invocation, from any
					// terminal, resolves the same ports.
					if err := service.Persist(a.st, insts); err != nil {
						return "", err
					}
				}
				var log strings.Builder
				ev := make(chan supervisor.Event, 256)
				done := make(chan struct{})
				r := &ui.Renderer{}
				go func() {
					defer close(done)
					for e := range ev {
						log.WriteString(r.Event(e) + "\n")
					}
				}()
				err := func() error {
					defer close(ev) // even if run panics, so the collector ends
					return run(ctx, args.Services, ev)
				}()
				<-done
				final, serr := statusJSON(context.WithoutCancel(ctx), args.Services)
				if serr != nil {
					return log.String(), serr
				}
				return log.String() + final, err
			},
		}
	}
	return append(tools,
		op("start", "start", "Start services (dependencies first) and wait for readiness; empty starts the autostart selection. Returns the events and the final status records.", sup.Start),
		op("stop", "stop", "Stop services: SIGTERM to the process group, then SIGKILL after stop_grace; empty stops every service. Dependencies are not stopped.", sup.Stop),
		op("restart", "restart", "Stop then start services; empty restarts every service.", sup.Restart),
	)
}

// mcpLogs collects a non-following tail of one service, or of all of them.
func mcpLogs(ctx context.Context, sup lifecycle, names []string, svc string, n int, grep string) (string, error) {
	if n <= 0 {
		n = 100
	}
	n = min(n, mcpMaxTail)
	targets := []string{svc}
	if svc == "all" {
		targets = names
	} else if err := knownServices(targets, names); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var b strings.Builder
	var errs []error
	for _, name := range targets {
		lines := make(chan string, 16)
		errc := make(chan error, 1)
		go func() { errc <- sup.Tail(ctx, name, n, false, lines) }()
		for line := range lines {
			text := ui.Sanitize(line)
			if grep != "" && !strings.Contains(text, grep) {
				continue
			}
			if len(targets) > 1 {
				b.WriteString(name + " | ")
			}
			b.WriteString(text + "\n")
		}
		if err := <-errc; err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	if b.Len() == 0 && len(errs) == 0 {
		b.WriteString("(no log output)")
	}
	return b.String(), errors.Join(errs...)
}

// decodeArgs strictly decodes a tool's arguments: an unknown or misspelled
// argument is an error the model sees, not something silently ignored.
func decodeArgs(raw json.RawMessage, into any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return mcp.Usagef("bad arguments: %v", err)
	}
	return nil
}

// knownServices refuses a name that is not declared, listing the ones that are.
func knownServices(want, names []string) error {
	for _, w := range want {
		found := false
		for _, n := range names {
			if n == w {
				found = true
			}
		}
		if !found {
			return mcp.Usagef("unknown service %q; declared services are: %s", w, strings.Join(names, ", "))
		}
	}
	return nil
}
