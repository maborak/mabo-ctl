package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// write drops body into dir/mabo-ctl.yaml and returns the path.
func write(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, FileName)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// mkdirs creates each relative path under root.
func mkdirs(t *testing.T, root string, rel ...string) {
	t.Helper()
	for _, r := range rel {
		if err := os.MkdirAll(filepath.Join(root, r), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", r, err)
		}
	}
}

// validationProblems asserts err is a *ValidationError and returns its list.
func validationProblems(t *testing.T, err error) []string {
	t.Helper()
	if err == nil {
		t.Fatal("expected a validation error, got nil")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	return ve.Problems
}

const okService = `
services:
  - name: backend
    cmd: [echo, hi]
`

func TestValidationRules(t *testing.T) {
	cases := []struct {
		name string
		dirs []string
		yaml string
		want []string // every substring must appear somewhere in the problems
	}{
		// ---- rule 1: at least one service, unique names ----
		{
			name: "no services",
			yaml: "services: []\n",
			want: []string{"at least one entry under `services:`"},
		},
		{
			name: "empty file",
			yaml: "",
			want: []string{"no services declared"},
		},
		{
			name: "duplicate names",
			yaml: `
services:
  - name: backend
    cmd: [echo, a]
  - name: backend
    cmd: [echo, b]
`,
			want: []string{`duplicate service name "backend"`, "services[0]"},
		},

		// ---- rule 2: name is a path component (security control) ----
		{
			name: "name with slash is a path traversal",
			yaml: `
services:
  - name: logs/backend
    cmd: [echo, a]
`,
			want: []string{
				`invalid name "logs/backend"`,
				".dev/logs/<name>.log",
				"path traversal",
			},
		},
		{
			name: "name with dotdot is a path traversal",
			yaml: `
services:
  - name: ../../etc/cron.d/x
    cmd: [echo, a]
`,
			want: []string{`invalid name "../../etc/cron.d/x"`, "path traversal"},
		},
		{
			name: "name is exactly dotdot",
			yaml: `
services:
  - name: ".."
    cmd: [echo, a]
`,
			want: []string{`invalid name ".."`, "path traversal"},
		},
		{
			name: "name may not start with an underscore",
			yaml: `
services:
  - name: _hidden
    cmd: [echo, a]
`,
			want: []string{`invalid name "_hidden"`},
		},
		{
			name: "name is required",
			yaml: `
services:
  - cmd: [echo, a]
`,
			want: []string{"services[0]: name is required"},
		},

		// ---- rule 3: dir must exist, be a directory, and stay inside Root ----
		{
			name: "dir does not exist (dev.sh bug 1)",
			yaml: `
services:
  - name: browser
    dir: browser
    cmd: [echo, a]
`,
			want: []string{`dir "browser" does not exist`, "can only ever fail later"},
		},
		{
			name: "dir is a file",
			yaml: `
services:
  - name: backend
    dir: mabo-ctl.yaml
    cmd: [echo, a]
`,
			want: []string{"is not a directory"},
		},
		{
			name: "dir escapes root via ../..",
			yaml: `
services:
  - name: backend
    dir: ../../etc
    cmd: [echo, a]
`,
			want: []string{"outside the project root", "must stay inside"},
		},
		{
			name: "absolute dir outside root is rejected",
			yaml: `
services:
  - name: backend
    dir: /etc
    cmd: [echo, a]
`,
			want: []string{"outside the project root"},
		},

		// ---- rule 4: cmd must be non-empty (dev.sh bug 2) ----
		{
			name: "cmd missing",
			yaml: `
services:
  - name: backend
`,
			want: []string{"cmd is empty"},
		},
		{
			name: "cmd empty list",
			yaml: `
services:
  - name: backend
    cmd: []
`,
			want: []string{"cmd is empty"},
		},
		{
			name: "cmd first element blank",
			yaml: `
services:
  - name: backend
    cmd: ["", "--flag"]
`,
			want: []string{"cmd[0] is empty"},
		},

		// ---- rule 5: depends_on resolves, and no cycles ----
		{
			name: "depends_on unknown service",
			yaml: `
services:
  - name: worker
    cmd: [echo, a]
    depends_on: [backend]
`,
			want: []string{`depends_on names unknown service "backend"`, "declared services are: worker"},
		},
		{
			name: "dependency cycle of three",
			yaml: `
services:
  - name: a
    cmd: [echo, a]
    depends_on: [b]
  - name: b
    cmd: [echo, b]
    depends_on: [c]
  - name: c
    cmd: [echo, c]
    depends_on: [a]
`,
			want: []string{"dependency cycle: a -> b -> c -> a"},
		},
		{
			name: "self dependency is a cycle",
			yaml: `
services:
  - name: a
    cmd: [echo, a]
    depends_on: [a]
`,
			want: []string{"dependency cycle: a -> a"},
		},
		{
			name: "duplicate dependency",
			yaml: `
services:
  - name: a
    cmd: [echo, a]
  - name: b
    cmd: [echo, b]
    depends_on: [a, a]
`,
			want: []string{`depends_on lists "a" more than once`},
		},

		// ---- rule 6: port range, and health that needs a port ----
		{
			name: "port out of range high",
			yaml: `
services:
  - name: a
    port: 70000
    cmd: [echo, a]
`,
			want: []string{"port 70000 is out of range", "1..65535"},
		},
		{
			name: "port out of range negative",
			yaml: `
services:
  - name: a
    port: -1
    cmd: [echo, a]
`,
			want: []string{"port -1 is out of range"},
		},
		{
			name: "health references own port but service has none",
			yaml: `
services:
  - name: worker
    health: http://localhost:{{.Port}}/health
    cmd: [echo, a]
`,
			want: []string{"references {{.Port}} but the service declares no port"},
		},

		// ---- rule 7: runtime ----
		{
			name: "unknown runtime",
			yaml: `
services:
  - name: a
    runtime: python3
    cmd: [echo, a]
`,
			want: []string{`invalid runtime "python3"`, `"conda:<env>"`},
		},
		{
			name: "bare conda runtime gets a hint",
			yaml: `
services:
  - name: a
    runtime: conda
    cmd: [echo, a]
`,
			want: []string{`invalid runtime "conda"`, `did you mean "conda:<env>"?`},
		},
		{
			name: "runtime env name may not traverse",
			yaml: `
services:
  - name: a
    runtime: conda:../../bin
    cmd: [echo, a]
`,
			want: []string{`invalid runtime "conda:../../bin"`, "escape the runtime root"},
		},
		{
			name: "node version may not traverse",
			yaml: `
services:
  - name: a
    runtime: node:../../../etc
    cmd: [echo, a]
`,
			want: []string{"node version", "escape the runtime root"},
		},

		// ---- rule 8: statically duplicate ports ----
		{
			name: "duplicate ports",
			yaml: `
services:
  - name: website
    port: 7100
    cmd: [echo, a]
  - name: frontend
    port: 7100
    cmd: [echo, b]
`,
			want: []string{"port 7100 is declared by more than one service", `services[0] "website"`, `services[1] "frontend"`},
		},

		// ---- templates and env keys ----
		{
			name: "unparseable template in cmd",
			yaml: `
services:
  - name: a
    port: 7100
    cmd: [echo, "{{.Port"]
`,
			want: []string{"is not a valid template"},
		},
		{
			name: "env key containing equals",
			yaml: `
services:
  - name: a
    cmd: [echo, a]
    env:
      "BAD=KEY": x
`,
			want: []string{`invalid env variable name "BAD=KEY"`},
		},

		// ---- durations ----
		{
			name: "negative stop_grace",
			yaml: `
stop_grace: -5s
services:
  - name: a
    cmd: [echo, a]
`,
			want: []string{"stop_grace must be positive"},
		},

		// ---- checks block ----
		{
			name: "check with both command and tcp",
			yaml: okService + `
checks:
  - name: pg
    tcp: localhost:5432
    command: [pg_isready]
`,
			want: []string{"set exactly one of command or tcp, not both"},
		},
		{
			name: "check with neither",
			yaml: okService + `
checks:
  - name: pg
`,
			want: []string{"set exactly one of command or tcp"},
		},
		{
			name: "check with a bad tcp target",
			yaml: okService + `
checks:
  - name: pg
    tcp: localhost
`,
			want: []string{"must be host:port"},
		},
		{
			name: "check with an out of range tcp port",
			yaml: okService + `
checks:
  - name: pg
    tcp: localhost:99999
`,
			want: []string{"invalid port"},
		},
		{
			name: "duplicate check names",
			yaml: okService + `
checks:
  - name: pg
    tcp: localhost:5432
  - name: pg
    tcp: localhost:5433
`,
			want: []string{`duplicate check name "pg"`},
		},

		// ---- shells block ----
		{
			name: "shell bound to an unknown service",
			yaml: okService + `
shells:
  - name: db
    service: nope
    command: [psql]
`,
			want: []string{`service "nope" is not a declared service`},
		},
		{
			name: "shell without a command",
			yaml: okService + `
shells:
  - name: db
    service: backend
`,
			want: []string{"shells[0] \"db\": command is empty"},
		},

		// ---- everything at once ----
		{
			name: "all problems are reported together, not first-error-wins",
			yaml: `
services:
  - name: bad/name
    dir: nope
    port: 99999
    runtime: perl
  - name: bad/name
    cmd: []
    depends_on: [ghost]
`,
			want: []string{
				"invalid name",
				`dir "nope" does not exist`,
				"port 99999 is out of range",
				`invalid runtime "perl"`,
				"cmd is empty",
				`depends_on names unknown service "ghost"`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mkdirs(t, root, tc.dirs...)
			path := write(t, root, tc.yaml)

			_, err := Load(path)
			problems := validationProblems(t, err)
			joined := strings.Join(problems, "\n")
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Errorf("missing %q in problems:\n%s", want, joined)
				}
			}
			// The rendered error must carry every problem too.
			for _, p := range problems {
				if !strings.Contains(err.Error(), p) {
					t.Errorf("Error() dropped problem %q", p)
				}
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("Error() should name the config path %q, got %q", path, err.Error())
			}
		})
	}
}

func TestValidConfigs(t *testing.T) {
	cases := []struct {
		name string
		dirs []string
		yaml string
	}{
		{
			name: "minimal",
			yaml: okService,
		},
		{
			name: "dir defaults to the root",
			yaml: `
services:
  - name: a
    dir: "."
    cmd: [echo, a]
`,
		},
		{
			name: "portless service may reference another service's port",
			yaml: `
services:
  - name: backend
    port: 7102
    cmd: [echo, a]
  - name: worker
    cmd: [echo, b]
    depends_on: [backend]
    env:
      API_BASE: http://localhost:{{.Port "backend"}}
`,
		},
		{
			name: "diamond dependencies are not a cycle",
			yaml: `
services:
  - name: a
    cmd: [echo, a]
  - name: b
    cmd: [echo, b]
    depends_on: [a]
  - name: c
    cmd: [echo, c]
    depends_on: [a]
  - name: d
    cmd: [echo, d]
    depends_on: [b, c]
`,
		},
		{
			name: "several portless services do not collide",
			yaml: `
services:
  - name: a
    cmd: [echo, a]
  - name: b
    cmd: [echo, b]
`,
		},
		{
			name: "nested dir inside the root",
			dirs: []string{"services/backend"},
			yaml: `
services:
  - name: backend
    dir: services/backend
    cmd: [echo, a]
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mkdirs(t, root, tc.dirs...)
			path := write(t, root, tc.yaml)
			if _, err := Load(path); err != nil {
				t.Fatalf("expected a valid config, got: %v", err)
			}
		})
	}
}

func TestLoadDefaultsAndOverrides(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		root := t.TempDir()
		cfg, err := Load(write(t, root, okService))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.StopGrace != 10*time.Second {
			t.Errorf("StopGrace = %s, want 10s", cfg.StopGrace)
		}
		if cfg.ReadyTimeout != 30*time.Second {
			t.Errorf("ReadyTimeout = %s, want 30s", cfg.ReadyTimeout)
		}
		if cfg.Root != root {
			t.Errorf("Root = %q, want %q", cfg.Root, root)
		}
		if cfg.Path != filepath.Join(root, FileName) {
			t.Errorf("Path = %q, want %q", cfg.Path, filepath.Join(root, FileName))
		}
		if !filepath.IsAbs(cfg.Root) || !filepath.IsAbs(cfg.Path) {
			t.Errorf("Root and Path must be absolute, got %q and %q", cfg.Root, cfg.Path)
		}
	})

	t.Run("duration strings", func(t *testing.T) {
		root := t.TempDir()
		cfg, err := Load(write(t, root, "stop_grace: 1m30s\nready_timeout: 500ms\n"+okService))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.StopGrace != 90*time.Second {
			t.Errorf("StopGrace = %s, want 1m30s", cfg.StopGrace)
		}
		if cfg.ReadyTimeout != 500*time.Millisecond {
			t.Errorf("ReadyTimeout = %s, want 500ms", cfg.ReadyTimeout)
		}
	})

	t.Run("bare seconds", func(t *testing.T) {
		root := t.TempDir()
		cfg, err := Load(write(t, root, "stop_grace: 45\n"+okService))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.StopGrace != 45*time.Second {
			t.Errorf("StopGrace = %s, want 45s", cfg.StopGrace)
		}
	})

	t.Run("unparseable duration", func(t *testing.T) {
		root := t.TempDir()
		_, err := Load(write(t, root, "stop_grace: soon\n"+okService))
		if err == nil {
			t.Fatal("expected an error for a non-duration stop_grace")
		}
		if !strings.Contains(err.Error(), "is not a duration") {
			t.Errorf("unhelpful error: %v", err)
		}
	})
}

func TestLoadParsesServiceFields(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "backend")
	path := write(t, root, `
services:
  - name: backend
    dir: backend
    port: 7102
    health: http://localhost:{{.Port}}/health
    runtime: conda:app-dev
    cmd: [uvicorn, "api_main:app", --port, "{{.Port}}"]
    env:
      LOG_LEVEL: info
      WORKERS: 4
      DEBUG: true
      EMPTY:
    depends_on: []
    color: blue
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := cfg.Service("backend")
	if !ok {
		t.Fatal("Service(backend) not found")
	}
	if got.Port != 7102 || got.Runtime != "conda:app-dev" || got.Color != "blue" {
		t.Errorf("unexpected spec: %+v", got)
	}
	if got.Health.Kind != "http" || got.Health.HTTP != "http://localhost:{{.Port}}/health" {
		t.Errorf("Health must stay RAW and decode a scalar as http, got %+v", got.Health)
	}
	if len(got.Cmd) != 4 || got.Cmd[0] != "uvicorn" || got.Cmd[3] != "{{.Port}}" {
		t.Errorf("Cmd must stay RAW, got %#v", got.Cmd)
	}
	// Non-string scalars are usable without quoting.
	for k, want := range map[string]string{"LOG_LEVEL": "info", "WORKERS": "4", "DEBUG": "true", "EMPTY": ""} {
		if got.Env[k] != want {
			t.Errorf("Env[%q] = %q, want %q", k, got.Env[k], want)
		}
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	root := t.TempDir()
	_, err := Load(write(t, root, `
services:
  - name: backend
    cmd: [echo, a]
    dependson: [ghost]
`))
	if err == nil {
		t.Fatal("a misspelled field must be an error, not a silent default")
	}
	if !strings.Contains(err.Error(), "dependson") {
		t.Errorf("error should name the unknown field, got: %v", err)
	}
}

func TestLoadMalformedYAML(t *testing.T) {
	root := t.TempDir()
	_, err := Load(write(t, root, "services: [\n  - name: x\n"))
	if err == nil {
		t.Fatal("expected a parse error")
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		t.Errorf("a parse error must not masquerade as a ValidationError: %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want fs.ErrNotExist, got %v", err)
	}
}

func TestDiscoverFromNestedSubdirectory(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "backend/app/internal/deep")
	path := write(t, root, `
services:
  - name: backend
    dir: backend
    cmd: [echo, a]
`)

	cfg, _, err := Discover(filepath.Join(root, "backend", "app", "internal", "deep"))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if cfg.Path != path {
		t.Errorf("Path = %q, want %q", cfg.Path, path)
	}
	if cfg.Root != root {
		t.Errorf("Root = %q, want %q", cfg.Root, root)
	}
}

func TestDiscoverPrefersTheNearestConfig(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "sub")
	write(t, root, "services:\n  - name: outer\n    cmd: [echo, a]\n")
	inner := write(t, filepath.Join(root, "sub"), "services:\n  - name: inner\n    cmd: [echo, a]\n")

	cfg, _, err := Discover(filepath.Join(root, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Path != inner {
		t.Errorf("Path = %q, want the nearest config %q", cfg.Path, inner)
	}
	if names := cfg.Names(); len(names) != 1 || names[0] != "inner" {
		t.Errorf("Names() = %v, want [inner]", names)
	}
}

// --- legacy-name fallback ---------------------------------------------------
//
// Discovery accepts the pre-rename spelling so an existing stack keeps
// working, but reports it: the caller is expected to tell the operator to
// rename. These tests pin the preference order — proximity first, then
// FileName over LegacyFileName within a directory.

// writeLegacy writes a file under the LEGACY name and returns its path.
func writeLegacy(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, LegacyFileName)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestDiscoverFallsBackToTheLegacyName(t *testing.T) {
	root := t.TempDir()
	legacy := writeLegacy(t, root, "services:\n  - name: old\n    cmd: [echo, a]\n")

	cfg, viaLegacy, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if cfg.Path != legacy {
		t.Errorf("Path = %q, want the legacy config %q", cfg.Path, legacy)
	}
	if !viaLegacy {
		t.Error("viaLegacy = false; a hit under the old spelling must be reported so the caller can announce it")
	}
}

func TestDiscoverPrefersTheNewNameWhenBothExist(t *testing.T) {
	root := t.TempDir()
	writeLegacy(t, root, "services:\n  - name: old\n    cmd: [echo, a]\n")
	fresh := write(t, root, "services:\n  - name: new\n    cmd: [echo, a]\n")

	cfg, viaLegacy, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if cfg.Path != fresh {
		t.Errorf("Path = %q, want %q — FileName must win where both spellings exist", cfg.Path, fresh)
	}
	if viaLegacy {
		t.Error("viaLegacy = true; loading the new name must not report the legacy one")
	}
}

func TestDiscoverLegacyFallbackIsProximityFirst(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "sub")
	write(t, root, "services:\n  - name: outer-new\n    cmd: [echo, a]\n")
	legacy := writeLegacy(t, filepath.Join(root, "sub"), "services:\n  - name: inner-old\n    cmd: [echo, a]\n")

	// The nearer directory carries only the legacy spelling; proximity wins
	// over preferring the new name at a higher level.
	cfg, viaLegacy, err := Discover(filepath.Join(root, "sub"))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if cfg.Path != legacy {
		t.Errorf("Path = %q, want the nearer legacy config %q", cfg.Path, legacy)
	}
	if !viaLegacy {
		t.Error("viaLegacy = false; the loaded file IS the legacy spelling")
	}
}

func TestDiscoverNotFoundNamesBothSpellings(t *testing.T) {
	repo := t.TempDir()
	mkdirs(t, repo, ".git", "deep")

	_, _, err := Discover(filepath.Join(repo, "deep"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), FileName) || !strings.Contains(err.Error(), LegacyFileName) {
		t.Errorf("not-found error should name both spellings, got: %v", err)
	}
}

func TestDiscoverStopsAtFilesystemRoot(t *testing.T) {
	dir := t.TempDir()

	_, _, err := Discover(dir)
	if err == nil {
		// Only possible if this machine really has a mabo-ctl.yaml above the
		// temp directory; that is not this test's subject.
		t.Skip("a mabo-ctl.yaml exists somewhere above the temp directory")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error should name where the search started, got: %v", err)
	}
}

func TestDiscoverFromAFilePath(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "backend")
	write(t, root, okService)
	marker := filepath.Join(root, "backend", "main.go")
	if err := os.WriteFile(marker, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := Discover(marker)
	if err != nil {
		t.Fatalf("Discover from a file path: %v", err)
	}
	if cfg.Root != root {
		t.Errorf("Root = %q, want %q", cfg.Root, root)
	}
}

func TestDiscoverPropagatesValidationErrors(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "sub")
	write(t, root, "services:\n  - name: bad/name\n    cmd: [echo, a]\n")

	_, _, err := Discover(filepath.Join(root, "sub"))
	problems := validationProblems(t, err)
	if len(problems) != 1 || !strings.Contains(problems[0], "invalid name") {
		t.Fatalf("unexpected problems: %v", problems)
	}
}

func TestServiceAndNames(t *testing.T) {
	root := t.TempDir()
	cfg, err := Load(write(t, root, `
services:
  - name: website
    port: 7100
    cmd: [echo, a]
    env:
      A: "1"
  - name: backend
    port: 7102
    cmd: [echo, b]
`))
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"website", "backend"}
	got := cfg.Names()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want declaration order %v", got, want)
		}
	}

	if _, ok := cfg.Service("ghost"); ok {
		t.Error("Service(ghost) reported found")
	}
	s, ok := cfg.Service("website")
	if !ok {
		t.Fatal("Service(website) not found")
	}

	// The returned Spec must not alias the Config.
	s.Cmd[0] = "mutated"
	s.Env["A"] = "mutated"
	again, _ := cfg.Service("website")
	if again.Cmd[0] != "echo" || again.Env["A"] != "1" {
		t.Errorf("Service must return a deep copy, config was mutated: %+v", again)
	}

	// Mutating the returned name slice must not affect the next call.
	got[0] = "mutated"
	if cfg.Names()[0] != "website" {
		t.Error("Names must return a fresh slice")
	}
}

func TestLookupCheckAndShell(t *testing.T) {
	root := t.TempDir()
	cfg, err := Load(write(t, root, okService+`
checks:
  - name: postgres
    tcp: localhost:5432
  - name: redis
    command: [redis-cli, ping]
shells:
  - name: db
    service: backend
    command: [psql, app_dev]
`))
	if err != nil {
		t.Fatal(err)
	}
	ck, ok := cfg.LookupCheck("postgres")
	if !ok || ck.TCP != "localhost:5432" {
		t.Errorf("LookupCheck(postgres) = %+v, %v", ck, ok)
	}
	ck, ok = cfg.LookupCheck("redis")
	if !ok || len(ck.Command) != 2 {
		t.Errorf("LookupCheck(redis) = %+v, %v", ck, ok)
	}
	if _, ok := cfg.LookupCheck("ghost"); ok {
		t.Error("LookupCheck(ghost) reported found")
	}
	sh, ok := cfg.LookupShell("db")
	if !ok || sh.Service != "backend" || sh.Command[0] != "psql" {
		t.Errorf("LookupShell(db) = %+v, %v", sh, ok)
	}
	if _, ok := cfg.LookupShell("ghost"); ok {
		t.Error("LookupShell(ghost) reported found")
	}
}

func TestNilConfigAccessorsAreSafe(t *testing.T) {
	var c *Config
	if got := c.Names(); got != nil {
		t.Errorf("Names() on nil = %v, want nil", got)
	}
	if _, ok := c.Service("x"); ok {
		t.Error("Service on nil reported found")
	}
	if _, ok := c.LookupCheck("x"); ok {
		t.Error("LookupCheck on nil reported found")
	}
	if _, ok := c.LookupShell("x"); ok {
		t.Error("LookupShell on nil reported found")
	}
}

func TestValidationErrorMessages(t *testing.T) {
	ve := &ValidationError{Path: "/repo/mabo-ctl.yaml", Problems: []string{"first", "second"}}
	msg := ve.Error()
	for _, want := range []string{"/repo/mabo-ctl.yaml", "2 problems", "first", "second"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, missing %q", msg, want)
		}
	}
	one := &ValidationError{Path: "p", Problems: []string{"only"}}
	if !strings.Contains(one.Error(), "1 problem)") {
		t.Errorf("singular form expected, got %q", one.Error())
	}

	got := ve.Messages()
	got[0] = "mutated"
	if ve.Problems[0] != "first" {
		t.Error("Messages must return a copy")
	}
}

// TestExampleConfigIsValid loads the shipped examples/mabo-ctl.yaml against a
// temporary tree that has the directories it declares. The file users copy has
// to survive its own validator.
func TestExampleConfigIsValid(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "examples", "mabo-ctl.yaml"))
	if err != nil {
		t.Fatalf("read the shipped example: %v", err)
	}

	root := t.TempDir()
	mkdirs(t, root, "website", "frontend", "backend", "browser-service")
	path := write(t, root, string(body))

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("the shipped example must be valid: %v", err)
	}

	wantNames := []string{"website", "frontend", "backend", "browser", "worker"}
	got := cfg.Names()
	if strings.Join(got, ",") != strings.Join(wantNames, ",") {
		t.Errorf("Names() = %v, want %v", got, wantNames)
	}
	if len(cfg.Checks) != 2 {
		t.Errorf("want 2 checks, got %d", len(cfg.Checks))
	}
	if len(cfg.Shells) != 2 {
		t.Errorf("want 2 shells, got %d", len(cfg.Shells))
	}
	worker, ok := cfg.Service("worker")
	if !ok {
		t.Fatal("worker missing")
	}
	if worker.Port != 0 {
		t.Errorf("worker must be portless, got port %d", worker.Port)
	}
	if len(worker.DependsOn) != 1 || worker.DependsOn[0] != "backend" {
		t.Errorf("worker depends_on = %v, want [backend]", worker.DependsOn)
	}
}

func TestDirEscapingViaSymlinkIsRejected(t *testing.T) {
	outside := t.TempDir()
	root := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := Load(write(t, root, `
services:
  - name: backend
    dir: escape
    cmd: [echo, a]
`))
	problems := validationProblems(t, err)
	if !strings.Contains(strings.Join(problems, "\n"), "symlink") {
		t.Fatalf("a symlink out of the root must be rejected, got: %v", problems)
	}
}

func TestLoadRejectsMultipleDocuments(t *testing.T) {
	root := t.TempDir()
	_, err := Load(write(t, root, okService+"---\n"+okService))
	if err == nil {
		t.Fatal("a second YAML document must not be silently ignored")
	}
	if !strings.Contains(err.Error(), "more than one YAML document") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// TestDiscoverStopsAtTheRepoBoundary is the regression test for a real attack
// path, not a hypothetical one.
//
// The walk used to run to the filesystem root. Every command loads a config and
// most of them EXECUTE what it declares, so a mabo-ctl.yaml planted in any
// ancestor — $HOME, /tmp, a shared parent of several checkouts — silently became
// the config for every project beneath it. Running a bare `mabo-ctl` in a deep
// subdirectory ran that file's commands, and no command except `mabo-ctl config`
// ever named the file it got them from.
func TestDiscoverStopsAtTheRepoBoundary(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// An unrelated mabo-ctl.yaml above the repo — the planted one.
	write(t, root, okService)

	repo := filepath.Join(root, "repo")
	deep := filepath.Join(repo, "child", "deep")
	mkdirs(t, repo, ".git")
	mkdirs(t, deep)

	_, _, err := Discover(deep)
	if err == nil {
		t.Fatal("Discover crossed out of the repository and loaded a parent's mabo-ctl.yaml")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	// The refusal has to teach, or the user's next move is to guess.
	for _, want := range []string{"top of this repository", "--config"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestDiscoverStillFindsAConfigBesideTheRepoMarker pins the other half. The
// boundary is checked AFTER the directory is searched, because a mabo-ctl.yaml
// sitting next to .git is the normal layout — bounding the walk must not break
// the case the walk exists for.
func TestDiscoverStillFindsAConfigBesideTheRepoMarker(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	mkdirs(t, repo, ".git")
	write(t, repo, okService)

	deep := filepath.Join(repo, "a", "b", "c")
	mkdirs(t, deep)

	cfg, _, err := Discover(deep)
	if err != nil {
		t.Fatalf("Discover from a subdirectory of the repo: %v", err)
	}
	if filepath.Dir(cfg.Path) != repo {
		t.Errorf("loaded %s, want the mabo-ctl.yaml at the repo root %s", cfg.Path, repo)
	}
}

// TestDiscoverPathAgreesWithDiscover keeps the two walks from drifting.
// DiscoverPath exists so `config --raw` can print a file that does not parse; if
// it walked further than Discover, mabo-ctl would NAME one file and EXECUTE
// another, which is worse than either bug alone.
func TestDiscoverPathAgreesWithDiscover(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, okService)
	repo := filepath.Join(root, "repo")
	deep := filepath.Join(repo, "x")
	mkdirs(t, repo, ".git")
	mkdirs(t, deep)

	_, _, discoverErr := Discover(deep)
	_, _, pathErr := DiscoverPath(deep)
	if (discoverErr == nil) != (pathErr == nil) {
		t.Fatalf("Discover err=%v but DiscoverPath err=%v — the two walks disagree", discoverErr, pathErr)
	}
}

// TestDiscoverRefusesAConfigClimbedToInAWorldWritableDirectory is the
// regression test for the half of the boundary fix the first attempt missed.
//
// Stopping AT a repo marker or $HOME only bounds a climb that reaches one. A
// tree with neither — /tmp, /opt, a mounted volume, a CI checkout without .git
// — had no boundary, so the search ran to the filesystem root. Refusing to
// climb at all there was tried first and broke the feature: a project unpacked
// into /opt/myapp is not a repository and still wants mabo-ctl to work from a
// subdirectory. The limit that actually separates "my project root" from "a
// file somebody dropped in a shared parent" is who can write the directory.
func TestDiscoverRefusesAConfigClimbedToInAWorldWritableDirectory(t *testing.T) {
	root := t.TempDir()
	write(t, root, okService)
	if err := os.Chmod(root, 0o777); err != nil { // the /tmp shape
		t.Fatalf("chmod: %v", err)
	}
	mkdirs(t, root, "child")
	child := filepath.Join(root, "child")

	_, _, err := Discover(child)
	if err == nil {
		t.Fatal("Discover climbed into a world-writable directory and trusted the config it found there")
	}
	for _, want := range []string{"writable by group or world", "--config"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	// Standing IN that directory is still allowed: being there is a decision,
	// the same one make or npm run acts on. Only the climb is refused.
	if _, _, err := Discover(root); err != nil {
		t.Errorf("Discover in the directory itself: %v", err)
	}
}

// TestDiscoverStillClimbsToAPrivateDirectory pins that the check narrows the
// dangerous case only. An ordinary project directory — owned by you, not group
// or world writable — is climbed to exactly as before, repository or not.
func TestDiscoverStillClimbsToAPrivateDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, okService)
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	mkdirs(t, root, "a/b")

	cfg, _, err := Discover(filepath.Join(root, "a", "b"))
	if err != nil {
		t.Fatalf("Discover from a subdirectory of a private project: %v", err)
	}
	if filepath.Dir(cfg.Path) != root {
		t.Errorf("loaded %s, want %s", cfg.Path, root)
	}
}

// ---------------------------------------------------------------------------
// env_file
// ---------------------------------------------------------------------------

// TestParseEnvFile covers the file grammar: comments, blanks, first-= splits,
// later-line overrides, and an error that names the offending line.
func TestParseEnvFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	body := "# comment\n\nA=1\nB = spaced value \nA=overrides\nEMPTY=\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ParseEnvFile(path)
	if err != nil {
		t.Fatalf("ParseEnvFile: %v", err)
	}
	want := map[string]string{"A": "overrides", "B": "spaced value", "EMPTY": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	bad := filepath.Join(dir, "bad.env")
	if err := os.WriteFile(bad, []byte("GOOD=1\nNOTAPAIR\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseEnvFile(bad); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("err = %v, want a failure naming line 2", err)
	}
}

// TestEnvFileValidation holds env_file entries to the same rules as inline
// env: traversal outside the root, a malformed line, a bad variable name and a
// broken template are all load-time problems.
func TestEnvFileValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		envFile string
		want    string
	}{
		{"escapes the root", "../../etc/secrets", "outside the project root"},
		{"missing file", "absent.env", "no such file"},
		{"malformed line", "broken.env", "not KEY=VALUE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.envFile == "broken.env" {
				if err := os.WriteFile(filepath.Join(root, "broken.env"), []byte("NOTAPAIR\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			body := "services:\n  - name: backend\n    cmd: [echo, hi]\n    env_file: " + tc.envFile + "\n"
			_, err := Load(write(t, root, body))
			var ve *ValidationError
			if !errors.As(err, &ve) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a ValidationError naming %q", err, tc.want)
			}
		})
	}

	t.Run("bad key and broken template in the file", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "svc.env"),
			[]byte("=NOVALUE\nTEMPLATE={{oops\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		body := "services:\n  - name: backend\n    cmd: [echo, hi]\n    env_file: svc.env\n"
		_, err := Load(write(t, root, body))
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("err = %v, want a ValidationError", err)
		}
		msg := err.Error()
		if !(strings.Contains(msg, "invalid env variable name") || strings.Contains(msg, "empty variable name")) ||
			!strings.Contains(msg, "env_file") {
			t.Errorf("the bad key was not reported as an env_file problem:\n%s", msg)
		}
		if !strings.Contains(msg, "is not a valid template") {
			t.Errorf("the broken template was not reported:\n%s", msg)
		}
	})
}

// TestEnvFileAbsentMeansNoEnv: a service without env_file behaves exactly as
// before — the field's absence is not an error and changes nothing.
func TestEnvFileAbsentMeansNoEnv(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg, err := Load(write(t, root, okService))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Services[0].EnvFile != "" {
		t.Errorf("EnvFile = %q, want empty", cfg.Services[0].EnvFile)
	}
}

// TestPerServiceReadyTimeout: the service-level key parses, an absent key
// means inherit, and a negative value is a load-time error.
func TestPerServiceReadyTimeout(t *testing.T) {
	t.Parallel()
	t.Run("parses and inherits", func(t *testing.T) {
		root := t.TempDir()
		body := `
services:
  - name: slow
    cmd: [echo, hi]
    ready_timeout: 2m
  - name: quick
    cmd: [echo, hi]
`
		cfg, err := Load(write(t, root, body))
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Services[0].ReadyTimeout; got != 2*time.Minute {
			t.Errorf("slow ReadyTimeout = %s, want 2m", got)
		}
		if got := cfg.Services[1].ReadyTimeout; got != 0 {
			t.Errorf("quick ReadyTimeout = %s, want 0 (inherit the global)", got)
		}
	})

	t.Run("negative is rejected", func(t *testing.T) {
		root := t.TempDir()
		body := `
services:
  - name: slow
    cmd: [echo, hi]
    ready_timeout: -5s
`
		_, err := Load(write(t, root, body))
		var ve *ValidationError
		if !errors.As(err, &ve) || !strings.Contains(err.Error(), "negative") {
			t.Fatalf("err = %v, want a negative-duration problem", err)
		}
	})

	t.Run("bare seconds accepted like the global", func(t *testing.T) {
		root := t.TempDir()
		body := `
services:
  - name: slow
    cmd: [echo, hi]
    ready_timeout: 45
`
		cfg, err := Load(write(t, root, body))
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Services[0].ReadyTimeout; got != 45*time.Second {
			t.Errorf("ReadyTimeout = %s, want 45s", got)
		}
	})
}

// health

func TestHealthMappingForms(t *testing.T) {
	t.Run("tcp", func(t *testing.T) {
		path := write(t, t.TempDir(), `
services:
  - name: backend
    cmd: [echo, hi]
    port: 7102
    health:
      tcp: localhost:{{.Port}}
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		h := cfg.Services[0].Health
		if h.Kind != "tcp" || h.Addr != "localhost:{{.Port}}" || h.HTTP != "" || len(h.Argv) != 0 {
			t.Errorf("Health = %+v, want kind tcp with the RAW addr", h)
		}
	})
	t.Run("exec", func(t *testing.T) {
		path := write(t, t.TempDir(), `
services:
  - name: db
    cmd: [postgres]
    port: 5432
    health:
      exec: [pg_isready, -h, localhost, "-p", "{{.Port}}"]
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		h := cfg.Services[0].Health
		if h.Kind != "exec" {
			t.Fatalf("Kind = %q, want exec", h.Kind)
		}
		want := []string{"pg_isready", "-h", "localhost", "-p", "{{.Port}}"}
		if !slicesEqual(h.Argv, want) {
			t.Errorf("Argv = %#v, want %#v (raw, unexpanded)", h.Argv, want)
		}
	})
	t.Run("explicit http key equals the scalar form", func(t *testing.T) {
		a, err := Load(write(t, t.TempDir(), okService+"\n    health: http://x/healthz\n"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := Load(write(t, t.TempDir(), okService+"\n    health:\n      http: http://x/healthz\n"))
		if err != nil {
			t.Fatal(err)
		}
		if a.Services[0].Health.Kind != b.Services[0].Health.Kind ||
			a.Services[0].Health.HTTP != b.Services[0].Health.HTTP {
			t.Errorf("scalar %+v and mapping %+v must decode identically", a.Services[0].Health, b.Services[0].Health)
		}
	})
}

func TestHealthMappingErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"two kinds", okService + "\n    health:\n      http: http://x/\n      tcp: localhost:5432\n",
			"exactly one"},
		{"unknown key", okService + "\n    health:\n      grpc: localhost:9000\n", "unknown health key"},
		{"empty mapping", okService + "\n    health: {}\n", "exactly one"},
		{"sequence", okService + "\n    health: [http://x/]\n", "must be a URL or a mapping"},
		{"exec not a list", okService + "\n    health:\n      exec: pg_isready\n", "argv list"},
		{"exec empty list", okService + "\n    health:\n      exec: []\n", "declare the command"},
		{"tcp empty", okService + "\n    health:\n      tcp: \"\"\n", "non-empty string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, t.TempDir(), tc.body))
			if err == nil {
				t.Fatal("expected a load error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestHealthValidationRules(t *testing.T) {
	t.Run("bad tcp address is a load-time problem", func(t *testing.T) {
		_, err := Load(write(t, t.TempDir(), okService+"\n    health:\n      tcp: localhost:nope\n"))
		got := validationProblems(t, err)
		if len(got) == 0 || !strings.Contains(got[0], "invalid port") {
			t.Errorf("problems = %v, want an invalid-port message", got)
		}
	})
	t.Run("own-port rule reaches tcp and exec parts", func(t *testing.T) {
		_, err := Load(write(t, t.TempDir(), `
services:
  - name: worker
    cmd: [run]
    health:
      exec: [check, "--port", "{{.Port}}"]
`))
		got := validationProblems(t, err)
		found := false
		for _, p := range got {
			if strings.Contains(p, "declares no port") {
				found = true
			}
		}
		if !found {
			t.Errorf("problems = %v, want the own-{{.Port}} rule for the exec argv", got)
		}
	})
	t.Run("another service's port in exec is fine", func(t *testing.T) {
		body := `
services:
  - name: worker
    cmd: [run]
    health:
      exec: [check, "--port", "{{.Port \"backend\"}}"]
  - name: backend
    cmd: [serve]
    port: 7102
`
		if _, err := Load(write(t, t.TempDir(), body)); err != nil {
			t.Errorf("a cross-service {{.Port}} reference must be legal in exec: %v", err)
		}
	})
}

// slicesEqual is a tiny local helper so this file does not import slices for
// one call.
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// expect: on preflight checks

func TestCheckExpectRoundTripsAndValidates(t *testing.T) {
	ok := `services:
  - name: api
    cmd: [echo, hi]
checks:
  - name: port-free
    tcp: localhost:7100
    expect: free
  - name: db
    tcp: localhost:5432
`
	if _, err := Load(write(t, t.TempDir(), ok)); err != nil {
		t.Fatalf("expect: free / default listening failed to load: %v", err)
	}

	bad := []struct{ name, body, want string }{
		{"nonsense value", ok + "  - name: x\n    command: [true]\n    expect: closed\n", `must be "listening"`},
		{"on a command check", `services:
  - name: api
    cmd: [echo, hi]
checks:
  - name: builds
    command: [true]
    expect: free
`, "only applies to a tcp:"},
	}
	for _, tc := range bad {
		_, err := Load(write(t, t.TempDir(), tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.want)
		}
	}
}
