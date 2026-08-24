package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maborak/mabo-ctl/internal/ui"
)

// configFixture declares one ported service with a credential in every field
// `mabo-ctl config` renders — the health URL, a command argument and the declared
// environment — plus a portless one, so a single fixture covers redaction and
// the portless row at once.
const configFixture = `
stop_grace: 5s
ready_timeout: 45s
services:
  - name: alpha
    port: 7100
    health: http://admin:hunter2@localhost:{{.Port}}/health?api_key=sk-live-DEADBEEF
    cmd: [echo, serve, --port, "{{.Port}}", --token, ghp_realtokenvalue]
    env:
      API_TOKEN: sk-live-SECRETVALUE
      LOG_LEVEL: debug
    color: green
  - name: beta
    port: 7101
    cmd: [echo, beta]
    depends_on: [alpha]
  - name: gamma
    cmd: [echo, gamma]
`

// runConfigHarness runs `mabo-ctl config` over configFixture and returns stdout.
func runConfigHarness(t *testing.T, args ...string) (h *harness, stdout string) {
	t.Helper()
	h = newHarnessWithConfig(t, configFixture, args...)
	if code := h.run(); code != exitOK {
		t.Fatalf("mabo-ctl %v = %d, want 0\nstderr: %s", args, code, h.stderr.String())
	}
	return h, h.stdout.String()
}

// TestConfigNamesTheLoadedPath is the first half of what the command is for:
// config discovery walks UP the tree, so the mabo-ctl.yaml that won may belong to
// a parent directory the operator was not thinking of, and no other command
// says which file it read.
func TestConfigNamesTheLoadedPath(t *testing.T) {
	h, out := runConfigHarness(t, "config")
	want := filepath.Join(h.root, "mabo-ctl.yaml")
	if !strings.Contains(out, want) {
		t.Fatalf("config did not name the loaded file %s:\n%s", want, out)
	}
	if !strings.Contains(out, "found by walking up") {
		t.Errorf("config did not say the file was discovered rather than given:\n%s", out)
	}
}

// TestConfigNamesAnExplicitPath is the same question when --config answered it.
// The path must change AND the command must say the file was given rather than
// discovered, because "which file is this?" and "why this file?" are different
// questions.
func TestConfigNamesAnExplicitPath(t *testing.T) {
	elsewhere := t.TempDir()
	path := filepath.Join(elsewhere, "other.yaml")
	if err := os.WriteFile(path, []byte(configFixture), 0o600); err != nil {
		t.Fatalf("write other.yaml: %v", err)
	}

	h := newHarnessWithConfig(t, configFixture, "config", "--config", path)
	if code := h.run(); code != exitOK {
		t.Fatalf("mabo-ctl config --config = %d, want 0\nstderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, path) {
		t.Fatalf("config did not name the --config file %s:\n%s", path, out)
	}
	if strings.Contains(out, filepath.Join(h.root, "mabo-ctl.yaml")) {
		t.Errorf("config named the discovered file even though --config was given:\n%s", out)
	}
	if !strings.Contains(out, "given with --config") {
		t.Errorf("config did not say the file came from --config:\n%s", out)
	}
}

// TestConfigReportsThePortSource walks all four precedence levels. Reporting
// the number without the level is the state this command replaces: four inputs
// resolve a port and until now nothing said which one won.
func TestConfigReportsThePortSource(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		_, out := runConfigHarness(t, "config", "alpha")
		assertPortLine(t, out, "7100  from default")
	})

	t.Run("run.env", func(t *testing.T) {
		h := newHarnessWithConfig(t, configFixture, "config", "alpha")
		writeRunEnv(t, h.root, "PORT_ALPHA=7100\n")
		if code := h.run(); code != exitOK {
			t.Fatalf("mabo-ctl config alpha = %d, want 0\nstderr: %s", code, h.stderr.String())
		}
		assertPortLine(t, h.stdout.String(), "7100  from run.env")
	})

	t.Run("env", func(t *testing.T) {
		// A caller variable outranks the persisted value, so both are set: a
		// test that only sets the winner cannot tell precedence from luck.
		t.Setenv("ALPHA_PORT", "7500")
		h := newHarnessWithConfig(t, configFixture, "config", "alpha")
		writeRunEnv(t, h.root, "PORT_ALPHA=7300\n")
		if code := h.run(); code != exitOK {
			t.Fatalf("mabo-ctl config alpha = %d, want 0\nstderr: %s", code, h.stderr.String())
		}
		assertPortLine(t, h.stdout.String(), "7500  from env")
	})

	t.Run("flag", func(t *testing.T) {
		t.Setenv("ALPHA_PORT", "7500")
		h := newHarnessWithConfig(t, configFixture, "config", "alpha", "--ports=7999")
		writeRunEnv(t, h.root, "PORT_ALPHA=7300\n")
		if code := h.run(); code != exitOK {
			t.Fatalf("mabo-ctl config alpha --ports = %d, want 0\nstderr: %s", code, h.stderr.String())
		}
		assertPortLine(t, h.stdout.String(), "7999  from flag")
	})
}

// TestConfigFlagsAPersistedOverride covers the trap that cost a real debugging
// round: a port in .dev/run.env outranks a default that has since changed, so
// editing mabo-ctl.yaml appears to do nothing. The port line has to say so.
func TestConfigFlagsAPersistedOverride(t *testing.T) {
	h := newHarnessWithConfig(t, configFixture, "config", "alpha")
	writeRunEnv(t, h.root, "PORT_ALPHA=7999\n")
	if code := h.run(); code != exitOK {
		t.Fatalf("mabo-ctl config alpha = %d, want 0\nstderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	assertPortLine(t, out, "7999  from run.env")
	if !strings.Contains(out, "OVERRIDES the declared 7100") {
		t.Fatalf("a persisted port beating the declared default was not flagged:\n%s", out)
	}
}

// TestConfigJSONCarriesTheResolvedValues asserts the machine view parses and
// answers the same questions the text view does.
func TestConfigJSONCarriesTheResolvedValues(t *testing.T) {
	h := newHarnessWithConfig(t, configFixture, "config", "--json")
	writeRunEnv(t, h.root, "PORT_ALPHA=7999\n")
	if code := h.run(); code != exitOK {
		t.Fatalf("mabo-ctl config --json = %d, want 0\nstderr: %s", code, h.stderr.String())
	}

	var view ui.ConfigView
	if err := json.Unmarshal(h.stdout.Bytes(), &view); err != nil {
		t.Fatalf("decoding config --json: %v\n%s", err, h.stdout.String())
	}
	if view.Source.Path != filepath.Join(h.root, "mabo-ctl.yaml") {
		t.Errorf("source.path = %q, want the loaded mabo-ctl.yaml", view.Source.Path)
	}
	if view.Source.StopGraceMS != 5000 || view.Source.ReadyTimeoutMS != 45000 {
		t.Errorf("timeouts = %d/%d ms, want 5000/45000", view.Source.StopGraceMS, view.Source.ReadyTimeoutMS)
	}
	if len(view.Services) != 3 {
		t.Fatalf("got %d services, want 3", len(view.Services))
	}

	alpha := view.Services[0]
	if alpha.Name != "alpha" || alpha.Port != 7999 {
		t.Errorf("alpha = %s on %d, want alpha on 7999", alpha.Name, alpha.Port)
	}
	if alpha.PortSource != "run.env" || alpha.PortDeclared != 7100 || !alpha.PortOverride {
		t.Errorf("alpha port origin = %+v, want run.env over a declared 7100 flagged as an override", alpha)
	}
	if len(alpha.Cmd) == 0 || !filepath.IsAbs(alpha.Cmd[0]) {
		t.Errorf("alpha cmd[0] = %v, want the absolute interpreter path the runtime resolved", alpha.Cmd)
	}
	if alpha.CmdLine == "" {
		t.Error("alpha cmd_line is empty; the copyable command is half of what the view is for")
	}
	if got := view.Services[2]; got.Port != 0 || got.Name != "gamma" {
		t.Errorf("gamma = %+v, want the portless service reported with port 0", got)
	}
	if view.Services[1].DependsOn == nil || view.Services[1].DependsOn[0] != "alpha" {
		t.Errorf("beta depends_on = %v, want [alpha]", view.Services[1].DependsOn)
	}
}

// TestConfigRedactsCredentials is the error path that matters here: every field
// this command renders — health, cmd arguments, declared env, and the raw file
// underneath them — carries credentials as often as the next one, and this
// output is pasted into issues and terminal recordings.
func TestConfigRedactsCredentials(t *testing.T) {
	secrets := []string{"hunter2", "sk-live-DEADBEEF", "ghp_realtokenvalue", "sk-live-SECRETVALUE"}

	for _, args := range [][]string{
		{"config"},
		{"config", "alpha"},
		{"config", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, out := runConfigHarness(t, args...)
			for _, s := range secrets {
				if strings.Contains(out, s) {
					t.Errorf("mabo-ctl %v leaked %q:\n%s", args, s, out)
				}
			}
			// Over-redaction would be its own defect: the keys and the benign
			// values are exactly what the operator asked to see.
			for _, keep := range []string{"API_TOKEN", "LOG_LEVEL", "7100"} {
				if !strings.Contains(out, keep) {
					t.Errorf("mabo-ctl %v dropped %q, which is not a credential:\n%s", args, keep, out)
				}
			}
		})
	}
}

// TestConfigRawIsByteIdentical is the promise --raw makes: it is the file, not
// mabo-ctl's reading of the file, so it can be piped into yq or diffed.
func TestConfigRawIsByteIdentical(t *testing.T) {
	h, out := runConfigHarness(t, "config", "--raw")
	want, err := os.ReadFile(filepath.Join(h.root, "mabo-ctl.yaml"))
	if err != nil {
		t.Fatalf("read mabo-ctl.yaml: %v", err)
	}
	if out != string(want) {
		t.Fatalf("--raw output is not the file on disk\n--- want %d bytes ---\n%q\n--- got %d bytes ---\n%q",
			len(want), string(want), len(out), out)
	}
}

// TestConfigNarrowsToOneService checks that a named service is the only one
// rendered, and that the file section is dropped with it: a per-service slice of
// YAML would be mabo-ctl's guess at what the operator wrote.
func TestConfigNarrowsToOneService(t *testing.T) {
	_, out := runConfigHarness(t, "config", "beta")
	if !strings.Contains(out, "beta") {
		t.Fatalf("config beta did not render beta:\n%s", out)
	}
	for _, other := range []string{"gamma", "API_TOKEN"} {
		if strings.Contains(out, other) {
			t.Errorf("config beta rendered %q, which belongs to another service:\n%s", other, out)
		}
	}
	if strings.Contains(out, "stop_grace: 5s") {
		t.Errorf("config beta printed the raw file; narrowing to a service must drop it:\n%s", out)
	}
}

// TestConfigUsageErrors covers the three refusals, all of which must be exit 2
// so a script can tell a mistyped invocation from a broken stack.
func TestConfigUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown service", []string{"config", "nosuch"}, "unknown service"},
		{"raw with a service", []string{"config", "--raw", "alpha"}, "cannot be narrowed"},
		{"raw with json", []string{"config", "--raw", "--json"}, "cannot be combined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessWithConfig(t, configFixture, tc.args...)
			if code := h.run(); code != exitUsage {
				t.Fatalf("mabo-ctl %v = %d, want %d\nstderr: %s", tc.args, code, exitUsage, h.stderr.String())
			}
			if !strings.Contains(h.stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to explain %q", h.stderr.String(), tc.want)
			}
		})
	}
}

// TestConfigReportsAnUnresolvedRuntime covers the one service state that makes
// the command more useful than reading mabo-ctl.yaml: an interpreter that could
// not be resolved leaves Cmd[0] unresolved, and the view must say so instead of
// printing a command that will never run.
func TestConfigReportsAnUnresolvedRuntime(t *testing.T) {
	const body = `
services:
  - name: alpha
    port: 7100
    runtime: conda:definitely-not-installed
    cmd: [python, app.py]
`
	h := newHarnessWithConfig(t, body, "config", "alpha")
	if code := h.run(); code != exitOK {
		t.Fatalf("mabo-ctl config alpha = %d, want 0; a service that cannot run must still be inspectable\nstderr: %s",
			code, h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, "unresolved") {
		t.Errorf("config did not mark the command as unresolved:\n%s", out)
	}
	if !strings.Contains(out, "conda:definitely-not-installed") {
		t.Errorf("config did not name the runtime that failed to resolve:\n%s", out)
	}
}

// assertPortLine fails unless out carries the given "port  from source" text.
func assertPortLine(t *testing.T, out, want string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Fatalf("config did not report %q:\n%s", want, out)
	}
}

// writeRunEnv seeds the persisted port cache the way a previous run would have.
func writeRunEnv(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, ".dev")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create .dev: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.env"), []byte(body), 0o600); err != nil {
		t.Fatalf("write run.env: %v", err)
	}
}
