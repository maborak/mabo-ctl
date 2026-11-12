package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestInitScaffoldNeutralisesNewlineInjection pins the audit finding on the
// never-before-tested init surface: repo-derived text lands inside '#'
// comment lines of the scaffolded mabo-ctl.yaml, and a raw newline would
// terminate the comment and turn the rest into ACTIVE yaml — where a
// checks: entry executes when preflight runs. The hostile inputs are
// ordinary repo content: a directory name and an .nvmrc.
func TestInitScaffoldNeutralisesNewlineInjection(t *testing.T) {
	dir := t.TempDir()
	hostile := "svc\n  pwned-check:\n    command: [touch, /tmp/mabo-pwned]"
	pkg := []byte(`{"scripts": {"dev": "vite"}}`)
	if err := os.WriteFile(filepath.Join(dir, "package.json"), pkg, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".nvmrc"),
		[]byte("1.8\n"+hostile), 0o644); err != nil {
		t.Fatal(err)
	}

	g := detectNode(dir, hostile)
	if g == nil {
		t.Fatal("detectNode did not recognise the package.json fixture")
	}
	body := renderInit([]guess{*g})

	for _, marker := range []string{"pwned-check", "touch", "mabo-pwned"} {
		for _, line := range strings.Split(body, "\n") {
			if strings.Contains(line, marker) && !strings.HasPrefix(strings.TrimSpace(line), "#") {
				t.Fatalf("scaffold has an ACTIVE line containing %q:\n%s", marker, line)
			}
		}
	}
	if !strings.Contains(body, "runtime: node:1.8") {
		t.Errorf("the real .nvmrc version was lost:\n%s", body)
	}
	if strings.Contains(body, "node:1.8\n") {
		t.Errorf("version line still carries a raw newline:\n%s", body)
	}
}

// TestYAMLCommentNeutralisesUnicodeLineBreaks: YAML ends a comment at NEL,
// LINE SEPARATOR and PARAGRAPH SEPARATOR as well as at ASCII newlines, so a
// directory name carrying one could otherwise inject live YAML into a
// scaffold that promises nothing runs yet.
func TestYAMLCommentNeutralisesUnicodeLineBreaks(t *testing.T) {
	for _, sep := range []string{"\u0085", " ", " "} {
		hostile := "a" + sep + "checks: [{name: pwn, command: [touch, PWNED]}]" + sep + "#"
		body := renderInit([]guess{{name: "x", comment: hostile, lines: []string{"# - name: x"}}})
		if strings.ContainsAny(body, "\u0085  ") {
			t.Errorf("separator %U reached the scaffold", []rune(sep)[0])
		}
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatalf("scaffold does not parse: %v", err)
		}
		if _, live := doc["checks"]; live {
			t.Errorf("separator %U injected a live checks: block", []rune(sep)[0])
		}
	}
}
