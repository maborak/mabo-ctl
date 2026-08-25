package main

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/state"
)

// reconcilePorts resolves the one drift the precedence chain cannot: the yaml
// changed a declared port and .dev/run.env still holds the old one. The chain
// itself is fixed — persisted state deliberately outranks the declared default,
// because ports must stay stable across invocations — so the remedy is consent,
// not reordering. The operator is asked whether to adopt the declared ports for
// this repository; on yes the resolution is redone without the run.env level
// and the file is rewritten, so every later invocation and the web console
// agree with what was adopted.
//
// `--refresh-ports` is the same adoption without the question, for scripts and
// for an operator who has already answered once and wants yaml to win from now
// on.
//
// It returns the instances and origins to use, which differ from its inputs
// only when adoption happened. The override notice is printed here whenever the
// drift survives the conversation, so callers never print it themselves.
func (a *app) reconcilePorts(cfg *config.Config, st *state.Dir, insts []service.Instance, origins []service.Origin) ([]service.Instance, []service.Origin, error) {
	notice := a.renderer().PortOrigins(origins)
	if notice != "" {
		fmt.Fprintln(a.env.Stderr, notice)
	}
	if a.refreshPorts {
		return a.adoptDeclaredPorts(cfg, st, nil)
	}
	if notice == "" || !a.canPromptPorts() {
		return insts, origins, nil
	}

	fmt.Fprint(a.env.Stderr, "adopt the declared ports (rewrites .dev/run.env)? [y/N] ")
	answer, err := bufio.NewReader(a.env.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(answer) == "" {
		// EOF or a read failure is a "no": keeping the persisted ports is the
		// conservative answer, and the notice above already said how to get the
		// new ones.
		return insts, origins, nil
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return a.adoptDeclaredPorts(cfg, st, origins)
	default:
		return insts, origins, nil
	}
}

// canPromptPorts reports whether asking the operator a question is possible: a
// terminal on both ends, no --json contract on this command, and not the
// interactive prompt, which owns stdin and dispatches back into this same
// command tree.
func (a *app) canPromptPorts() bool {
	if a.jsonContract || a.inREPL {
		return false
	}
	return a.env.IsTTY != nil && a.env.IsTTY()
}

// adoptDeclaredPorts re-resolves without the run.env level and persists the
// result, so the adoption outlives this invocation. prev carries the origins
// the drift was seen in, when it was seen, so the confirmation can name every
// port that moved; nil means the caller arrived by flag, and the old values are
// read from .dev/run.env instead.
func (a *app) adoptDeclaredPorts(cfg *config.Config, st *state.Dir, prev []service.Origin) ([]service.Instance, []service.Origin, error) {
	if prev == nil {
		if re, err := st.ReadRunEnv(); err == nil {
			prev = originsFromRunEnv(cfg, re)
		}
	}

	adopted, origins, err := service.Resolve(cfg, st, service.Options{
		Ports:        a.ports.values,
		EnvVars:      a.captured,
		IgnoreRunEnv: true,
	})
	if err != nil {
		return nil, nil, err
	}
	if err := service.Persist(st, adopted); err != nil {
		return nil, nil, fmt.Errorf("mabo-ctl: persist adopted ports: %w", err)
	}

	// Name what moved, or say nothing when nothing did: a flag run against an
	// already-agreeing repository must not manufacture an announcement.
	var moved []string
	for _, fresh := range origins {
		old, ok := originForName(prev, fresh.Service)
		if !ok || old.Port == fresh.Port || old.Port == 0 {
			continue
		}
		moved = append(moved, fmt.Sprintf("%s %d → %d", fresh.Service, old.Port, fresh.Port))
	}
	if len(moved) > 0 {
		fmt.Fprintf(a.env.Stderr, "adopted declared ports, .dev/run.env updated: %s\n", strings.Join(moved, ", "))
	}
	return adopted, origins, nil
}

// originsFromRunEnv reconstructs the previous origins well enough to describe a
// change: one entry per service run.env names, carrying the persisted port.
func originsFromRunEnv(cfg *config.Config, re *state.RunEnv) []service.Origin {
	if re == nil {
		return nil
	}
	var out []service.Origin
	for _, s := range cfg.Services {
		if p, ok := re.Port(s.Name); ok {
			out = append(out, service.Origin{Service: s.Name, Port: p, Declared: s.Port})
		}
	}
	return out
}

// originForName returns the origin of the named service.
func originForName(origins []service.Origin, name string) (service.Origin, bool) {
	for _, o := range origins {
		if o.Service == name {
			return o, true
		}
	}
	return service.Origin{}, false
}
