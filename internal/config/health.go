package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Probe kinds. The empty kind means "no readiness check", which is how a
// Spec carries the absence of a health entry.
const (
	// HealthNone means no readiness probe is declared.
	HealthNone = ""
	// HealthHTTP probes an absolute http(s) URL; any response is ready.
	HealthHTTP = "http"
	// HealthTCP dials host:port; a connected socket is ready.
	HealthTCP = "tcp"
	// HealthExec runs an argv vector; exit 0 is ready.
	HealthExec = "exec"
)

// Health declares how a service's readiness is probed.
//
// It accepts two spellings on disk, and both decode into this one shape:
//
//	health: http://localhost:{{.Port}}/healthz   # scalar == http
//	health: {tcp: localhost:5432}
//	health: {exec: [pg_isready, -h, localhost]}
//
// The parts hold RAW templates exactly like Cmd and Env do; they are expanded
// later by internal/service once every port is known. A scalar value is
// shorthand for kind http so that every config written before non-HTTP probes
// existed keeps parsing byte for byte.
type Health struct {
	// Kind selects the probe family: HealthHTTP, HealthTCP or HealthExec.
	Kind string
	// HTTP is the probed URL when Kind is HealthHTTP.
	HTTP string
	// Addr is "host:port" when Kind is HealthTCP.
	Addr string
	// Argv is the command to run when Kind is HealthExec.
	Argv []string
}

// Zero reports whether h declares no probe at all. A zero Health is the normal
// way a service says "there is nothing to ask about my readiness".
func (h Health) Zero() bool {
	return h.Kind == HealthNone && h.HTTP == "" && h.Addr == "" && len(h.Argv) == 0
}

// Raw returns every template-bearing part joined, in declaration order, so
// validation can apply text-level rules (template syntax, own-port references)
// without knowing which part they came from.
func (h Health) Raw() string {
	parts := make([]string, 0, 1+len(h.Argv))
	if h.HTTP != "" {
		parts = append(parts, h.HTTP)
	}
	if h.Addr != "" {
		parts = append(parts, h.Addr)
	}
	parts = append(parts, h.Argv...)
	return strings.Join(parts, " ")
}

// String renders h the way status output quotes it back. It is the RAW form,
// not the expanded one: Spec renders what was declared.
func (h Health) String() string {
	switch h.Kind {
	case HealthTCP:
		return "tcp:" + h.Addr
	case HealthExec:
		return "exec: [" + strings.Join(h.Argv, ", ") + "]"
	case HealthHTTP:
		return h.HTTP
	default:
		return ""
	}
}

// UnmarshalYAML decodes either spelling and rejects anything else: a mapping
// must carry exactly one of http/tcp/exec, unknown keys are errors (the same
// strictness the rest of mabo-ctl.yaml gets), and a sequence or nested mapping
// is not a health declaration at all.
func (h *Health) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == "!!null" || strings.TrimSpace(node.Value) == "" {
			*h = Health{}
			return nil
		}
		*h = Health{Kind: HealthHTTP, HTTP: node.Value}
		return nil

	case yaml.MappingNode:
		return h.unmarshalMapping(node)

	default:
		return fmt.Errorf("line %d: health must be a URL or a mapping with exactly one of "+
			"http/tcp/exec, got %s", node.Line, nodeKind(node.Kind))
	}
}

// unmarshalMapping walks the mapping by hand rather than through node.Decode,
// because KnownFields strictness does not reach a sub-decode and a misspelled
// key inside health must be a load-time error, not silence.
func (h *Health) unmarshalMapping(node *yaml.Node) error {
	*h = Health{}
	const want = `health must set exactly one of http/tcp/exec`
	kinds := 0

	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		taken := func(field string) bool {
			switch field {
			case "http":
				return h.HTTP != ""
			case "tcp":
				return h.Addr != ""
			case "exec":
				return len(h.Argv) > 0
			}
			return false
		}
		set := func() { kinds++ }

		switch key.Value {
		case "http", "tcp", "exec":
		default:
			return fmt.Errorf("line %d: unknown health key %q; %s", key.Line, key.Value, want)
		}
		if taken(key.Value) {
			return fmt.Errorf("line %d: health sets %q more than once; %s", key.Line, key.Value, want)
		}
		if err := checkProbeValue(key, val); err != nil {
			return err
		}
		switch key.Value {
		case "http":
			h.Kind, h.HTTP = HealthHTTP, val.Value
		case "tcp":
			h.Kind, h.Addr = HealthTCP, val.Value
		case "exec":
			h.Kind = HealthExec
			for _, arg := range val.Content {
				h.Argv = append(h.Argv, arg.Value)
			}
		}
		set()
	}

	if kinds > 1 {
		return fmt.Errorf("line %d: %s — got %d of them", node.Line, want, kinds)
	}
	if h.Zero() {
		return fmt.Errorf("line %d: %s — a health mapping that names none of them checks nothing", node.Line, want)
	}
	return nil
}

// checkProbeValue holds each health value to its kind's shape before it is
// stored, so `tcp: [a, b]` or `exec: notalist` fails here instead of puzzling
// whoever reads the probe output later.
func checkProbeValue(key, val *yaml.Node) error {
	switch key.Value {
	case "http", "tcp":
		if val.Kind != yaml.ScalarNode || val.Tag == "!!null" || strings.TrimSpace(val.Value) == "" {
			return fmt.Errorf("line %d: health %s must be a non-empty string", val.Line, key.Value)
		}
		return nil
	default: // exec
		if val.Kind != yaml.SequenceNode {
			return fmt.Errorf("line %d: health exec must be an argv list such as [pg_isready, -h, localhost]", val.Line)
		}
		for _, arg := range val.Content {
			if arg.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: health exec entries must be scalars, got %s", arg.Line, nodeKind(arg.Kind))
			}
		}
		if len(val.Content) == 0 {
			return fmt.Errorf("line %d: health exec is empty; declare the command to run, e.g. [pg_isready, -h, localhost]", val.Line)
		}
		return nil
	}
}
