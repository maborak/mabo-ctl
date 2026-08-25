package config

import "encoding/json"

// Schema returns the JSON Schema (draft-07) describing a mabo-ctl.yaml, as
// pretty-printed JSON with no trailing newline.
//
// It is hand-mapped from the same structs Load decodes — fileDoc, specDoc,
// Check, Shell — because those structs are the only truth about what the
// parser accepts. A hand-written schema can drift from them, so the test in
// this package pins the two together: every key the shipped example uses must
// appear in the schema, and a minimal document must satisfy its required
// lists. When you add a field to specDoc or fileDoc, add it here in the same
// commit; the example-driven test will remind you.
//
// The schema is deliberately LOOSE where the parser is lenient (a duration is
// a string or a number; a colour is any string the terminal can interpret) and
// strict where the parser is strict (additionalProperties: false everywhere
// KnownFields(true) rejects a key).
func Schema() ([]byte, error) {
	// The schema is a literal so it reads as documentation. json.Marshal
	// indents it; nothing here is computed, so nothing here can drift at
	// runtime.
	s := map[string]any{
		"$schema":              "http://json-schema.org/draft-07/schema#",
		"$id":                  "mabo-ctl.schema.json",
		"title":                "mabo-ctl.yaml",
		"type":                 "object",
		"required":             []string{"services"},
		"additionalProperties": false,
		"properties": map[string]any{
			"$schema":       stringSchema("A JSON Schema reference for editors; mabo-ctl ignores it."),
			"stop_grace":    durationSchema("How long Stop waits after SIGTERM before SIGKILL. Default 10s."),
			"ready_timeout": durationSchema("How long a readiness probe polls before a service is slow, then degraded. Default 30s."),
			"services": map[string]any{
				"type":     "array",
				"minItems": 1,
				"items":    serviceSchema,
			},
			"checks": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"name"},
					"anyOf": []any{
						map[string]any{"required": []string{"command"}},
						map[string]any{"required": []string{"tcp"}},
					},
					"properties": map[string]any{
						"name":    stringSchema("Check name, shown in preflight output."),
						"command": argvSchema("Run this argv; exit 0 passes. Exactly one of command/tcp."),
						"tcp":     stringSchema("Dial host:port; connecting passes. Exactly one of command/tcp."),
					},
				},
			},
			"shells": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"name", "command"},
					"properties": map[string]any{
						"name":    stringSchema("Shell name for `mabo-ctl shell <name>`."),
						"service": stringSchema("Service whose dir and environment to reuse; empty means the repo root and the caller's environment."),
						"command": argvSchema("The argv to run."),
					},
				},
			},
		},
	}
	return json.MarshalIndent(s, "", "  ")
}

// serviceSchema describes one entry of services:. It is a package-level var
// because Schema and the drift test both read it.
var serviceSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"name", "cmd"},
	"properties": map[string]any{
		"name": map[string]any{
			"type":    "string",
			"pattern": "^[a-zA-Z0-9][a-zA-Z0-9_-]*$",
			"description": "Required. Must match the pattern: the name composes .dev/logs/<name>.log and" +
				" .dev/pids/<name>.pid, so / or .. would write outside .dev/.",
		},
		"dir":    stringSchema("Working directory relative to this file; must exist and stay inside the repo root. Defaults to the repo root."),
		"port":   map[string]any{"type": "integer", "minimum": 0, "maximum": 65535, "description": "0 (the default) = no port and no port guard."},
		"health": stringSchema("Readiness URL; {{.Port}} and {{.Port \"name\"}} templates allowed. Empty = no readiness probe."),
		"cmd":    argvSchema("The argv to run — never a shell string. cmd[0] is resolved through runtime:."),
		"env": map[string]any{
			"type":                 "object",
			"additionalProperties": scalarSchema("Environment values are scalars; unquoted numbers and booleans are read as strings."),
			"description":          "Inline environment. Values are templates. Overrides env_file key by key.",
		},
		"env_file":      stringSchema("KEY=VALUE file anchored at the repo root; inline env: overrides it. Validated at load, re-read at every resolve."),
		"runtime":       stringSchema("\"\", \"system\", \"conda:<env>\" or \"node:<version>\". Resolves cmd[0] to an absolute interpreter path; never falls back to ambient PATH."),
		"ready_timeout": durationSchema("This service's own readiness window, overriding the global. Leave out to inherit."),
		"depends_on":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Services that start first. Orders STARTS only — stop never expands dependencies."},
		"autostart":     map[string]any{"type": "boolean", "description": "false opts out of a bare `mabo-ctl start` only; naming the service always starts it. Default true."},
		"color":         stringSchema("Terminal colour for this service's label: a name (green, blue, bright-cyan, …), a 0-255 palette number, or #rrggbb."),
	},
}

// stringSchema is the one-line string property every schema repeats.
func stringSchema(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// argvSchema is a command vector: a non-empty list of strings.
func argvSchema(description string) map[string]any {
	return map[string]any{
		"type":        "array",
		"minItems":    1,
		"items":       map[string]any{"type": "string"},
		"description": description,
	}
}

// durationSchema accepts what durationValue accepts: a Go duration string
// ("10s", "1m30s", "500ms") or a bare number of seconds. A bare number arrives
// as a JSON number, a duration string as a string.
func durationSchema(description string) map[string]any {
	return map[string]any{
		"type":        []string{"string", "number"},
		"description": description + " Accepts \"10s\", \"1m30s\", \"500ms\" or a bare number of seconds.",
	}
}

// scalarSchema is the per-value schema of an env map: any YAML scalar.
func scalarSchema(description string) map[string]any {
	return map[string]any{
		"type":        []string{"string", "number", "boolean", "null"},
		"description": description,
	}
}
