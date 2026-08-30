// Package surface enumerates mabo-ctl's LIVE integration surfaces so a
// committed map can be diffed against reality on every suite run. Four
// sections exist because four things integrate against us from the outside,
// and anything else would claim an interface nobody has:
//
//	cli     every command AND every long-form flag of it
//	config  every schema field of mabo-ctl.yaml (+ template forms)
//	json    every key of the status --json document
//	http    every web-console route as method+path+guard, from [internal/web.Routes]
//
// Everything here is derived FROM THE BUILT BINARY (schema --commands and
// schema are generated from its live command tree, the json section is
// marshalled through ui.StatusJSON itself, and the http section is the served
// route table), so renaming a shipped flag, field or key — or turning a
// documented guard into a bare registration — fails the drift gate rather than
// drifting silently.
package surface

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"

	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/ui"
	"github.com/maborak/mabo-ctl/internal/web"
)

// Map is the committed artifact: stable ids grouped by section, sorted.
type Map struct {
	Generator string            `json:"generator"`
	Sections  map[string][]Name `json:"sections"`
}

// Name is one enumerated surface id, e.g. `cli:start --ports`.
type Name string

// Enumerate shells out to the built binary for the cli and config sections and
// marshals a sample status record through ui.StatusJSON for the json section.
func Enumerate(binaryPath string) (Map, error) {
	m := Map{Generator: "go run ./tools/surfacemap", Sections: map[string][]Name{}}

	cli, err := enumerateCLI(binaryPath)
	if err != nil {
		return m, err
	}
	cfg, err := enumerateConfig(binaryPath)
	if err != nil {
		return m, err
	}
	jsonKeys, err := enumerateStatusJSON()
	if err != nil {
		return m, err
	}
	m.Sections["cli"] = cli
	m.Sections["config"] = cfg
	m.Sections["json"] = jsonKeys
	m.Sections["http"] = enumerateHTTP()

	for _, s := range m.Sections {
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	}
	return m, nil
}

// enumerateHTTP names every web-console route as method + path + guard. The
// guard is part of the id deliberately: a route whose kind is changed from
// RouteRead to an unguarded registration is a security-relevant change, and the
// drift gate must fail on it exactly as it fails on a renamed flag.
func enumerateHTTP() []Name {
	out := make([]Name, 0, 32)
	for _, r := range web.Routes() {
		out = append(out, Name(fmt.Sprintf("http:%s %s (%s)", r.Method, r.Path, r.Kind)))
	}
	return out
}

func runBinary(bin string, args ...string) ([]byte, error) {
	out, err := exec.Command(bin, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", bin[skipToLastSlash(bin):], joinArgs(args), err)
	}
	return out, nil
}

func enumerateCLI(bin string) ([]Name, error) {
	raw, err := runBinary(bin, "schema", "--commands")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Commands []struct {
			Name           string `json:"name"`
			Flags          []flag `json:"flags"`
			InheritedFlags []flag `json:"inherited_flags"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("catalogue JSON: %w", err)
	}
	var out []Name
	for _, c := range doc.Commands {
		out = append(out, Name("cli:"+c.Name))
		for _, grp := range [][]flag{c.Flags, c.InheritedFlags} {
			for _, f := range grp {
				out = append(out, Name(fmt.Sprintf("cli:%s --%s", c.Name, f.Name)))
			}
		}
	}
	return out, nil
}

type flag struct {
	Name string `json:"name"`
}

func enumerateConfig(bin string) ([]Name, error) {
	raw, err := runBinary(bin, "schema")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Properties map[string]node `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("schema JSON: %w", err)
	}
	var out []Name
	for name, n := range doc.Properties {
		out = append(out, Name("config:"+name))
		if len(n.Items.Properties) > 0 {
			for f := range n.Items.Properties {
				out = append(out, Name(fmt.Sprintf("config:%s[].%s", name, f)))
			}
		}
	}
	for _, form := range []string{"{{.Port}}", `{{.Port "svc"}}`} {
		out = append(out, Name("config:template."+form))
	}
	return out, nil
}

type node struct {
	Items struct {
		Properties map[string]json.RawMessage `json:"properties"`
	} `json:"items"`
}

// enumerateStatusJSON routes ONE filled record through the very function every
// consumer relies on, so the ids here cannot disagree with a shipped rename:
// the drift-gate failure IS the compile-and-run failing.
func enumerateStatusJSON() ([]Name, error) {
	doc, err := ui.StatusJSON([]supervisor.Status{
		{Name: "probe"},
	})
	if err != nil {
		return nil, err
	}
	var records []map[string]any
	if err := json.Unmarshal(doc, &records); err != nil {
		return nil, fmt.Errorf("status --json sample: %w", err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("status --json emitted no records")
	}
	keys := make([]string, 0, len(records[0]))
	for k := range records[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Name, 0, len(keys))
	for _, k := range keys {
		out = append(out, Name("json[]."+k))
	}
	return out, nil
}

func skipToLastSlash(p string) int {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return i + 1
		}
	}
	return 0
}

func joinArgs(args []string) string { return fmt.Sprint(args) }
