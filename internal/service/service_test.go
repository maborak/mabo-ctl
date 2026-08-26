package service

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/state"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// testRepo creates a repository root and a directory holding one executable
// named "runme", and points PATH at it so the "system" runtime resolves.
func testRepo(t *testing.T) (root, bin string) {
	t.Helper()
	root = t.TempDir()
	bin = t.TempDir()
	writeExecutable(t, filepath.Join(bin, "runme"))
	t.Setenv("PATH", bin)
	return root, bin
}

// unsetEnv removes key for the duration of the test and restores whatever the
// process had afterwards, which os.Unsetenv alone would not do.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "") // registers restoration of the original value
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
}

// writeExecutable creates a small executable file at path.
func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// testConfig builds a Config rooted at root, creating each declared directory.
func testConfig(t *testing.T, root string, specs ...config.Spec) *config.Config {
	t.Helper()
	for _, s := range specs {
		if s.Dir == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Join(root, s.Dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", s.Dir, err)
		}
	}
	return &config.Config{
		Root:     root,
		Path:     filepath.Join(root, config.FileName),
		Services: specs,
	}
}

// svc is a minimal runnable spec.
func svc(name string, port int) config.Spec {
	return config.Spec{Name: name, Port: port, Cmd: []string{"runme"}}
}

// testState creates a .dev state directory under root, optionally seeded with
// persisted ports.
func testState(t *testing.T, root string, ports map[string]int) *state.Dir {
	t.Helper()
	st, err := state.New(root)
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	if ports != nil {
		re := &state.RunEnv{Ports: ports}
		if err := st.WriteRunEnv(re); err != nil {
			t.Fatalf("WriteRunEnv: %v", err)
		}
	}
	return st
}

// originFor returns the Origin of the named service.
func originFor(t *testing.T, origins []Origin, name string) Origin {
	t.Helper()
	for _, o := range origins {
		if o.Service == name {
			return o
		}
	}
	t.Fatalf("no origin for %q in %+v", name, origins)
	return Origin{}
}

// instFor returns the Instance of the named service.
func instFor(t *testing.T, insts []Instance, name string) Instance {
	t.Helper()
	for _, in := range insts {
		if in.Name == name {
			return in
		}
	}
	t.Fatalf("no instance for %q", name)
	return Instance{}
}

// envValue reads a KEY=VALUE slice.
func envValue(env []string, key string) (string, bool) {
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// names lists instance names in order.
func names(insts []Instance) []string {
	out := make([]string, 0, len(insts))
	for _, in := range insts {
		out = append(out, in.Name)
	}
	return out
}

// ---------------------------------------------------------------------------
// port precedence
// ---------------------------------------------------------------------------

// TestResolvePortPrecedence exercises all four precedence levels against one
// config, so a change to any level is visible against the others.
func TestResolvePortPrecedence(t *testing.T) {
	cases := []struct {
		name       string
		opt        Options
		persisted  map[string]int
		wantPort   int
		wantSource PortSource
	}{
		{
			name:       "flag beats everything",
			opt:        Options{Ports: []int{0, 7999}, EnvVars: map[string]string{"BACKEND_PORT": "7500"}},
			persisted:  map[string]int{"backend": 7600},
			wantPort:   7999,
			wantSource: FromFlag,
		},
		{
			name:       "caller env beats run.env and default",
			opt:        Options{EnvVars: map[string]string{"BACKEND_PORT": "7500"}},
			persisted:  map[string]int{"backend": 7600},
			wantPort:   7500,
			wantSource: FromEnv,
		},
		{
			name:       "run.env beats the declared default",
			persisted:  map[string]int{"backend": 7600},
			wantPort:   7600,
			wantSource: FromRunEnv,
		},
		{
			name:       "declared default when nothing else is set",
			wantPort:   7102,
			wantSource: FromDefault,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := testRepo(t)
			cfg := testConfig(t, root, svc("website", 7100), svc("backend", 7102))
			st := testState(t, root, tc.persisted)

			insts, origins, err := Resolve(cfg, st, tc.opt)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			// One origin per service, in declaration order.
			if len(origins) != len(cfg.Services) {
				t.Fatalf("got %d origins, want one per service (%d)", len(origins), len(cfg.Services))
			}
			for i, o := range origins {
				if o.Service != cfg.Services[i].Name {
					t.Fatalf("origins[%d] is %q, want %q: origins must be in declaration order",
						i, o.Service, cfg.Services[i].Name)
				}
			}
			o := originFor(t, origins, "backend")
			if o.Port != tc.wantPort || o.Source != tc.wantSource {
				t.Errorf("backend resolved to %d from %q, want %d from %q",
					o.Port, o.Source, tc.wantPort, tc.wantSource)
			}
			if o.Declared != 7102 {
				t.Errorf("Declared = %d, want 7102", o.Declared)
			}
			if got := instFor(t, insts, "backend").Port; got != tc.wantPort {
				t.Errorf("instance port = %d, want %d", got, tc.wantPort)
			}
			// The untouched service always falls through to its default.
			if w := originFor(t, origins, "website"); w.Port != 7100 || w.Source != FromDefault {
				t.Errorf("website = %d from %q, want 7100 from default", w.Port, w.Source)
			}
		})
	}
}

// TestOriginOverrideOnChangedDefault is the documented trap: the declared
// default moved and .dev/run.env still holds the old value. The persisted value
// still wins, but Override must make that visible.
func TestOriginOverrideOnChangedDefault(t *testing.T) {
	root, _ := testRepo(t)
	// The default was changed from 7002 to 7102; run.env still holds 7002.
	cfg := testConfig(t, root, svc("backend", 7102))
	st := testState(t, root, map[string]int{"backend": 7002})

	_, origins, err := Resolve(cfg, st, Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	o := originFor(t, origins, "backend")
	if o.Source != FromRunEnv || o.Port != 7002 {
		t.Fatalf("got %d from %q, want 7002 from run.env", o.Port, o.Source)
	}
	if o.Declared != 7102 {
		t.Errorf("Declared = %d, want 7102", o.Declared)
	}
	if !o.Override {
		t.Error("Override = false; a persisted port that differs from the declared default must be flagged")
	}
}

// TestIgnoreRunEnvLetsTheDeclaredDefaultWin is the --refresh-ports switch: the
// same stale-state trap as TestOriginOverrideOnChangedDefault, resolved by
// dropping the run.env level for this one resolution instead of by hand-deleting
// the file. Higher levels keep their ranks — a flag or caller variable still
// beats the declared default.
func TestIgnoreRunEnvLetsTheDeclaredDefaultWin(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root, svc("website", 7100), svc("backend", 7102))
	st := testState(t, root, map[string]int{"backend": 7002, "website": 7001})

	insts, origins, err := Resolve(cfg, st, Options{IgnoreRunEnv: true})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, svcName := range []string{"backend", "website"} {
		o := originFor(t, origins, svcName)
		if o.Source != FromDefault {
			t.Errorf("%s resolved from %q, want default — IgnoreRunEnv must skip the run.env level", svcName, o.Source)
		}
		if o.Override {
			t.Errorf("%s: Override = true with run.env skipped; there is nothing to override", svcName)
		}
	}
	if got := instFor(t, insts, "backend").Port; got != 7102 {
		t.Errorf("backend port = %d, want the declared 7102", got)
	}
	if got := instFor(t, insts, "website").Port; got != 7100 {
		t.Errorf("website port = %d, want the declared 7100", got)
	}

	// A caller variable still outranks the declared default with run.env skipped.
	_, origins, err = Resolve(cfg, st, Options{IgnoreRunEnv: true, EnvVars: map[string]string{"BACKEND_PORT": "7500"}})
	if err != nil {
		t.Fatalf("Resolve with caller env: %v", err)
	}
	if o := originFor(t, origins, "backend"); o.Source != FromEnv || o.Port != 7500 {
		t.Errorf("backend = %d from %q, want 7500 from env", o.Port, o.Source)
	}
}

// TestOriginNoOverrideWhenPersistedMatchesDefault keeps Override meaningful:
// run.env agreeing with the default is not an override and must not print.
func TestOriginNoOverrideWhenPersistedMatchesDefault(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root, svc("backend", 7102))
	st := testState(t, root, map[string]int{"backend": 7102})

	_, origins, err := Resolve(cfg, st, Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	o := originFor(t, origins, "backend")
	if o.Source != FromRunEnv {
		t.Fatalf("Source = %q, want run.env", o.Source)
	}
	if o.Override {
		t.Error("Override = true for a persisted port equal to the declared default")
	}
}

// TestOriginOverrideOnlyForRunEnv pins the contract: Override is a statement
// about stale persisted state, not about any port that differs from the default.
func TestOriginOverrideOnlyForRunEnv(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root, svc("backend", 7102))
	st := testState(t, root, nil)

	_, origins, err := Resolve(cfg, st, Options{Ports: []int{7999}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	o := originFor(t, origins, "backend")
	if o.Source != FromFlag || o.Port != 7999 {
		t.Fatalf("got %d from %q, want 7999 from flag", o.Port, o.Source)
	}
	if o.Override {
		t.Error("Override = true for a flag override; it must only describe run.env")
	}
}

// TestStalePersistedPortDoesNotResurrectAPortlessService: a service whose port
// was removed from mabo-ctl.yaml must stay portless. Stale state may outrank a
// changed default, but it may not invent a port the config no longer declares.
func TestStalePersistedPortDoesNotResurrectAPortlessService(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root, svc("worker", 0))
	st := testState(t, root, map[string]int{"worker": 7104})

	_, origins, err := Resolve(cfg, st, Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	o := originFor(t, origins, "worker")
	if o.Port != 0 || o.Source != FromDefault {
		t.Errorf("worker = %d from %q, want 0 from default", o.Port, o.Source)
	}
}

// TestCallerEnvDoesNotInventAPortForAPortlessService is the caller-env twin of
// TestStalePersistedPortDoesNotResurrectAPortlessService, and it is here because
// only the run.env branch had the guard.
//
// A stray WORKER_PORT in the developer's shell gave a portless worker a port.
// That port then reached the --json contract and, through `reset --force`, told
// mabo-ctl to kill whoever held it — so mabo-ctl signalled a process it never
// started, on a port no service declared.
func TestCallerEnvDoesNotInventAPortForAPortlessService(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root, svc("worker", 0))

	_, origins, err := Resolve(cfg, testState(t, root, nil), Options{
		EnvVars: map[string]string{"WORKER_PORT": "7987"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	o := originFor(t, origins, "worker")
	if o.Port != 0 || o.Source != FromDefault {
		t.Errorf("worker = %d from %q, want 0 from default: a caller variable must not "+
			"give a port to a service that declares none", o.Port, o.Source)
	}
}

// TestCallerEnvForAPortlessServiceIsIgnoredNotRejected pins the deliberate half
// of the guard above. An unparseable value is an error for a service that HAS a
// port, but for a portless one the variable is simply not that service's to
// read — and erroring would make mabo-ctl unusable in any shell that exports the
// name for something else, which is exactly how the bug was triggered.
func TestCallerEnvForAPortlessServiceIsIgnoredNotRejected(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root, svc("worker", 0))

	_, origins, err := Resolve(cfg, testState(t, root, nil), Options{
		EnvVars: map[string]string{"WORKER_PORT": "not-a-port"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if o := originFor(t, origins, "worker"); o.Port != 0 {
		t.Errorf("worker = %d, want 0", o.Port)
	}
}

// TestResolveRejectsUnparseableCallerPort: silently ignoring an explicit
// BACKEND_PORT=abc would leave the supervisor reporting something untrue.
func TestResolveRejectsUnparseableCallerPort(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root, svc("backend", 7102))

	_, _, err := Resolve(cfg, testState(t, root, nil), Options{
		EnvVars: map[string]string{"BACKEND_PORT": "abc"},
	})
	if err == nil {
		t.Fatal("expected an error for BACKEND_PORT=abc")
	}
	for _, want := range []string{"BACKEND_PORT", "abc", "backend"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestResolveBlankCallerPortFallsThrough treats BACKEND_PORT= as unset.
func TestResolveBlankCallerPortFallsThrough(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root, svc("backend", 7102))

	_, origins, err := Resolve(cfg, testState(t, root, nil), Options{
		EnvVars: map[string]string{"BACKEND_PORT": "  "},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if o := originFor(t, origins, "backend"); o.Source != FromDefault || o.Port != 7102 {
		t.Errorf("got %d from %q, want 7102 from default", o.Port, o.Source)
	}
}

// TestPortSlotsSkipPortlessServices pins the meaning of a positional --ports
// slot: it addresses the i-th service that DECLARES a port, so a portless
// service declared in the middle does not shift the mapping.
func TestPortSlotsSkipPortlessServices(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root,
		svc("website", 7100),
		svc("worker", 0),
		svc("backend", 7102),
	)
	if got := PortSlotNames(cfg); strings.Join(got, ",") != "website,backend" {
		t.Fatalf("PortSlotNames = %v, want [website backend]", got)
	}

	_, origins, err := Resolve(cfg, testState(t, root, nil), Options{Ports: []int{0, 8102}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if o := originFor(t, origins, "backend"); o.Port != 8102 || o.Source != FromFlag {
		t.Errorf("backend = %d from %q, want 8102 from flag", o.Port, o.Source)
	}
	if o := originFor(t, origins, "website"); o.Port != 7100 {
		t.Errorf("website = %d, want 7100 (an empty slot keeps the default)", o.Port)
	}
	if o := originFor(t, origins, "worker"); o.Port != 0 {
		t.Errorf("worker = %d, want 0", o.Port)
	}
}

// TestPortSlotsTooMany reports a --ports list longer than the number of ported
// services instead of silently dropping the extras.
func TestPortSlotsTooMany(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root, svc("website", 7100))

	_, _, err := Resolve(cfg, testState(t, root, nil), Options{Ports: []int{1, 2}})
	if err == nil {
		t.Fatal("expected an error for more --ports values than ported services")
	}
	if !strings.Contains(err.Error(), "website") {
		t.Errorf("error %q does not name the services a slot maps to", err)
	}
}

// TestPortSlotOutOfRange rejects a nonsense port rather than passing it on.
func TestPortSlotOutOfRange(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root, svc("website", 7100))

	_, _, err := Resolve(cfg, testState(t, root, nil), Options{Ports: []int{70000}})
	if err == nil || !strings.Contains(err.Error(), "70000") {
		t.Fatalf("got %v, want an out-of-range error naming 70000", err)
	}
}

// TestResolveWithoutStateDir skips the persisted level rather than failing.
func TestResolveWithoutStateDir(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root, svc("backend", 7102))

	_, origins, err := Resolve(cfg, nil, Options{})
	if err != nil {
		t.Fatalf("Resolve with a nil state dir: %v", err)
	}
	if o := originFor(t, origins, "backend"); o.Source != FromDefault {
		t.Errorf("Source = %q, want default", o.Source)
	}
}

// ---------------------------------------------------------------------------
// CaptureEnv
// ---------------------------------------------------------------------------

// TestCaptureEnvUnsets is the whole reason CaptureEnv exists: a value left in
// the environment is inherited by every child.
func TestCaptureEnvUnsets(t *testing.T) {
	t.Setenv("BACKEND_PORT", "7102")
	t.Setenv("BROWSER_SERVICE_PORT", "7103")
	unsetEnv(t, "WEBSITE_PORT")

	got := CaptureEnv([]string{"backend", "browser-service", "website"})

	if got["BACKEND_PORT"] != "7102" {
		t.Errorf("captured BACKEND_PORT = %q, want 7102", got["BACKEND_PORT"])
	}
	if got["BROWSER_SERVICE_PORT"] != "7103" {
		t.Errorf("captured BROWSER_SERVICE_PORT = %q, want 7103", got["BROWSER_SERVICE_PORT"])
	}
	if _, ok := got["WEBSITE_PORT"]; ok {
		t.Error("captured WEBSITE_PORT, which was never set")
	}
	if v, ok := os.LookupEnv("BACKEND_PORT"); ok {
		t.Errorf("BACKEND_PORT is still set to %q after CaptureEnv; a child would inherit it", v)
	}
	if v, ok := os.LookupEnv("BROWSER_SERVICE_PORT"); ok {
		t.Errorf("BROWSER_SERVICE_PORT is still set to %q after CaptureEnv", v)
	}
}

// TestPortEnvVar pins the variable-name mapping the CLI and the tests share.
func TestPortEnvVar(t *testing.T) {
	cases := map[string]string{
		"backend":         "BACKEND_PORT",
		"browser-service": "BROWSER_SERVICE_PORT",
		"Web2":            "WEB2_PORT",
	}
	for in, want := range cases {
		if got := PortEnvVar(in); got != want {
			t.Errorf("PortEnvVar(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestChildDoesNotSeeCallerPort is the bug end-to-end: the caller exported
// BACKEND_PORT=7102, the supervisor resolved 7999, and the child must see 7999.
func TestChildDoesNotSeeCallerPort(t *testing.T) {
	root, _ := testRepo(t)
	t.Setenv("BACKEND_PORT", "7102")

	captured := CaptureEnv([]string{"backend"})
	cfg := testConfig(t, root, svc("backend", 7102))

	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{
		Ports:   []int{7999},
		EnvVars: captured,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, ok := envValue(instFor(t, insts, "backend").Env, "BACKEND_PORT")
	if !ok {
		t.Fatal("BACKEND_PORT is missing from the child environment")
	}
	if got != "7999" {
		t.Errorf("child sees BACKEND_PORT=%s, want 7999 (the resolved port)", got)
	}
}

// ---------------------------------------------------------------------------
// collisions
// ---------------------------------------------------------------------------

// TestCollisionNamesBothServices uses four services, because the predecessor's
// three hardcoded comparisons left three of the six pairs unchecked when a
// fourth service arrived.
func TestCollisionNamesBothServices(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root,
		svc("website", 7100),
		svc("frontend", 7101),
		svc("backend", 7102),
		svc("browser", 7103),
	)
	// The pair the predecessor never compared: the third and fourth services.
	st := testState(t, root, map[string]int{"browser": 7102})

	_, _, err := Resolve(cfg, st, Options{})
	if err == nil {
		t.Fatal("expected a collision error")
	}
	msg := err.Error()
	for _, want := range []string{"backend", "browser", "7102"} {
		if !strings.Contains(msg, want) {
			t.Errorf("collision error %q does not mention %q", msg, want)
		}
	}

	var ce *CollisionError
	if !errors.As(err, &ce) {
		t.Fatalf("error is %T, want *CollisionError", err)
	}
	if len(ce.Collisions) != 1 {
		t.Fatalf("got %d collisions, want 1: %+v", len(ce.Collisions), ce.Collisions)
	}
	if got := strings.Join(ce.Collisions[0].Services, ","); got != "backend,browser" {
		t.Errorf("Services = %q, want backend,browser", got)
	}
	if ce.Collisions[0].Port != 7102 {
		t.Errorf("Port = %d, want 7102", ce.Collisions[0].Port)
	}
}

// TestCollisionAcrossEveryPair walks every pair of four services, which is the
// property a hand-written comparison list cannot have.
func TestCollisionAcrossEveryPair(t *testing.T) {
	all := []string{"a", "b", "c", "d"}
	for i := range all {
		for j := range all {
			if i >= j {
				continue
			}
			t.Run(all[i]+"/"+all[j], func(t *testing.T) {
				root, _ := testRepo(t)
				specs := make([]config.Spec, 0, len(all))
				for k, n := range all {
					specs = append(specs, svc(n, 7100+k))
				}
				cfg := testConfig(t, root, specs...)
				st := testState(t, root, map[string]int{all[j]: 7100 + i})

				_, _, err := Resolve(cfg, st, Options{})
				if err == nil {
					t.Fatalf("no collision reported for %s and %s on port %d", all[i], all[j], 7100+i)
				}
				for _, want := range []string{all[i], all[j]} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not name %q", err, want)
					}
				}
			})
		}
	}
}

// TestPortlessServicesNeverCollide: two services with no port share the value 0
// and must not be reported.
func TestPortlessServicesNeverCollide(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root, svc("worker", 0), svc("scheduler", 0))

	if _, _, err := Resolve(cfg, testState(t, root, nil), Options{}); err != nil {
		t.Fatalf("portless services reported a collision: %v", err)
	}
}

// TestCollisionReturnsOriginsForDisplay: the caller still needs to show where
// each contended port came from.
func TestCollisionReturnsOriginsForDisplay(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root, svc("a", 7100), svc("b", 7101))
	st := testState(t, root, map[string]int{"b": 7100})

	_, origins, err := Resolve(cfg, st, Options{})
	if err == nil {
		t.Fatal("expected a collision error")
	}
	if len(origins) != 2 {
		t.Fatalf("got %d origins alongside the collision error, want 2", len(origins))
	}
	if o := originFor(t, origins, "b"); o.Source != FromRunEnv {
		t.Errorf("b came from %q, want run.env", o.Source)
	}
}

// ---------------------------------------------------------------------------
// template expansion
// ---------------------------------------------------------------------------

// TestTemplateExpansion covers a service's own port and another service's,
// across health, cmd and env — and proves expansion runs after every port
// resolves, since website is declared before the backend it references.
func TestTemplateExpansion(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root,
		config.Spec{
			Name:   "website",
			Port:   7100,
			Health: config.Health{Kind: "http", HTTP: "http://localhost:{{.Port}}/robots.txt"},
			Cmd:    []string{"runme", "--port", "{{.Port}}"},
			Env:    map[string]string{"PUBLIC_API_BASE": `http://localhost:{{.Port "backend"}}`},
		},
		svc("backend", 7102),
	)

	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{Ports: []int{8100, 8102}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	in := instFor(t, insts, "website")
	if in.Health != "http://localhost:8100/robots.txt" {
		t.Errorf("Health = %q, want the resolved own port", in.Health)
	}
	if strings.Join(in.Cmd[1:], " ") != "--port 8100" {
		t.Errorf("Cmd = %v, want --port 8100", in.Cmd)
	}
	if v, _ := envValue(in.Env, "PUBLIC_API_BASE"); v != "http://localhost:8102" {
		t.Errorf("PUBLIC_API_BASE = %q, want http://localhost:8102", v)
	}
}

// TestTemplateUnknownService names the valid services rather than expanding to
// nothing.
func TestTemplateUnknownService(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root,
		config.Spec{Name: "website", Port: 7100, Cmd: []string{"runme", `{{.Port "nope"}}`}},
		svc("backend", 7102),
	)

	_, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err == nil {
		t.Fatal("expected an error for a reference to an unknown service")
	}
	for _, want := range []string{"nope", "website", "backend", "cmd[1]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestTemplateMissingKeyIsAnError: with missingkey=error an unresolvable
// reference fails loudly instead of rendering "<no value>" into a command line.
func TestTemplateMissingKeyIsAnError(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root,
		config.Spec{Name: "website", Port: 7100, Cmd: []string{"runme", "{{.Nope}}"}},
	)

	_, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err == nil {
		t.Fatal("expected an error for an unresolvable template reference")
	}
	if strings.Contains(err.Error(), "<no value>") {
		t.Errorf("error %q rendered <no value> instead of failing", err)
	}
	if !strings.Contains(err.Error(), "Nope") {
		t.Errorf("error %q does not name the offending reference", err)
	}
}

// TestTemplateBrokenSyntax reports the service and the field.
func TestTemplateBrokenSyntax(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root,
		config.Spec{Name: "website", Port: 7100, Health: config.Health{Kind: "http", HTTP: "http://x/{{.Port"}, Cmd: []string{"runme"}},
	)

	_, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err == nil {
		t.Fatal("expected a template parse error")
	}
	for _, want := range []string{"website", "health"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestTemplatePortOfPortlessService refuses to expand to ":0", which would
// produce a URL that can only ever fail.
func TestTemplatePortOfPortlessService(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root,
		svc("worker", 0),
		config.Spec{Name: "website", Port: 7100, Cmd: []string{"runme", `{{.Port "worker"}}`}},
	)

	_, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err == nil || !strings.Contains(err.Error(), "worker") {
		t.Fatalf("got %v, want an error naming the portless service", err)
	}
}

// TestTemplateTooManyArguments rejects {{.Port "a" "b"}}.
func TestTemplateTooManyArguments(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root,
		config.Spec{Name: "a", Port: 7100, Cmd: []string{"runme", `{{.Port "a" "a"}}`}},
	)

	if _, _, err := Resolve(cfg, testState(t, root, nil), Options{}); err == nil {
		t.Fatal("expected an error for two arguments to .Port")
	}
}

// TestTemplateTextWithoutActionsIsUntouched keeps ordinary argv verbatim.
func TestTemplateTextWithoutActionsIsUntouched(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root,
		config.Spec{Name: "a", Port: 7100, Cmd: []string{"runme", "a}}b", "100%"}},
	)

	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	in := instFor(t, insts, "a")
	if in.Cmd[1] != "a}}b" || in.Cmd[2] != "100%" {
		t.Errorf("Cmd = %v, want the arguments unchanged", in.Cmd)
	}
}

// ---------------------------------------------------------------------------
// runtime resolution
// ---------------------------------------------------------------------------

// TestRuntimeSystemResolvesToAbsolutePath keeps Cmd[0] from ever being handed
// to the child as a bare name.
func TestRuntimeSystemResolvesToAbsolutePath(t *testing.T) {
	root, bin := testRepo(t)
	cfg := testConfig(t, root, config.Spec{Name: "a", Port: 7100, Cmd: []string{"runme"}, Runtime: "system"})

	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join(bin, "runme")
	if got := instFor(t, insts, "a").Cmd[0]; got != want {
		t.Errorf("Cmd[0] = %q, want %q", got, want)
	}
}

// TestRuntimeSystemNotFound names the command and the directories searched.
func TestRuntimeSystemNotFound(t *testing.T) {
	root, bin := testRepo(t)
	cfg := testConfig(t, root, config.Spec{Name: "a", Port: 7100, Cmd: []string{"definitely-not-here"}})

	// Resolve SUCCEEDS: a missing interpreter is this machine's problem, not
	// the config's, and must not stop the other services being managed. The
	// failure is carried on the instance and fires when something runs it.
	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	err = instFor(t, insts, "a").Runnable()
	if err == nil {
		t.Fatal("expected a lookup failure")
	}
	for _, want := range []string{"definitely-not-here", bin, "system"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestRuntimeCondaResolves builds a fake conda installation and checks the
// interpreter, the PATH prepend and the conda variables.
func TestRuntimeCondaResolves(t *testing.T) {
	root, _ := testRepo(t)
	base := t.TempDir()
	condaBin := filepath.Join(base, "envs", "app-dev", "bin")
	writeExecutable(t, filepath.Join(condaBin, "python"))
	writeExecutable(t, filepath.Join(base, "bin", "conda"))
	t.Setenv("CONDA_EXE", filepath.Join(base, "bin", "conda"))

	cfg := testConfig(t, root, config.Spec{
		Name: "backend", Port: 7102, Cmd: []string{"python", "app.py"}, Runtime: "conda:app-dev",
	})

	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	in := instFor(t, insts, "backend")
	if want := filepath.Join(condaBin, "python"); in.Cmd[0] != want {
		t.Errorf("Cmd[0] = %q, want %q", in.Cmd[0], want)
	}
	path, _ := envValue(in.Env, "PATH")
	if !strings.HasPrefix(path, condaBin+string(os.PathListSeparator)) {
		t.Errorf("PATH = %q, want it to start with %q; npm-style shebangs re-resolve their interpreter through PATH", path, condaBin)
	}
	if v, _ := envValue(in.Env, "CONDA_DEFAULT_ENV"); v != "app-dev" {
		t.Errorf("CONDA_DEFAULT_ENV = %q, want app-dev", v)
	}
	if v, _ := envValue(in.Env, "CONDA_PREFIX"); v != filepath.Join(base, "envs", "app-dev") {
		t.Errorf("CONDA_PREFIX = %q, want the env prefix", v)
	}
}

// TestRuntimeCondaFailureNamesResolvedPath is spec bug #5: never fall back to
// PATH, and say exactly which path was tried.
func TestRuntimeCondaFailureNamesResolvedPath(t *testing.T) {
	root, bin := testRepo(t)
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "envs", "app-dev", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(base, "bin", "conda"))
	t.Setenv("CONDA_EXE", filepath.Join(base, "bin", "conda"))
	// "runme" IS on PATH; a conda service must not silently use it.
	writeExecutable(t, filepath.Join(bin, "runme"))

	cfg := testConfig(t, root, config.Spec{
		Name: "backend", Port: 7102, Cmd: []string{"runme"}, Runtime: "conda:app-dev",
	})

	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	in := instFor(t, insts, "backend")
	err = in.Runnable()
	if err == nil {
		t.Fatal("expected the conda lookup to fail rather than fall back to PATH")
	}
	// The heart of bug #5: "runme" IS on PATH, so Cmd[0] must NOT have been
	// resolved to it. An unrunnable instance keeps the unresolved name.
	if in.Cmd[0] == filepath.Join(bin, "runme") {
		t.Fatalf("Cmd[0] fell back to the PATH copy %s", in.Cmd[0])
	}
	wantPath := filepath.Join(base, "envs", "app-dev", "bin", "runme")
	for _, want := range []string{wantPath, "conda:app-dev", "backend"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestRuntimeCondaBaseNotFound names every candidate it tried.
func TestRuntimeCondaBaseNotFound(t *testing.T) {
	root, _ := testRepo(t)
	t.Setenv("HOME", t.TempDir())
	unsetEnv(t, "CONDA_EXE")
	unsetEnv(t, "CONDA_PREFIX")

	cfg := testConfig(t, root, config.Spec{
		Name: "backend", Port: 7102, Cmd: []string{"python"}, Runtime: "conda:app-dev",
	})

	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	err = instFor(t, insts, "backend").Runnable()
	if err == nil {
		t.Fatal("expected a conda base failure")
	}
	for _, want := range []string{"miniconda3", "anaconda3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not list the %s candidate", err, want)
		}
	}
}

// TestRuntimeCondaBaseFromActivePrefix recovers the base from an activated
// environment's CONDA_PREFIX (<base>/envs/<name>).
func TestRuntimeCondaBaseFromActivePrefix(t *testing.T) {
	root, _ := testRepo(t)
	base := t.TempDir()
	condaBin := filepath.Join(base, "envs", "app-dev", "bin")
	writeExecutable(t, filepath.Join(condaBin, "python"))
	unsetEnv(t, "CONDA_EXE")
	t.Setenv("CONDA_PREFIX", filepath.Join(base, "envs", "other"))
	if err := os.MkdirAll(filepath.Join(base, "envs", "other"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig(t, root, config.Spec{
		Name: "backend", Port: 7102, Cmd: []string{"python"}, Runtime: "conda:app-dev",
	})

	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := filepath.Join(condaBin, "python"); instFor(t, insts, "backend").Cmd[0] != want {
		t.Errorf("Cmd[0] = %q, want %q", instFor(t, insts, "backend").Cmd[0], want)
	}
}

// TestRuntimeNodeResolves builds a fake nvm tree.
func TestRuntimeNodeResolves(t *testing.T) {
	root, _ := testRepo(t)
	nvm := t.TempDir()
	nodeBin := filepath.Join(nvm, "versions", "node", "v24.4.1", "bin")
	writeExecutable(t, filepath.Join(nodeBin, "npm"))
	t.Setenv("NVM_DIR", nvm)

	cfg := testConfig(t, root, config.Spec{
		Name: "website", Port: 7100, Cmd: []string{"npm", "run", "dev"}, Runtime: "node:24.4.1",
	})

	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	in := instFor(t, insts, "website")
	if want := filepath.Join(nodeBin, "npm"); in.Cmd[0] != want {
		t.Errorf("Cmd[0] = %q, want %q", in.Cmd[0], want)
	}
	path, _ := envValue(in.Env, "PATH")
	if !strings.HasPrefix(path, nodeBin) {
		t.Errorf("PATH = %q, want it to start with %q", path, nodeBin)
	}
}

// TestRuntimeNodeFailureNamesResolvedPath is the other half of spec bug #5:
// under a non-login shell nvm is absent and npm is missing or the wrong major.
func TestRuntimeNodeFailureNamesResolvedPath(t *testing.T) {
	root, _ := testRepo(t)
	nvm := t.TempDir()
	t.Setenv("NVM_DIR", nvm)

	cfg := testConfig(t, root, config.Spec{
		Name: "website", Port: 7100, Cmd: []string{"npm"}, Runtime: "node:24.4.1",
	})

	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	err = instFor(t, insts, "website").Runnable()
	if err == nil {
		t.Fatal("expected the node lookup to fail")
	}
	wantPath := filepath.Join(nvm, "versions", "node", "v24.4.1", "bin", "npm")
	for _, want := range []string{wantPath, "node:24.4.1", "website"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestRuntimeNodeAcceptsAVPrefixedVersion normalises node:v24 and node:24.
func TestRuntimeNodeAcceptsAVPrefixedVersion(t *testing.T) {
	root, _ := testRepo(t)
	nvm := t.TempDir()
	nodeBin := filepath.Join(nvm, "versions", "node", "v24.4.1", "bin")
	writeExecutable(t, filepath.Join(nodeBin, "npm"))
	t.Setenv("NVM_DIR", nvm)

	cfg := testConfig(t, root, config.Spec{
		Name: "website", Port: 7100, Cmd: []string{"npm"}, Runtime: "node:v24.4.1",
	})

	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := filepath.Join(nodeBin, "npm"); instFor(t, insts, "website").Cmd[0] != want {
		t.Errorf("Cmd[0] = %q, want %q", instFor(t, insts, "website").Cmd[0], want)
	}
}

// TestRuntimeRejectsTraversal defends the path composition even when Resolve is
// handed a Config that config.Load never validated.
func TestRuntimeRejectsTraversal(t *testing.T) {
	root, _ := testRepo(t)
	for _, rt := range []string{"conda:../../etc", "node:../..", "docker:latest", "conda"} {
		cfg := testConfig(t, root, config.Spec{Name: "a", Port: 7100, Cmd: []string{"runme"}, Runtime: rt})
		if _, _, err := Resolve(cfg, testState(t, root, nil), Options{}); err == nil {
			t.Errorf("runtime %q was accepted", rt)
		}
	}
}

// TestRuntimeNotExecutable distinguishes "missing" from "not executable".
func TestRuntimeNotExecutable(t *testing.T) {
	root, _ := testRepo(t)
	base := t.TempDir()
	condaBin := filepath.Join(base, "envs", "e", "bin")
	if err := os.MkdirAll(condaBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(condaBin, "python"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(base, "bin", "conda"))
	t.Setenv("CONDA_EXE", filepath.Join(base, "bin", "conda"))

	cfg := testConfig(t, root, config.Spec{
		Name: "a", Port: 7100, Cmd: []string{"python"}, Runtime: "conda:e",
	})

	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	err = instFor(t, insts, "a").Runnable()
	if err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("got %v, want a not-executable error", err)
	}
}

// TestRuntimeSystemRelativePathUsesServiceDir resolves ./script against the
// service's own directory, which is the child's working directory.
func TestRuntimeSystemRelativePathUsesServiceDir(t *testing.T) {
	root, _ := testRepo(t)
	writeExecutable(t, filepath.Join(root, "backend", "start.sh"))

	cfg := testConfig(t, root, config.Spec{
		Name: "backend", Dir: "backend", Port: 7102, Cmd: []string{"./start.sh"},
	})

	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join(root, "backend", "start.sh")
	if got := instFor(t, insts, "backend").Cmd[0]; got != want {
		t.Errorf("Cmd[0] = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// directories and environment
// ---------------------------------------------------------------------------

// TestResolveDirMissing is dev.sh bug #1: a declared directory that never
// existed could only ever fail later, at cd time.
func TestResolveDirMissing(t *testing.T) {
	root, _ := testRepo(t)
	cfg := &config.Config{
		Root:     root,
		Path:     filepath.Join(root, config.FileName),
		Services: []config.Spec{{Name: "browser", Dir: "browser", Port: 7103, Cmd: []string{"runme"}}},
	}

	_, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err == nil {
		t.Fatal("expected an error for a missing directory")
	}
	if !strings.Contains(err.Error(), filepath.Join(root, "browser")) {
		t.Errorf("error %q does not name the resolved directory", err)
	}
}

// TestResolveDirEscapingRoot refuses a directory outside the project root.
func TestResolveDirEscapingRoot(t *testing.T) {
	root, _ := testRepo(t)
	cfg := &config.Config{
		Root:     root,
		Path:     filepath.Join(root, config.FileName),
		Services: []config.Spec{{Name: "a", Dir: "../..", Port: 7100, Cmd: []string{"runme"}}},
	}

	if _, _, err := Resolve(cfg, testState(t, root, nil), Options{}); err == nil {
		t.Fatal("expected an error for a directory outside the project root")
	}
}

// TestResolveDirDefaultsToRoot: an omitted dir means the directory holding
// mabo-ctl.yaml.
func TestResolveDirDefaultsToRoot(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root, svc("a", 7100))

	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := instFor(t, insts, "a").Dir; got != root {
		t.Errorf("Dir = %q, want %q", got, root)
	}
}

// TestEnvDeclaredValueWinsOverInherited: the service's own env is the most
// specific declaration there is.
func TestEnvDeclaredValueWinsOverInherited(t *testing.T) {
	root, _ := testRepo(t)
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("KEEP_ME", "yes")

	cfg := testConfig(t, root, config.Spec{
		Name: "a", Port: 7100, Cmd: []string{"runme"},
		Env: map[string]string{"LOG_LEVEL": "info"},
	})

	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	in := instFor(t, insts, "a")
	if v, _ := envValue(in.Env, "LOG_LEVEL"); v != "info" {
		t.Errorf("LOG_LEVEL = %q, want info", v)
	}
	if v, _ := envValue(in.Env, "KEEP_ME"); v != "yes" {
		t.Errorf("KEEP_ME = %q, want the inherited value", v)
	}
	// No key may appear twice: exec's own de-duplication is not a contract.
	seen := map[string]bool{}
	for _, e := range in.Env {
		k, _, _ := strings.Cut(e, "=")
		if seen[k] {
			t.Errorf("environment contains %q twice", k)
		}
		seen[k] = true
	}
}

// TestEnvCarriesEveryResolvedPort lets a service address its peers without
// templating every variable.
func TestEnvCarriesEveryResolvedPort(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root, svc("website", 7100), svc("backend", 7102), svc("worker", 0))

	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	in := instFor(t, insts, "worker")
	if v, _ := envValue(in.Env, "BACKEND_PORT"); v != "7102" {
		t.Errorf("BACKEND_PORT = %q, want 7102", v)
	}
	if v, _ := envValue(in.Env, "WEBSITE_PORT"); v != "7100" {
		t.Errorf("WEBSITE_PORT = %q, want 7100", v)
	}
	if _, ok := envValue(in.Env, "WORKER_PORT"); ok {
		t.Error("a portless service must not be given a *_PORT variable")
	}
}

// ---------------------------------------------------------------------------
// Select
// ---------------------------------------------------------------------------

// TestSelectDependencyOrder returns dependencies before their dependants,
// transitively.
func TestSelectDependencyOrder(t *testing.T) {
	insts := []Instance{
		{Name: "worker", DependsOn: []string{"backend"}},
		{Name: "backend", DependsOn: []string{"db"}},
		{Name: "db"},
		{Name: "website"},
	}

	got, err := Select(insts, []string{"worker"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if strings.Join(names(got), ",") != "db,backend,worker" {
		t.Errorf("Select = %v, want [db backend worker]", names(got))
	}
}

// TestSelectEmptyWantIsEverything, still in dependency order.
func TestSelectEmptyWantIsEverything(t *testing.T) {
	insts := []Instance{
		{Name: "worker", DependsOn: []string{"backend"}},
		{Name: "backend"},
	}

	got, err := Select(insts, nil)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if strings.Join(names(got), ",") != "backend,worker" {
		t.Errorf("Select = %v, want [backend worker]", names(got))
	}
}

// TestSelectDeduplicates: naming a service twice, or naming both a dependency
// and its dependant, yields each instance once.
func TestSelectDeduplicates(t *testing.T) {
	insts := []Instance{
		{Name: "backend"},
		{Name: "worker", DependsOn: []string{"backend"}},
	}

	got, err := Select(insts, []string{"backend", "worker", "backend"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if strings.Join(names(got), ",") != "backend,worker" {
		t.Errorf("Select = %v, want [backend worker]", names(got))
	}
}

// TestSelectUnknownNameListsValidOnes.
func TestSelectUnknownNameListsValidOnes(t *testing.T) {
	insts := []Instance{{Name: "backend"}, {Name: "website"}}

	_, err := Select(insts, []string{"backned"})
	if err == nil {
		t.Fatal("expected an error for an unknown service")
	}
	for _, want := range []string{"backned", "backend", "website"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestSelectCycleTerminates: config rejects cycles at load time, but Select
// must terminate on any input rather than recursing forever.
func TestSelectCycleTerminates(t *testing.T) {
	insts := []Instance{
		{Name: "a", DependsOn: []string{"b"}},
		{Name: "b", DependsOn: []string{"a"}},
	}

	_, err := Select(insts, nil)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("got %v, want a cycle error", err)
	}
}

// TestSelectUnknownDependency reports a dependency that is not in the instance
// list at all.
func TestSelectUnknownDependency(t *testing.T) {
	insts := []Instance{{Name: "worker", DependsOn: []string{"ghost"}}}

	_, err := Select(insts, nil)
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("got %v, want an error naming the missing dependency", err)
	}
}

// TestSelectDiamond visits a shared dependency once and still orders correctly.
func TestSelectDiamond(t *testing.T) {
	insts := []Instance{
		{Name: "top", DependsOn: []string{"left", "right"}},
		{Name: "left", DependsOn: []string{"base"}},
		{Name: "right", DependsOn: []string{"base"}},
		{Name: "base"},
	}

	got, err := Select(insts, []string{"top"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if strings.Join(names(got), ",") != "base,left,right,top" {
		t.Errorf("Select = %v, want [base left right top]", names(got))
	}
}

// TestSelectExactStopsAtTheNamedRoots: SelectExact is the stop-side selector,
// and a stop means "these services down" — never the closure beneath them.
// The expansion it must not do is exactly what made `stop listener` kill its
// backend (docs/LANDMINES.md §8).
func TestSelectExactStopsAtTheNamedRoots(t *testing.T) {
	insts := []Instance{
		{Name: "worker", DependsOn: []string{"backend"}},
		{Name: "backend"},
		{Name: "website"},
	}

	got, err := SelectExact(insts, []string{"worker"})
	if err != nil {
		t.Fatalf("SelectExact: %v", err)
	}
	if strings.Join(names(got), ",") != "worker" {
		t.Errorf("SelectExact = %v, want [worker] — dependencies must not ride along", names(got))
	}
}

func TestSelectExactEmptyWantIsEverythingInDeclarationOrder(t *testing.T) {
	insts := []Instance{
		{Name: "worker", DependsOn: []string{"backend"}},
		{Name: "backend"},
		{Name: "website"},
	}

	got, err := SelectExact(insts, nil)
	if err != nil {
		t.Fatalf("SelectExact: %v", err)
	}
	if strings.Join(names(got), ",") != "worker,backend,website" {
		t.Errorf("SelectExact = %v, want [worker backend website]", names(got))
	}
}

func TestSelectExactDeduplicatesAndKeepsOrder(t *testing.T) {
	insts := []Instance{
		{Name: "backend"},
		{Name: "worker", DependsOn: []string{"backend"}},
		{Name: "website"},
	}

	got, err := SelectExact(insts, []string{"website", "backend", "website"})
	if err != nil {
		t.Fatalf("SelectExact: %v", err)
	}
	if strings.Join(names(got), ",") != "backend,website" {
		t.Errorf("SelectExact = %v, want [backend website]", names(got))
	}
}

func TestSelectExactUnknownNameListsValidOnes(t *testing.T) {
	insts := []Instance{{Name: "backend"}, {Name: "worker"}}
	if _, err := SelectExact(insts, []string{"ghost"}); err == nil {
		t.Error("an unknown service name was accepted")
	}
}

// ---------------------------------------------------------------------------
// persistence
// ---------------------------------------------------------------------------

// TestPersistRoundTrip writes the resolved ports and reads them back as the
// run.env precedence level.
func TestPersistRoundTrip(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root, svc("backend", 7102), svc("worker", 0))
	st := testState(t, root, nil)

	insts, _, err := Resolve(cfg, st, Options{Ports: []int{7999}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := Persist(st, insts); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	_, origins, err := Resolve(cfg, st, Options{})
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	o := originFor(t, origins, "backend")
	if o.Port != 7999 || o.Source != FromRunEnv || !o.Override {
		t.Errorf("got %d from %q (override %v), want 7999 from run.env with override", o.Port, o.Source, o.Override)
	}
	re, err := st.ReadRunEnv()
	if err != nil {
		t.Fatalf("ReadRunEnv: %v", err)
	}
	if p, ok := re.Port("worker"); ok {
		t.Errorf("portless service persisted port %d", p)
	}
	raw, err := os.ReadFile(st.RunEnvPath())
	if err != nil {
		t.Fatalf("read run.env: %v", err)
	}
	if !strings.Contains(string(raw), "PORT_BACKEND=7999") {
		t.Errorf("run.env = %q, want a PORT_BACKEND=7999 line", raw)
	}
}

// TestPersistNilState reports rather than panicking.
func TestPersistNilState(t *testing.T) {
	if err := Persist(nil, nil); err == nil {
		t.Fatal("expected an error for a nil state dir")
	}
}

// ---------------------------------------------------------------------------
// misc
// ---------------------------------------------------------------------------

// TestResolveRejectsEmptyConfig.
func TestResolveRejectsEmptyConfig(t *testing.T) {
	if _, _, err := Resolve(nil, nil, Options{}); err == nil {
		t.Error("expected an error for a nil config")
	}
	if _, _, err := Resolve(&config.Config{Root: t.TempDir()}, nil, Options{}); err == nil {
		t.Error("expected an error for a config with no services")
	}
}

// TestResolveRejectsEmptyCmd keeps dev.sh bug #2 unrepresentable: a service that
// runs nothing must not reach the supervisor and be reported as "process died".
func TestResolveRejectsEmptyCmd(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root, config.Spec{Name: "a", Port: 7100})

	if _, _, err := Resolve(cfg, testState(t, root, nil), Options{}); err == nil {
		t.Fatal("expected an error for an empty cmd")
	}
}

// TestInstanceFieldsAreCopied: mutating an Instance must not reach back into
// the Config.
func TestInstanceFieldsAreCopied(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root,
		svc("backend", 7102),
		config.Spec{Name: "worker", Cmd: []string{"runme"}, DependsOn: []string{"backend"}},
	)

	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	in := instFor(t, insts, "worker")
	in.DependsOn[0] = "mutated"
	if cfg.Services[1].DependsOn[0] != "backend" {
		t.Error("mutating Instance.DependsOn changed the Config")
	}
}

// TestUnavailableRuntimeDoesNotBlockOtherServices is a regression test for a
// wedged supervisor.
//
// When an unresolvable runtime failed the whole Resolve call, a single service
// whose interpreter was not installed took every other service down with it:
// `mabo-ctl stop`, `status`, `logs` and even `reset` all refused to run, so
// services that were ALREADY RUNNING could not be stopped or cleaned up. That
// is precisely the orphan accumulation mabo-ctl exists to prevent, triggered by a
// service the user never asked to touch.
func TestUnavailableRuntimeDoesNotBlockOtherServices(t *testing.T) {
	root, bin := testRepo(t)
	t.Setenv("HOME", t.TempDir())
	unsetEnv(t, "CONDA_EXE")
	unsetEnv(t, "CONDA_PREFIX")

	cfg := testConfig(t, root,
		config.Spec{Name: "broken", Port: 7100, Cmd: []string{"python"}, Runtime: "conda:not-installed"},
		config.Spec{Name: "fine", Port: 7101, Cmd: []string{"runme"}},
	)

	insts, _, err := Resolve(cfg, testState(t, root, nil), Options{})
	if err != nil {
		t.Fatalf("Resolve failed because of an unrelated service: %v", err)
	}
	if len(insts) != 2 {
		t.Fatalf("got %d instances, want 2", len(insts))
	}

	// The healthy service is fully usable.
	fine := instFor(t, insts, "fine")
	if err := fine.Runnable(); err != nil {
		t.Errorf("fine.Runnable() = %v, want nil", err)
	}
	if want := filepath.Join(bin, "runme"); fine.Cmd[0] != want {
		t.Errorf("fine Cmd[0] = %q, want %q", fine.Cmd[0], want)
	}
	if fine.Port != 7101 {
		t.Errorf("fine Port = %d, want 7101", fine.Port)
	}

	// The broken one still refuses to run, and still says why.
	broken := instFor(t, insts, "broken")
	err = broken.Runnable()
	if err == nil {
		t.Fatal("broken.Runnable() = nil, want the deferred runtime error")
	}
	for _, want := range []string{"broken", "conda:not-installed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestMalformedRuntimeStillFailsResolve guards the other side of the split: a
// runtime that is syntactically wrong, or that would traverse out of the
// runtime root, is wrong on EVERY machine and must fail the load rather than be
// deferred to spawn time.
func TestMalformedRuntimeStillFailsResolve(t *testing.T) {
	for _, runtime := range []string{"conda:../../etc", "node:../..", "docker:latest", "conda"} {
		t.Run(runtime, func(t *testing.T) {
			root, _ := testRepo(t)
			cfg := testConfig(t, root, config.Spec{
				Name: "a", Port: 7100, Cmd: []string{"runme"}, Runtime: runtime,
			})
			if _, _, err := Resolve(cfg, testState(t, root, nil), Options{}); err == nil {
				t.Fatalf("runtime %q was accepted by Resolve", runtime)
			}
		})
	}
}

// TestSelectLevels groups services by dependency depth so a supervisor can
// start a whole level at once instead of paying the sum of every startup.
func TestSelectLevels(t *testing.T) {
	inst := func(name string, deps ...string) Instance {
		return Instance{Name: name, Dir: ".", Cmd: []string{"x"}, DependsOn: deps}
	}
	names := func(levels [][]Instance) [][]string {
		out := make([][]string, len(levels))
		for i, lv := range levels {
			for _, in := range lv {
				out[i] = append(out[i], in.Name)
			}
		}
		return out
	}

	tests := []struct {
		name  string
		insts []Instance
		want  []string
		lvls  [][]string
	}{
		{
			name:  "all independent collapse to one level",
			insts: []Instance{inst("a"), inst("b"), inst("c")},
			lvls:  [][]string{{"a", "b", "c"}},
		},
		{
			name:  "a chain is one service per level",
			insts: []Instance{inst("a"), inst("b", "a"), inst("c", "b")},
			lvls:  [][]string{{"a"}, {"b"}, {"c"}},
		},
		{
			name:  "a diamond puts the two middles together",
			insts: []Instance{inst("db"), inst("api", "db"), inst("web", "db"), inst("e2e", "api", "web")},
			lvls:  [][]string{{"db"}, {"api", "web"}, {"e2e"}},
		},
		{
			name:  "an independent service rides level 0 with a chain root",
			insts: []Instance{inst("db"), inst("api", "db"), inst("docs")},
			lvls:  [][]string{{"db", "docs"}, {"api"}},
		},
		{
			name:  "declaration order is preserved within a level",
			insts: []Instance{inst("z"), inst("m"), inst("a")},
			lvls:  [][]string{{"z", "m", "a"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			levels, err := SelectLevels(tc.insts, tc.want)
			if err != nil {
				t.Fatalf("SelectLevels: %v", err)
			}
			got := names(levels)
			if len(got) != len(tc.lvls) {
				t.Fatalf("got %d levels %v, want %d %v", len(got), got, len(tc.lvls), tc.lvls)
			}
			for i := range tc.lvls {
				if strings.Join(got[i], ",") != strings.Join(tc.lvls[i], ",") {
					t.Errorf("level %d = %v, want %v", i, got[i], tc.lvls[i])
				}
			}
		})
	}
}

// TestSelectLevelsIgnoresDependenciesOutsideTheSelection: `mabo-ctl start api`
// must not be held back by a level created for something the user did not ask
// for and that Select therefore did not return.
func TestSelectLevelsIgnoresDependenciesOutsideTheSelection(t *testing.T) {
	insts := []Instance{
		{Name: "db", Dir: ".", Cmd: []string{"x"}},
		{Name: "api", Dir: ".", Cmd: []string{"x"}, DependsOn: []string{"db"}},
		{Name: "solo", Dir: ".", Cmd: []string{"x"}},
	}
	levels, err := SelectLevels(insts, []string{"solo"})
	if err != nil {
		t.Fatalf("SelectLevels: %v", err)
	}
	if len(levels) != 1 || len(levels[0]) != 1 || levels[0][0].Name != "solo" {
		t.Fatalf("levels = %v, want a single level holding only solo", levels)
	}
}

// TestSelectLevelsPropagatesSelectErrors keeps the two entry points reporting
// the same problems the same way.
func TestSelectLevelsPropagatesSelectErrors(t *testing.T) {
	cyc := []Instance{
		{Name: "a", Dir: ".", Cmd: []string{"x"}, DependsOn: []string{"b"}},
		{Name: "b", Dir: ".", Cmd: []string{"x"}, DependsOn: []string{"a"}},
	}
	if _, err := SelectLevels(cyc, nil); err == nil {
		t.Error("a dependency cycle was accepted")
	}
	ok := []Instance{{Name: "a", Dir: ".", Cmd: []string{"x"}}}
	if _, err := SelectLevels(ok, []string{"ghost"}); err == nil {
		t.Error("an unknown service name was accepted")
	}
}

// ---------------------------------------------------------------------------
// env_file
// ---------------------------------------------------------------------------

// TestEnvFileFeedsTheChildEnvironment: file values land in Instance.Env,
// inline env overrides the same key, and a template inside the file expands.
func TestEnvFileFeedsTheChildEnvironment(t *testing.T) {
	root, _ := testRepo(t)
	envPath := filepath.Join(root, "api.env")
	if err := os.WriteFile(envPath, []byte(
		"# base\nSHARED=from-file\nFILE_ONLY=hello\nURL=http://localhost:{{.Port}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, root, config.Spec{
		Name: "backend", Port: 7102, Cmd: []string{"runme"},
		EnvFile: "api.env",
		Env:     map[string]string{"SHARED": "from-inline"},
	})

	insts, _, err := Resolve(cfg, nil, Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	env := instFor(t, insts, "backend").Env
	for k, want := range map[string]string{
		"SHARED":    "from-inline", // inline wins over the file
		"FILE_ONLY": "hello",
		"URL":       "http://localhost:7102", // file values are templates too
	} {
		if got, ok := envValue(env, k); !ok || got != want {
			t.Errorf("%s = %q (present=%t), want %q", k, got, ok, want)
		}
	}
}

// TestEnvFileRereadAtResolve: the file is parsed at RESOLVE time, so an edit
// after Load is picked up without touching mabo-ctl.yaml.
func TestEnvFileRereadAtResolve(t *testing.T) {
	root, _ := testRepo(t)
	envPath := filepath.Join(root, "api.env")
	if err := os.WriteFile(envPath, []byte("TOKEN=before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, root, config.Spec{Name: "backend", Cmd: []string{"runme"}, EnvFile: "api.env"})

	if err := os.WriteFile(envPath, []byte("TOKEN=after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	insts, _, err := Resolve(cfg, nil, Options{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, _ := envValue(instFor(t, insts, "backend").Env, "TOKEN"); got != "after" {
		t.Errorf("TOKEN = %q, want the edited value %q", got, "after")
	}
}

// TestEnvFileBrokenAtResolveIsAnError: a file deleted or broken after load
// fails the resolve naming the service, instead of silently starting with a
// half environment.
func TestEnvFileBrokenAtResolveIsAnError(t *testing.T) {
	root, _ := testRepo(t)
	cfg := testConfig(t, root, config.Spec{Name: "backend", Cmd: []string{"runme"}, EnvFile: "gone.env"})
	if _, _, err := Resolve(cfg, nil, Options{}); err == nil || !strings.Contains(err.Error(), "backend") {
		t.Fatalf("err = %v, want a failure naming the service", err)
	}
}

// tcp and exec probes

// TestProbeExpansionCoversEveryKind: each probe family is template-expanded
// like Cmd and Env are, and the display form is what status output will quote.
func TestProbeExpansionCoversEveryKind(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t, root,
		config.Spec{Name: "backend", Cmd: []string{"serve"}, Port: 7102},
		config.Spec{
			Name:   "worker",
			Cmd:    []string{"run"},
			Health: config.Health{Kind: config.HealthTCP, Addr: "localhost:{{.Port \"backend\"}}"},
		},
		config.Spec{
			Name:   "checker",
			Cmd:    []string{"run"},
			Health: config.Health{Kind: config.HealthExec, Argv: []string{"/bin/echo", "{{.Port}}"}},
		},
	)
	// checker declares its own port so {{.Port}} is legal in the argv.
	cfg.Services[2].Port = 7999

	insts, _, err := Resolve(cfg, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Instance{}
	for _, in := range insts {
		byName[in.Name] = in
	}

	if p := byName["worker"].Readiness(); p.Kind != ProbeTCP || p.Addr != "localhost:7102" {
		t.Errorf("tcp probe = %+v, want addr expanded to localhost:7102", p)
	} else if byName["worker"].Health != "tcp:localhost:7102" {
		t.Errorf("tcp display = %q, want tcp:localhost:7102", byName["worker"].Health)
	}

	p := byName["checker"].Readiness()
	if p.Kind != ProbeExec || !slices.Equal(p.Argv, []string{"/bin/echo", "7999"}) {
		t.Errorf("exec probe = %+v, want argv expanded to [/bin/echo 7999]", p)
	}
	if !strings.HasPrefix(byName["checker"].Health, "exec: [") {
		t.Errorf("exec display = %q, want the exec: [...] form", byName["checker"].Health)
	}
}

// TestReadinessTreatsAHandBuiltHealthAsHTTP: instances composed directly carry
// only Health, and Health has always meant an HTTP URL.
func TestReadinessTreatsAHandBuiltHealthAsHTTP(t *testing.T) {
	in := Instance{Name: "a", Health: "http://x/healthz"}
	p := in.Readiness()
	if p.Kind != ProbeHTTP || p.URL != "http://x/healthz" {
		t.Errorf("Readiness = %+v, want the http probe from Health", p)
	}
	if (Instance{Name: "b"}).Readiness().Kind != ProbeNone {
		t.Error("an instance with no health must have no readiness")
	}
}

// TestExecDisplayRedactsSecretLookingArgs: the exec display reaches every
// output channel, so it is redacted at the source.
func TestExecDisplayRedactsSecretLookingArgs(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t, root, config.Spec{
		Name:   "db",
		Cmd:    []string{"postgres"},
		Port:   5432,
		Env:    map[string]string{"API_KEY": "sk-live-whatever"},
		Health: config.Health{Kind: config.HealthExec, Argv: []string{"psql", "--password=hunter2", "postgres://user:hunter2@localhost/db"}},
	})
	insts, _, err := Resolve(cfg, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(insts[0].Health, "hunter2") {
		t.Errorf("display %q leaked the credential; redact.Args must run before display", insts[0].Health)
	}
}

// named port overrides and bare PORT injection

// TestPortOverrideIsTheTopLevel: --port svc=N beats every other level, and a
// name that cannot apply is an error rather than silence.
func TestPortOverrideIsTheTopLevel(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t, root,
		config.Spec{Name: "backend", Cmd: []string{"run"}, Port: 7100},
		config.Spec{Name: "worker", Cmd: []string{"run"}},
	)

	st := testState(t, root, map[string]int{"backend": 7999})

	insts, origins, err := Resolve(cfg, st, Options{PortOverrides: map[string]int{"backend": 8123}})
	if err != nil {
		t.Fatal(err)
	}
	if insts[0].Port != 8123 {
		t.Errorf("Port = %d, want the override 8123 beating even run.env's 7999", insts[0].Port)
	}
	if origins[0].Source != FromFlag {
		t.Errorf("Source = %q, want flag: an override arrives as a flag", origins[0].Source)
	}

	if _, _, err := Resolve(cfg, nil, Options{PortOverrides: map[string]int{"ghost": 1}}); err == nil ||
		!strings.Contains(err.Error(), "undeclared") {
		t.Errorf("override for an undeclared service: err = %v, want a naming error", err)
	}
	if _, _, err := Resolve(cfg, nil, Options{PortOverrides: map[string]int{"worker": 5000}}); err == nil ||
		!strings.Contains(err.Error(), "declares no port") {
		t.Errorf("override for a portless service: err = %v, want a cannot-apply error", err)
	}
}

// TestBuildEnvInjectsBarePORT: a service that declares a port gets it again as
// a bare PORT, the Procfile convention, and its declared env still wins.
func TestBuildEnvInjectsBarePORT(t *testing.T) {
	base := []string{"PATH=/usr/bin"}
	names := []string{"web"}
	ports := map[string]int{"web": 7100}
	specEnv := map[string]string{}

	env := buildEnv(base, names, ports, specEnv, resolvedRuntime{}, 7100)
	if lookupEnv(env, "PORT") != "7100" {
		t.Errorf("PORT = %q, want 7100", lookupEnv(env, "PORT"))
	}
	if lookupEnv(env, "WEB_PORT") != "7100" {
		t.Errorf("WEB_PORT = %q, want 7100 beside PORT", lookupEnv(env, "WEB_PORT"))
	}

	// A service that declares no port gets no PORT at all: injecting one would
	// hand a meaningless number to processes that mean something else by it.
	env = buildEnv(base, []string{"worker"}, ports, specEnv, resolvedRuntime{}, 0)
	if v, ok := lookupEnvOK(env, "PORT"); ok {
		t.Errorf("a portless service got PORT=%q; it must not be injected without a declared port", v)
	}

	// The declared env wins over both injected forms.
	env = buildEnv(base, names, ports, map[string]string{"PORT": "9999"}, resolvedRuntime{}, 7100)
	if lookupEnv(env, "PORT") != "9999" {
		t.Errorf("PORT = %q, want the declared 9999 to outrank the injection", lookupEnv(env, "PORT"))
	}
}

// lookupEnvOK is lookupEnv with a presence bit, for tests asserting absence.
func lookupEnvOK(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return entry[len(prefix):], true
		}
	}
	return "", false
}
