package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/maborak/mabo-ctl/internal/web"
	"github.com/spf13/cobra"
)

// parseCatalogue runs `mabo-ctl schema --commands` against a harness and
// decodes the document.
func parseCatalogue(t *testing.T, h *harness) map[string]any {
	t.Helper()
	if code := h.run(); code != exitOK {
		t.Fatalf("schema --commands exited %d; stderr:\n%s", code, h.stderr.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(h.stdout.Bytes(), &doc); err != nil {
		t.Fatalf("schema --commands stdout is not JSON: %v\n%s", err, h.stdout.String())
	}
	return doc
}

// catalogCommands extracts the commands array, keyed by name.
func catalogCommands(t *testing.T, doc map[string]any) map[string]map[string]any {
	t.Helper()
	raw, ok := doc["commands"].([]any)
	if !ok || len(raw) == 0 {
		t.Fatalf("catalogue has no commands array")
	}
	out := make(map[string]map[string]any, len(raw))
	for _, e := range raw {
		c, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("command entry is not an object: %v", e)
		}
		name, _ := c["name"].(string)
		if name == "" {
			t.Fatalf("command entry without a name: %v", c)
		}
		if _, dup := out[name]; dup {
			t.Errorf("command %q documented twice", name)
		}
		out[name] = c
	}
	return out
}

// TestSchemaCommandsEmitsACatalogue covers the shape of the whole document:
// exit-code table, machine contracts, HTTP surface, and per-command entries
// with the fields an integrator branches on.
func TestSchemaCommandsEmitsACatalogue(t *testing.T) {
	t.Parallel()
	doc := parseCatalogue(t, newHarnessAt(t, t.TempDir(), "schema", "--commands"))

	if got := doc["tool"]; got != "mabo-ctl" {
		t.Errorf("tool = %v, want mabo-ctl", got)
	}

	// Exit codes are THE script-level contract; an agent wiring this binary
	// into anything branches on them before anything else.
	rawCodes, ok := doc["exit_codes"].([]any)
	if !ok || len(rawCodes) < 5 {
		t.Fatalf("exit_codes missing or too short: %v", doc["exit_codes"])
	}
	got := map[string]bool{}
	for _, e := range rawCodes {
		entry := e.(map[string]any)
		got[entry["code"].(string)] = true
		if s, _ := entry["meaning"].(string); strings.TrimSpace(s) == "" {
			t.Errorf("exit code %v has empty meaning", entry["code"])
		}
	}
	for _, want := range []string{"0", "1", "2", "3", "4"} {
		if !got[want] {
			t.Errorf("exit_codes lacks %s", want)
		}
	}

	// Machine contracts must NAME the stable one: status --json.
	contracts := doc["machine_contracts"].([]any)
	var statusStable bool
	for _, c := range contracts {
		e := c.(map[string]any)
		if e["command"] == "status --json" && e["stable"] == true {
			statusStable = true
		}
	}
	if !statusStable {
		t.Errorf("machine_contracts does not mark status --json stable: %v", contracts)
	}

	// The HTTP surface comes straight from the live web package.
	routes := doc["http_api"].(map[string]any)["routes"].([]any)
	if len(routes) != len(web.Routes()) {
		t.Errorf("http_api.routes = %d entries, web.Routes() has %d", len(routes), len(web.Routes()))
	}
	for _, r := range routes {
		e := r.(map[string]any)
		switch e["method"] {
		case http.MethodGet, http.MethodPost:
		default:
			t.Errorf("route %v method %v", e["path"], e["method"])
		}
		if p, _ := e["path"].(string); p == "" || p[0] != '/' {
			t.Errorf("route path %q", p)
		}
		if d, _ := e["desc"].(string); strings.TrimSpace(d) == "" {
			t.Errorf("route %v has no description", e["path"])
		}
		if a, _ := e["auth"].(string); strings.TrimSpace(a) == "" {
			t.Errorf("route %v has no auth line", e["path"])
		}
	}
}

// TestSchemaCommandsDocumentsEveryCommand pins per-command content: the root
// shorthand, start's metadata, and every entry carrying flags with types.
func TestSchemaCommandsDocumentsEveryCommand(t *testing.T) {
	t.Parallel()
	doc := parseCatalogue(t, newHarnessAt(t, t.TempDir(), "schema", "--commands"))
	cmds := catalogCommands(t, doc)

	root, ok := cmds["mabo-ctl"]
	if !ok {
		t.Fatal("catalogue does not document the root binary")
	}
	if root["mutates"] != true {
		t.Errorf("root mutates = %v, want true (bare invocation starts services)", root["mutates"])
	}
	// The start flags live on the ROOT, because a bare service name uses them;
	// an agent driving `mabo-ctl backend -f` needs them documented there.
	flagNames := func(entry map[string]any) []string {
		fl, _ := entry["flags"].([]any)
		names := make([]string, 0, len(fl))
		for _, f := range fl {
			m := f.(map[string]any)
			names = append(names, m["name"].(string))
			if d, _ := m["desc"].(string); strings.TrimSpace(d) == "" {
				t.Errorf("flag %q of %v has no description", m["name"], entry["name"])
			}
			if _, ok := m["type"].(string); !ok {
				t.Errorf("flag %q of %v has no type", m["name"], entry["name"])
			}
		}
		return names
	}
	names := flagNames(root)
	for _, want := range []string{"ports", "port", "follow", "all", "attach", "interactive", "web-console"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("root flags lack %q: %v", want, names)
		}
	}

	start := cmds["start"]
	if start == nil {
		t.Fatal("catalogue lacks start")
	}
	if start["mutates"] != true {
		t.Errorf("start mutates = %v, want true", start["mutates"])
	}
	sides, _ := start["side_effects"].([]any)
	if len(sides) == 0 {
		t.Errorf("start documents no side effects")
	}
	if ec, _ := start["exit_codes"].(string); !strings.Contains(ec, "4") {
		t.Errorf("start exit_codes %q should mention exit 4", ec)
	}

	execEntry := cmds["exec"]
	if execEntry == nil {
		t.Fatal("catalogue lacks exec")
	}
	if ec, _ := execEntry["exit_codes"].(string); !strings.Contains(ec, "verbatim") {
		t.Errorf("exec exit_codes %q should say the child's code passes through verbatim", ec)
	}

	statusEntry := cmds["status"]
	if statusEntry == nil {
		t.Fatal("catalogue lacks status")
	}
	if statusEntry["mutates"] != false {
		t.Errorf("status mutates = %v, want false", statusEntry["mutates"])
	}
}

// TestSchemaCommandsMatchesTheLiveTree fails when a command is added without
// being given [commandMetas], or when one goes away leaving stale documentation
// behind.
func TestSchemaCommandsMatchesTheLiveTree(t *testing.T) {
	t.Parallel()
	h := newHarnessAt(t, t.TempDir(), "schema", "--commands")
	doc := parseCatalogue(t, h)

	a := newApp(h.env)
	a.bootstrap()
	tree := a.rootCmd()

	live := map[string]bool{}
	// Only commands cobra itself considers available are documented; the same
	// rule buildCatalog enforces. This includes the root binary under its own
	// name — a bare invocation is real behaviour an integrator must model.
	live[tree.Name()] = true
	for _, c := range tree.Commands() {
		if c.IsAvailableCommand() {
			live[c.Name()] = true
		}
	}

	documented := map[string]bool{}
	for name := range catalogCommands(t, doc) {
		documented[name] = true
		if !live[name] {
			t.Errorf("catalogue documents %q which no longer exists; remove its metadata", name)
		}
	}
	for name := range live {
		if !documented[name] {
			t.Errorf("command %q exists but is undocumented: add it to commandMetas", name)
		}
	}
}

// TestSchemaCommandsIsDeterministic: same binary, two runs, identical bytes.
// Anything else means an agent or cache sees different answers at random.
func TestSchemaCommandsIsDeterministic(t *testing.T) {
	t.Parallel()
	runOnce := func() string {
		h := newHarnessAt(t, t.TempDir(), "schema", "--commands")
		if code := h.run(); code != exitOK {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
		return h.stdout.String()
	}
	first := runOnce()
	for i := 0; i < 3; i++ {
		if again := runOnce(); again != first {
			t.Fatalf("run %d differs from run 1\n--- first ---\n%s\n--- again ---\n%s", i+2, first, again)
		}
	}
}

// TestSchemaCommandsRefusesUndocumented is the drift guard working as intended:
// a subcommand nobody wrote metadata for breaks the catalogue loudly.
func TestSchemaCommandsRefusesUndocumented(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{Use: "stub"}
	root.AddCommand(&cobra.Command{
		Use:   "frank",
		Short: "a command with no documentation",
		RunE:  func(*cobra.Command, []string) error { return nil },
	})
	_, err := buildCatalog(root)
	if err == nil {
		t.Fatal("buildCatalog accepted an undocumented command")
	}
	for _, want := range []string{"frank", "commandMetas"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestSchemaWithoutCommandsStillPrintsTheYAMLShape guards the default output
// the --commands branch sat beside: the draft-07 schema, untouched.
func TestSchemaWithoutCommandsStillPrintsTheYAMLShape(t *testing.T) {
	t.Parallel()
	h := newHarnessAt(t, t.TempDir(), "schema")
	if code := h.run(); code != exitOK {
		t.Fatalf("schema exited %d; stderr:\n%s", code, h.stderr.String())
	}
	var schema map[string]any
	if err := json.Unmarshal(h.stdout.Bytes(), &schema); err != nil {
		t.Fatalf("default schema output is not JSON: %v", err)
	}
	if id, _ := schema["$id"].(string); id != "mabo-ctl.schema.json" {
		t.Errorf("$id = %q, want mabo-ctl.schema.json", id)
	}
}
