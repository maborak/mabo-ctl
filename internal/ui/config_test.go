package ui

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/service"
)

// configFixture builds a config, its resolved instances and their port origins
// the way service.Resolve hands them over.
func configFixture() ConfigInput {
	return ConfigInput{
		Config: &config.Config{
			Root:         "/repo",
			Path:         "/repo/mabo-ctl.yaml",
			StopGrace:    5 * time.Second,
			ReadyTimeout: 45 * time.Second,
			Services: []config.Spec{
				{
					Name: "alpha", Dir: "alpha", Port: 7100,
					Env: map[string]string{"API_TOKEN": "sk-live-SECRETVALUE", "LOG_LEVEL": "debug"},
				},
				{Name: "gamma", Dir: "."},
			},
		},
		Instances: []service.Instance{
			{
				Name:    "alpha",
				Dir:     "/repo/alpha",
				Port:    7999,
				Health:  "http://admin:hunter2@localhost:7999/health?api_key=sk-live-DEADBEEF",
				Cmd:     []string{"/usr/bin/node", "serve", "--token", "ghp_realtokenvalue"},
				Runtime: "node:20",
				Color:   "green",
			},
			{Name: "gamma", Dir: "/repo", Cmd: []string{"/bin/echo", "gamma"}},
		},
		Origins: []service.Origin{
			{Service: "alpha", Port: 7999, Source: service.FromRunEnv, Declared: 7100, Override: true},
			{Service: "gamma", Source: service.FromDefault},
		},
		StateDir: "/repo/.dev",
	}
}

// TestBuildConfigViewTakesTheOriginItIsGiven is the reason Origins is a
// parameter rather than something this package derives: the precedence chain
// has one implementation, in service.Resolve, and a view that recomputed it
// would be free to disagree with the supervisor that acted on it.
func TestBuildConfigViewTakesTheOriginItIsGiven(t *testing.T) {
	t.Parallel()
	v := BuildConfigView(configFixture())

	if len(v.Services) != 2 {
		t.Fatalf("got %d services, want 2", len(v.Services))
	}
	alpha := v.Services[0]
	if alpha.Port != 7999 || alpha.PortSource != "run.env" || alpha.PortDeclared != 7100 || !alpha.PortOverride {
		t.Errorf("alpha = %+v, want 7999 from run.env over a declared 7100, flagged", alpha)
	}
	if v.Source.Path != "/repo/mabo-ctl.yaml" || v.Source.Root != "/repo" || v.Source.StateDir != "/repo/.dev" {
		t.Errorf("source = %+v, want the loaded path, root and state dir", v.Source)
	}
	if v.Source.StopGraceMS != 5000 || v.Source.ReadyTimeoutMS != 45000 {
		t.Errorf("timeouts = %d/%d ms, want 5000/45000", v.Source.StopGraceMS, v.Source.ReadyTimeoutMS)
	}
	if gamma := v.Services[1]; gamma.Port != 0 || gamma.PortSource != "default" {
		t.Errorf("gamma = %+v, want a portless service reported as port 0 from default", gamma)
	}
}

// TestBuildConfigViewRedactsEveryField covers all three carriers at once. Each
// one was added to the console's redaction separately, and the field that was
// missed each time was the one nobody thought of as a credential.
func TestBuildConfigViewRedactsEveryField(t *testing.T) {
	t.Parallel()
	v := BuildConfigView(configFixture())
	body, err := ConfigJSON(v)
	if err != nil {
		t.Fatalf("ConfigJSON: %v", err)
	}
	out := string(body)
	for _, secret := range []string{"hunter2", "sk-live-DEADBEEF", "ghp_realtokenvalue", "sk-live-SECRETVALUE"} {
		if strings.Contains(out, secret) {
			t.Errorf("the resolved view carries %q:\n%s", secret, out)
		}
	}
	for _, keep := range []string{"API_TOKEN", "LOG_LEVEL", "debug", "--token"} {
		if !strings.Contains(out, keep) {
			t.Errorf("the resolved view dropped %q, which is not a credential:\n%s", keep, out)
		}
	}
}

// TestBuildConfigViewDoesNotMutateTheInstance guards the caller that renders
// the same instance and then spawns it.
func TestBuildConfigViewDoesNotMutateTheInstance(t *testing.T) {
	t.Parallel()
	in := configFixture()
	_ = BuildConfigView(in)
	if got := in.Instances[0].Cmd[3]; got != "ghp_realtokenvalue" {
		t.Fatalf("BuildConfigView rewrote the argv mabo-ctl execs: %q", got)
	}
}

// TestConfigJSONRendersEmptyServicesAsArray keeps a consumer's loop
// unconditional.
func TestConfigJSONRendersEmptyServicesAsArray(t *testing.T) {
	t.Parallel()
	body, err := ConfigJSON(ConfigView{})
	if err != nil {
		t.Fatalf("ConfigJSON: %v", err)
	}
	if !strings.Contains(string(body), `"services": []`) {
		t.Errorf("empty services rendered as null rather than []:\n%s", body)
	}
	var back ConfigView
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatalf("the rendered view does not parse: %v", err)
	}
}

// TestConfigBlockShowsThePrecedence is the text half. The number alone is what
// the tool printed before this command existed, and it is the half that cannot
// answer "why".
func TestConfigBlockShowsThePrecedence(t *testing.T) {
	t.Parallel()
	r := &Renderer{}
	out := r.ConfigBlock(BuildConfigView(configFixture()), "")

	for _, want := range []string{
		"/repo/mabo-ctl.yaml",
		"found by walking up",
		"stop_grace 5.0s",
		"ready_timeout 45.0s",
		"7999  from run.env",
		"OVERRIDES the declared 7100",
		"node:20",
		"/usr/bin/node",
		"(portless)",
		"no readiness probe declared",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ConfigBlock is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("a plain Renderer emitted an escape sequence:\n%q", out)
	}
}

// TestConfigBlockAppendsTheFileOnlyWhenGiven covers the narrowed view, which
// has no file section because a slice of YAML is not the operator's file.
func TestConfigBlockAppendsTheFileOnlyWhenGiven(t *testing.T) {
	t.Parallel()
	r := &Renderer{}
	v := BuildConfigView(configFixture())

	if got := r.ConfigBlock(v, ""); strings.Contains(got, "verbatim") {
		t.Errorf("ConfigBlock added a file section for an empty raw:\n%s", got)
	}
	got := r.ConfigBlock(v, "services:\n  - name: alpha\n")
	if !strings.Contains(got, "services:\n  - name: alpha") {
		t.Errorf("ConfigBlock did not render the file it was given:\n%s", got)
	}
	if !strings.Contains(got, "`mabo-ctl config --raw` prints the file verbatim") {
		t.Errorf("ConfigBlock did not say the file it printed was redacted:\n%s", got)
	}
}

// TestConfigBlockNamesAnUnresolvedRuntime: a service whose interpreter did not
// resolve must not be shown a command that cannot run, without saying so.
func TestConfigBlockNamesAnUnresolvedRuntime(t *testing.T) {
	t.Parallel()
	in := configFixture()
	in.Instances = []service.Instance{{
		Name:    "alpha",
		Dir:     "/repo/alpha",
		Cmd:     []string{"python", "app.py"},
		Runtime: "conda:app-dev",
		CmdErr:  errors.New("conda environment \"app-dev\" has no python"),
	}}
	out := (&Renderer{}).ConfigBlock(BuildConfigView(in), "")

	if !strings.Contains(out, "(unresolved)") {
		t.Errorf("ConfigBlock rendered an unrunnable command as if it would run:\n%s", out)
	}
	if !strings.Contains(out, "has no python") {
		t.Errorf("ConfigBlock dropped the resolution error:\n%s", out)
	}
	if strings.Contains(out, "→") {
		t.Errorf("ConfigBlock claimed the runtime produced an interpreter it did not:\n%s", out)
	}
}

func TestShellLineQuotesOnlyWhatNeedsIt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"/usr/bin/npm", "run", "dev"}, "/usr/bin/npm run dev"},
		{[]string{"echo", "hello world"}, "echo 'hello world'"},
		{[]string{"sh", "-c", "echo $HOME"}, `sh -c 'echo $HOME'`},
		{[]string{"echo", "it's"}, `echo 'it'\''s'`},
		{[]string{"echo", ""}, "echo ''"},
		{[]string{"app", "--port=7100"}, "app --port=7100"},
	}
	for _, tc := range cases {
		if got := ShellLine(tc.argv); got != tc.want {
			t.Errorf("ShellLine(%q) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}
