package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSchemaDescribesTheShippedExample is the drift guard between the schema
// and the parser. Every mapping key the shipped example uses — at any depth —
// must appear in the schema text, so a field added to the parser without a
// schema row is a test failure here, not a silent gap for editors.
func TestSchemaDescribesTheShippedExample(t *testing.T) {
	t.Parallel()
	raw, err := Schema()
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	if !json.Valid(raw) {
		t.Fatal("Schema() did not emit valid JSON")
	}

	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "mabo-ctl.yaml"))
	if err != nil {
		t.Fatalf("read the shipped example: %v", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(example, &node); err != nil {
		t.Fatalf("parse the shipped example: %v", err)
	}

	schema := string(raw)
	missing := map[string]bool{}
	collectKeys(&node, false, missing)
	for k := range missing {
		if !strings.Contains(schema, `"`+k+`"`) {
			t.Errorf("the example uses key %q that the schema never mentions", k)
		}
	}
}

// collectKeys walks a YAML node collecting every mapping key, EXCEPT the keys
// inside env: maps — those are arbitrary variable names no schema enumerates.
func collectKeys(n *yaml.Node, inEnv bool, out map[string]bool) {
	if n == nil {
		return
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			if !inEnv {
				out[key] = true
			}
			collectKeys(n.Content[i+1], inEnv || key == "env", out)
		}
		return
	}
	for _, c := range n.Content {
		collectKeys(c, inEnv, out)
	}
}

// TestSchemaRequiresTheEssentials pins the required lists: a document without
// services, or a service without name and cmd, must be describable as invalid
// by the schema's own required arrays.
func TestSchemaRequiresTheEssentials(t *testing.T) {
	t.Parallel()
	raw, err := Schema()
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	var doc struct {
		Properties struct {
			Services struct {
				Items struct {
					Required []string `json:"required"`
				} `json:"items"`
			} `json:"services"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if !contains(doc.Required, "services") {
		t.Error("the top-level required list does not demand services")
	}
	if !contains(doc.Properties.Services.Items.Required, "name") || !contains(doc.Properties.Services.Items.Required, "cmd") {
		t.Errorf("a service is not required to have name and cmd: %v", doc.Properties.Services.Items.Required)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestSchemaKeyIsAccepted: a $schema reference line parses — the strict decoder
// must not reject the very key that points editors at the schema.
func TestSchemaKeyIsAccepted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	body := `$schema: ./mabo-ctl.schema.json
services:
  - name: backend
    cmd: [echo, hi]
`
	cfg, err := Load(write(t, root, body))
	if err != nil {
		t.Fatalf("a $schema reference line failed to parse: %v", err)
	}
	if len(cfg.Services) != 1 {
		t.Fatalf("got %d services, want 1", len(cfg.Services))
	}
}
