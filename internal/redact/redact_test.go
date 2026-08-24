package redact

import (
	"strings"
	"testing"
)

// TestURLStripsCredentials covers the disclosure the console's /api/services
// and /api/status had before health and cmd were routed through redaction: both
// are readable without a token, and a health URL carries credentials as often
// as an environment variable does.
func TestURLStripsCredentials(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in string
		leaks    []string
	}{
		{"plain", "http://localhost:7100/health", nil},
		{"userinfo", "http://admin:hunter2@localhost:7100/health", []string{"hunter2"}},
		{"api key query", "http://localhost:7100/health?api_key=sk-live-DEADBEEF", []string{"sk-live-DEADBEEF"}},
		{"both", "http://admin:hunter2@h/health?token=ghp_realtokenvalue", []string{"hunter2", "ghp_realtokenvalue"}},
		{"benign query kept", "http://localhost:7100/health?verbose=1", nil},
		{"empty", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := URL(tc.in)
			for _, secret := range tc.leaks {
				if strings.Contains(got, secret) {
					t.Errorf("URL(%q) = %q; still contains %q", tc.in, got, secret)
				}
			}
			if tc.leaks == nil && got != tc.in {
				t.Errorf("URL(%q) = %q; a URL with no credential must be unchanged", tc.in, got)
			}
		})
	}
}

// TestArgsStripsCredentials covers the same disclosure via the command, which
// both front ends show deliberately because the operator asked to see it.
func TestArgsStripsCredentials(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		in    []string
		leaks []string
		keep  []string
	}{
		{"benign", []string{"npm", "run", "dev", "--port", "7100"}, nil, []string{"npm", "run", "dev", "7100"}},
		{"inline flag", []string{"serve", "--token=ghp_realtokenvalue"}, []string{"ghp_realtokenvalue"}, []string{"serve", "--token"}},
		{"separated flag", []string{"serve", "--api-key", "sk-live-DEADBEEF"}, []string{"sk-live-DEADBEEF"}, []string{"serve"}},
		{"dsn value", []string{"app", "postgres://user:hunter2@db/app"}, []string{"hunter2"}, []string{"app"}},
		{"bare token", []string{"app", "ghp_realtokenvalue"}, []string{"ghp_realtokenvalue"}, []string{"app"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := strings.Join(Args(tc.in), " ")
			for _, secret := range tc.leaks {
				if strings.Contains(got, secret) {
					t.Errorf("Args(%q) = %q; still contains %q", tc.in, got, secret)
				}
			}
			for _, want := range tc.keep {
				if !strings.Contains(got, want) {
					t.Errorf("Args(%q) = %q; dropped %q, which is not a secret and is why the command is shown at all", tc.in, got, want)
				}
			}
		})
	}
}

// TestArgsDoesNotMutateItsInput guards the caller that renders the same argv
// twice — once redacted for display and once as the vector mabo-ctl execs.
func TestArgsDoesNotMutateItsInput(t *testing.T) {
	t.Parallel()
	in := []string{"serve", "--token=ghp_realtokenvalue"}
	_ = Args(in)
	if in[1] != "--token=ghp_realtokenvalue" {
		t.Fatalf("Args mutated its argument: %q", in)
	}
}

func TestIsSecret(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key, value string
		want       bool
	}{
		{"API_TOKEN", "abc", true},
		{"api_token", "abc", true},
		{"GITHUB_SECRET", "abc", true},
		{"SSH_KEY_PATH", "/home/x/.ssh/id", true},
		{"DB_PASSWORD", "x", true},
		{"PGPASSWD", "x", true},
		{"AWS_CREDENTIALS", "x", true},
		{"AUTHORIZATION", "x", true},
		{"LOG_LEVEL", "debug", false},
		{"PORT", "7100", false},
		{"NODE_ENV", "development", false},
		// value-shaped credentials whose key names nothing
		{"DATABASE_URL", "postgres://app:hunter2@localhost/app", true},
		{"DATABASE_URL", "postgres://localhost/app", false},
		{"OPENAI", "sk-abcdefghijklmnop", true},
		{"CI_VAR", "eyJhbGciOiJIUzI1NiJ9.e30.x", true},
		{"CI_VAR", "-----BEGIN PRIVATE KEY-----", true},
		{"GREETING", "", false},
	}
	for _, tc := range cases {
		if got := IsSecret(tc.key, tc.value); got != tc.want {
			t.Errorf("IsSecret(%q, %q) = %v, want %v", tc.key, tc.value, got, tc.want)
		}
	}
}

func TestEnvIsSortedAndComplete(t *testing.T) {
	t.Parallel()
	got := Env(map[string]string{"ZED": "1", "ALPHA": "2", "MIDDLE_TOKEN": "3"})
	if len(got) != 3 {
		t.Fatalf("got %d vars, want 3", len(got))
	}
	if got[0].Key != "ALPHA" || got[1].Key != "MIDDLE_TOKEN" || got[2].Key != "ZED" {
		t.Errorf("keys = %v, want them sorted", []string{got[0].Key, got[1].Key, got[2].Key})
	}
	if got[1].Value == "3" {
		t.Error("MIDDLE_TOKEN kept its value")
	}
	if !got[1].Redacted {
		t.Error("MIDDLE_TOKEN is not flagged as redacted, so a reader cannot tell [redacted] from a literal value")
	}
	if Env(nil) == nil {
		t.Error("Env(nil) = nil, want an empty slice so JSON renders []")
	}
}

// TestYAMLLeavesStructureAlone is the property that makes it safe to show a
// redacted mabo-ctl.yaml at all: a file with no credentials in it must come back
// byte-identical, comments, blank lines, indentation and quoting included.
func TestYAMLLeavesStructureAlone(t *testing.T) {
	t.Parallel()
	const in = `# mabo-ctl.yaml
services:
  - name: website
    dir: website
    port: 7100
    health: http://localhost:{{.Port}}/robots.txt
    cmd: [npm, run, dev, --, --port, "{{.Port}}"]
    env:
      PUBLIC_API_BASE: http://localhost:{{.Port "backend"}}
      LOG_LEVEL: debug

    depends_on: [backend]
`
	if got := YAML(in); got != in {
		t.Fatalf("YAML rewrote a file with no credentials:\n--- want ---\n%s\n--- got ---\n%s", in, got)
	}
}

func TestYAMLRedactsCredentials(t *testing.T) {
	t.Parallel()
	const in = `services:
  - name: backend
    health: http://admin:hunter2@localhost:7102/health?api_key=sk-live-DEADBEEF
    cmd: [serve, "--token=ghp_realtokenvalue", --api-key, sk-live-OTHER]
    env:
      DB_PASSWORD: hunter3
      DATABASE_URL: postgres://app:hunter4@db/app
      LOG_LEVEL: debug
`
	got := YAML(in)
	for _, secret := range []string{
		"hunter2", "hunter3", "hunter4",
		"sk-live-DEADBEEF", "sk-live-OTHER", "ghp_realtokenvalue",
	} {
		if strings.Contains(got, secret) {
			t.Errorf("YAML output still contains %q:\n%s", secret, got)
		}
	}
	for _, keep := range []string{"name: backend", "LOG_LEVEL: debug", "cmd: [serve,", "DB_PASSWORD:"} {
		if !strings.Contains(got, keep) {
			t.Errorf("YAML output dropped %q, which is structure and not a credential:\n%s", keep, got)
		}
	}
}

// TestYAMLKeepsCommentsAndEmptyInput covers the two shapes a line-oriented
// redactor is most likely to mangle.
func TestYAMLKeepsCommentsAndEmptyInput(t *testing.T) {
	t.Parallel()
	if got := YAML(""); got != "" {
		t.Errorf("YAML(%q) = %q, want it unchanged", "", got)
	}
	const in = "  # DB_PASSWORD: hunter2 is documented here on purpose\n"
	if got := YAML(in); got != in {
		t.Errorf("YAML rewrote a comment:\n%q", got)
	}
}

// TestRedactsCredentialShapesThatUsedToLeak covers three disclosure paths found
// by adversarial review. Each one had the same character: the redaction fired
// on something harmless and left the actual secret in the clear.
func TestRedactsCredentialShapesThatUsedToLeak(t *testing.T) {
	t.Run("positional KEY=VALUE in an argv", func(t *testing.T) {
		// `env DATABASE_URL=... cmd` is the ordinary way a credential reaches an
		// argv. The dash test meant only --flag=value was ever considered.
		got := strings.Join(Args([]string{
			"/usr/bin/env",
			"DATABASE_URL=postgres://app:pgpass1@db:5432/app",
			"API_TOKEN=ghp_ARGTOKEN1234",
			"/bin/sh",
		}), " ")
		for _, secret := range []string{"pgpass1", "ghp_ARGTOKEN1234"} {
			if strings.Contains(got, secret) {
				t.Errorf("Args leaked %q: %s", secret, got)
			}
		}
		if !strings.Contains(got, "/usr/bin/env") || !strings.Contains(got, "DATABASE_URL") {
			t.Errorf("Args over-redacted; the command and the key must stay visible: %s", got)
		}
	})

	t.Run("bare-token userinfo", func(t *testing.T) {
		// https://<token>@host is the standard git/npm/pip form. It has no
		// colon, so the user:pass pattern never matched it.
		for _, raw := range []string{
			"https://ghp_REALTOKENVALUE@github.com/org/repo.git",
			"https://x-access-token:ghp_OTHER@github.com/org/repo.git",
		} {
			got := URL(raw)
			for _, secret := range []string{"ghp_REALTOKENVALUE", "ghp_OTHER"} {
				if strings.Contains(got, secret) {
					t.Errorf("URL(%q) leaked %q: %s", raw, secret, got)
				}
			}
			if !strings.Contains(got, "github.com") {
				t.Errorf("URL(%q) lost the host: %s", raw, got)
			}
		}
	})

	t.Run("block scalar", func(t *testing.T) {
		// The value lives on the lines BELOW the key, so a line-at-a-time pass
		// redacted the `|` indicator and printed the secret underneath it.
		in := "services:\n  - name: api\n    api_key: |\n      super-secret-value\n      second-secret-line\n    port: 7100\n"
		got := YAML(in)
		for _, secret := range []string{"super-secret-value", "second-secret-line"} {
			if strings.Contains(got, secret) {
				t.Errorf("YAML leaked %q from a block scalar:\n%s", secret, got)
			}
		}
		if !strings.Contains(got, "port: 7100") {
			t.Errorf("YAML dropped a sibling key after the block:\n%s", got)
		}
		if !strings.Contains(got, "name: api") {
			t.Errorf("YAML dropped an unrelated key:\n%s", got)
		}
	})

	t.Run("tab separator", func(t *testing.T) {
		got := YAML("password:\thunter2\n")
		if strings.Contains(got, "hunter2") {
			t.Errorf("a tab between key and value bypassed redaction: %q", got)
		}
	})

	t.Run("flow mapping", func(t *testing.T) {
		got := YAML("env: {LOG_LEVEL: debug, API_TOKEN: ghp_FLOWTOKEN}\n")
		if strings.Contains(got, "ghp_FLOWTOKEN") {
			t.Errorf("an inline flow mapping bypassed redaction: %q", got)
		}
		if !strings.Contains(got, "debug") {
			t.Errorf("flow mapping over-redacted a benign value: %q", got)
		}
	})
}
