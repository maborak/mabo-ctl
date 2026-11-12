package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maborak/mabo-ctl/internal/config"
)

// importInto writes name with body into a fresh directory, runs
// `init --from name` there, and returns the directory and the exit code.
func importInto(t *testing.T, name, body string) (string, int, *harness) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarnessAt(t, dir, "init", "--from", name)
	return dir, h.run(), h
}

func TestInitFromProcfileWritesLiveValidServices(t *testing.T) {
	dir, code, h := importInto(t, "Procfile",
		"web: bundle exec rails s -p $PORT\n# a comment\nworker: bundle exec sidekiq\n")
	if code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr)
	}
	cfg, err := config.Load(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatalf("the imported file does not load: %v", err)
	}
	if len(cfg.Services) != 2 {
		t.Fatalf("imported %d services, want 2", len(cfg.Services))
	}
	web := cfg.Services[0]
	if web.Name != "web" || web.Port != 5000 || strings.Join(web.Cmd, " ") != "sh -c bundle exec rails s -p $PORT" {
		t.Errorf("web = %+v, want sh -c with foreman's port 5000", web)
	}
	if cfg.Services[1].Port != 0 {
		t.Errorf("worker reads no $PORT but got port %d", cfg.Services[1].Port)
	}
	body, _ := os.ReadFile(filepath.Join(dir, config.FileName))
	if strings.Contains(string(body), dir) {
		t.Error("the imported file records the absolute path of its source")
	}
}

func TestInitFromProcessComposeMapsDependenciesAndProbes(t *testing.T) {
	dir, code, h := importInto(t, "process-compose.yaml", `
processes:
  db:
    command: "postgres"
    readiness_probe:
      http_get: {host: 127.0.0.1, port: 5432, path: /health}
  api:
    command: "uvicorn app:app"
    environment: ["DEBUG=true"]
    depends_on:
      db: {condition: process_healthy}
`)
	if code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr)
	}
	cfg, err := config.Load(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatalf("the imported file does not load: %v", err)
	}
	db, api := cfg.Services[0], cfg.Services[1]
	if db.Port != 5432 || !strings.Contains(db.Health.HTTP, "127.0.0.1:{{.Port}}/health") {
		t.Errorf("db = port %d health %q", db.Port, db.Health.HTTP)
	}
	if strings.Join(api.DependsOn, ",") != "db" || strings.Join(api.DependsReadyOn, ",") != "db" || api.Env["DEBUG"] != "true" {
		t.Errorf("api = %+v, want depends_on/depends_ready_on db and DEBUG=true", api)
	}
}

// TestInitFromNeutralisesInjection: a process file is repo content, and its
// names and commands must not become extra YAML or a template mabo-ctl expands.
func TestInitFromNeutralisesInjection(t *testing.T) {
	dir, code, h := importInto(t, "process-compose.yaml", `
processes:
  "evil\n  - name: pwned":
    command: "echo hi\n  - name: pwned2\n    cmd: [touch, /tmp/mabo-pwned]"
  tmpl:
    command: "echo {{.Port}}"
`)
	if code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr)
	}
	cfg, err := config.Load(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatalf("the imported file does not load: %v", err)
	}
	if len(cfg.Services) != 1 || cfg.Services[0].Name != "evil-namepwned" || cfg.Services[0].Cmd[0] != "sh" {
		names := []string{}
		for _, s := range cfg.Services {
			names = append(names, s.Name)
		}
		t.Fatalf("live services = %v, want only the sanitised evil-namepwned, run through sh -c", names)
	}
	body, _ := os.ReadFile(filepath.Join(dir, config.FileName))
	if !strings.Contains(string(body), "# tmpl: not imported live") {
		t.Errorf("the {{ command was not written commented out:\n%s", body)
	}
}

func TestInitFromRefusesAnUnknownFile(t *testing.T) {
	_, code, _ := importInto(t, "docker-compose.yml", "services: {}\n")
	if code != exitUsage {
		t.Errorf("exit %d, want %d for a file that is neither format", code, exitUsage)
	}
}
