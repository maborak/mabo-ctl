package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The baseline is what Load parsed, not a second read: an edit to the YAML, an
// edit to an env_file it names, and a revert back to the loaded bytes must each
// be reported as exactly what they are.
func TestChangedComparesAgainstTheLoadedSources(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env.local")
	if err := os.WriteFile(envPath, []byte("GREETING=hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := "services:\n  - name: api\n    cmd: [echo, hi]\n    env_file: .env.local\n"
	path := write(t, dir, body)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := func() bool {
		t.Helper()
		got, err := cfg.Changed()
		if err != nil {
			t.Fatalf("Changed: %v", err)
		}
		return got
	}

	if changed() {
		t.Fatal("a freshly loaded config reports a change")
	}
	write(t, dir, body+"# edited\n")
	if !changed() {
		t.Fatal("a YAML edit was not reported")
	}
	write(t, dir, body)
	if changed() {
		t.Fatal("reverting the YAML to the loaded bytes still reports a change")
	}
	if err := os.WriteFile(envPath, []byte("GREETING=hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !changed() {
		t.Fatal("an env_file edit was not reported; its contents are the service's environment")
	}
}

// ApplyProfiles filters Services after Load. The sources must be the set Load
// recorded, or a profiled config would hash fewer env files than its baseline
// and report a change forever.
func TestChangedIsStableAcrossProfileFiltering(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"a.env", "b.env"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("X=1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := write(t, dir, `services:
  - name: a
    cmd: [echo]
    env_file: a.env
  - name: b
    cmd: [echo]
    env_file: b.env
    profiles: [extra]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ApplyProfiles([]string{"extra"}); err != nil {
		t.Fatal(err)
	}
	if got, err := cfg.Changed(); err != nil || got {
		t.Fatalf("Changed after ApplyProfiles = %v, %v; want false, nil", got, err)
	}
}

func TestChangedReportsAnUnreadableConfig(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, okService)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, err := cfg.Changed(); !got || err == nil {
		t.Fatalf("Changed on a deleted config = %v, %v; want true and the read error", got, err)
	}
}

// A load error is printed on stderr and served by the web console, so the
// credential a malformed line or template carries must never be in it.
// docs/LANDMINES.md §4: redaction belongs where the string is built.
func TestLoadErrorsNeverQuoteACredential(t *testing.T) {
	const secret = "sk-live-SECRET-VALUE-42"
	cases := map[string]struct {
		envFile string
		yaml    string
	}{
		"env_file line in the wrong syntax": {
			envFile: "GOOD=1\nAPI_TOKEN: " + secret + "\n",
			yaml:    "services:\n  - name: api\n    cmd: [echo]\n    env_file: .env.local\n",
		},
		"env value that is not a template": {
			yaml: "services:\n  - name: api\n    cmd: [echo]\n    env:\n      API_TOKEN: \"" + secret + "{{\"\n",
		},
		"env_file value that is not a template": {
			envFile: "API_TOKEN=" + secret + "{{\n",
			yaml:    "services:\n  - name: api\n    cmd: [echo]\n    env_file: .env.local\n",
		},
		"cmd flag value that is not a template": {
			yaml: "services:\n  - name: api\n    cmd: [echo, \"--token=" + secret + "{{\"]\n",
		},
		"health URL credential with an unusable template": {
			yaml: "services:\n  - name: api\n    cmd: [echo]\n    health: \"http://admin:" + secret + "@localhost:{{.Port}}/\"\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.envFile != "" {
				if err := os.WriteFile(filepath.Join(dir, ".env.local"), []byte(tc.envFile), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Load(write(t, dir, tc.yaml))
			if err == nil {
				t.Fatal("Load accepted a config that must be refused")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("the load error quotes the credential:\n%v", err)
			}
		})
	}
}
