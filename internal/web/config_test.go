package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/ui"
)

// These tests cover GET /api/config, the console's answer to "why is this
// service on 7999, and what is it actually going to run?".
//
// The port SOURCE is the load-bearing part. Precedence has four levels and
// nothing else in mabo-ctl showed an operator which one won, so a route that
// reported the resolved port without its provenance would answer the easy half
// of the question and leave the expensive half exactly where it was.

// configServer builds a Server the way cmd/mabo-ctl builds one for `mabo-ctl serve`:
// with the port origins the resolution produced and the state directory
// internal/state owns.
func configServer(t *testing.T, ctrl Controller, origins []service.Origin, stateDir string) *Server {
	t.Helper()
	s, err := NewWith(ctrl, Options{Addr: recorderAddr, Origins: origins, StateDir: stateDir})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}
	return s
}

// getConfig performs GET /api/config and decodes the body.
func getConfig(t *testing.T, s *Server) ui.ConfigView {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/config", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out ui.ConfigView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding /api/config: %v (body %s)", err, rec.Body.String())
	}
	return out
}

// findService returns the entry named n, failing the test when it is absent.
func findService(t *testing.T, resp ui.ConfigView, n string) ui.ConfigService {
	t.Helper()
	for _, s := range resp.Services {
		if s.Name == n {
			return s
		}
	}
	t.Fatalf("/api/config has no service %q", n)
	return ui.ConfigService{}
}

func TestConfigNamesTheLoadedFileAndTheEffectiveTimeouts(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	ctrl.cfg.StopGrace = 7 * time.Second
	ctrl.cfg.ReadyTimeout = 45 * time.Second

	got := getConfig(t, configServer(t, ctrl, nil, "/repo/.dev"))

	if got.Source.Path != "/repo/mabo-ctl.yaml" {
		t.Errorf("source.path = %q, want /repo/mabo-ctl.yaml", got.Source.Path)
	}
	if got.Source.Root != "/repo" {
		t.Errorf("source.root = %q, want /repo", got.Source.Root)
	}
	if got.Source.StateDir != "/repo/.dev" {
		t.Errorf("source.state_dir = %q, want /repo/.dev", got.Source.StateDir)
	}
	if got.Source.StopGraceMS != 7000 {
		t.Errorf("source.stop_grace_ms = %d, want 7000", got.Source.StopGraceMS)
	}
	if got.Source.ReadyTimeoutMS != 45000 {
		t.Errorf("source.ready_timeout_ms = %d, want 45000", got.Source.ReadyTimeoutMS)
	}
	if got.Source.Explicit {
		t.Error("source.explicit is set, but this server was not told the config came from --config")
	}
	if len(got.Services) != 2 {
		t.Fatalf("got %d services, want 2", len(got.Services))
	}
	if got.Services[0].Name != "backend" || got.Services[1].Name != "frontend" {
		t.Errorf("services are not in declaration order: %q, %q",
			got.Services[0].Name, got.Services[1].Name)
	}
}

// TestConfigReportsThePortSourceForEveryPrecedenceLevel walks all four levels.
// A route that reported only the resolved number would leave the question this
// panel exists for exactly where it was.
func TestConfigReportsThePortSourceForEveryPrecedenceLevel(t *testing.T) {
	t.Parallel()
	for _, src := range []service.PortSource{
		service.FromFlag, service.FromEnv, service.FromRunEnv, service.FromDefault,
	} {
		t.Run(string(src), func(t *testing.T) {
			t.Parallel()
			ctrl := twoServices()
			origins := []service.Origin{
				{Service: "backend", Port: 7100, Source: src, Declared: 7100},
				{Service: "frontend", Port: 7200, Source: service.FromDefault, Declared: 7200},
			}
			got := findService(t, getConfig(t, configServer(t, ctrl, origins, "/repo/.dev")), "backend")

			if got.PortSource != string(src) {
				t.Errorf("port_source = %q, want %q", got.PortSource, src)
			}
			if got.Port != 7100 || got.PortDeclared != 7100 {
				t.Errorf("port/declared_port = %d/%d, want 7100/7100", got.Port, got.PortDeclared)
			}
			if got.PortOverride {
				t.Errorf("port_override is set for %s, but the resolved port equals the declared one", src)
			}
		})
	}
}

// TestConfigFlagsAPersistedPortBeatingAChangedDefault covers the documented
// trap: a port in .dev/run.env outranks the declared default, so editing
// mabo-ctl.yaml does nothing until the state is cleared. That silence cost a real
// debugging round, which is why the flag travels with the numbers.
func TestConfigFlagsAPersistedPortBeatingAChangedDefault(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	origins := []service.Origin{{
		Service:  "backend",
		Port:     7999,
		Source:   service.FromRunEnv,
		Declared: 7100,
		Override: true,
	}}
	got := findService(t, getConfig(t, configServer(t, ctrl, origins, "/repo/.dev")), "backend")

	if !got.PortOverride {
		t.Error("port_override is not set, so the console cannot warn about stale state")
	}
	if got.PortSource != string(service.FromRunEnv) {
		t.Errorf("port_source = %q, want run.env", got.PortSource)
	}
	if got.PortDeclared != 7100 {
		t.Errorf("declared_port = %d, want 7100 — the value being ignored is the point", got.PortDeclared)
	}
}

// TestConfigWithoutOriginsSaysSoRatherThanGuessing checks the degraded case. A
// missing origin must not be rendered as "default": that is a claim mabo-ctl did
// not make, and it is the one a reader would act on.
func TestConfigWithoutOriginsSaysSoRatherThanGuessing(t *testing.T) {
	t.Parallel()
	got := findService(t, getConfig(t, configServer(t, twoServices(), nil, "")), "backend")

	if got.PortSource != "" {
		t.Errorf("port_source = %q, want \"\" when nothing recorded the resolution", got.PortSource)
	}
	if got.PortOverride {
		t.Error("port_override is set with no origin to support it")
	}
}

// TestConfigRendersTheResolvedCommandAndRuntime checks the other half of the
// panel: which interpreter a service actually runs under, rather than the bare
// name written in mabo-ctl.yaml.
func TestConfigRendersTheResolvedCommandAndRuntime(t *testing.T) {
	t.Parallel()
	got := findService(t, getConfig(t, configServer(t, twoServices(), nil, "/repo/.dev")), "backend")

	if got.Runtime != "conda:api" {
		t.Errorf("runtime = %q, want conda:api", got.Runtime)
	}
	if len(got.Cmd) == 0 || got.Cmd[0] != "/usr/bin/python3" {
		t.Errorf("cmd = %v, want cmd[0] to be the absolute interpreter /usr/bin/python3", got.Cmd)
	}
	if !strings.HasPrefix(got.CmdLine, "/usr/bin/python3 ") {
		t.Errorf("cmd_line = %q, want it to start with the resolved interpreter", got.CmdLine)
	}
	if got.Dir != "/repo/backend" {
		t.Errorf("dir = %q, want /repo/backend", got.Dir)
	}
	if !strings.HasPrefix(got.Health, "http://localhost:7100/health") {
		t.Errorf("health = %q, want the EXPANDED probe URL", got.Health)
	}

	dep := findService(t, getConfig(t, configServer(t, twoServices(), nil, "")), "frontend")
	if len(dep.DependsOn) != 1 || dep.DependsOn[0] != "backend" {
		t.Errorf("depends_on = %v, want [backend]", dep.DependsOn)
	}
}

// TestConfigRedactsCredentialsEverywhereServicesDoes is the channel check. This
// route publishes the same three fields /api/services does — health, cmd and
// the declared environment — so a credential withheld there and printed here
// would be the control existing and simply not being wired to the new route.
func TestConfigRedactsCredentialsEverywhereServicesDoes(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	ctrl.insts[0].Health = "http://admin:hunter2@localhost:7100/health?api_key=sk-live-abcdef"
	ctrl.insts[0].Cmd = []string{
		"/usr/bin/python3", "-m", "uvicorn", "--token", "sk-live-abcdef",
		"--dsn=postgres://app:hunter2@localhost:5432/app",
	}

	s := configServer(t, ctrl, nil, "/repo/.dev")
	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/config", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, leak := range []string{"hunter2", "sk-live-abcdef", declaredSecret, inheritedSecret} {
		if strings.Contains(body, leak) {
			t.Errorf("/api/config leaked %q:\n%s", leak, body)
		}
	}
	// Keys and structure survive: "this service sets API_TOKEN" is exactly what
	// a developer needs, and withholding the key too would make the panel
	// useless rather than safe.
	for _, keep := range []string{"API_TOKEN", "LOG_LEVEL", "DATABASE_URL", "uvicorn"} {
		if !strings.Contains(body, keep) {
			t.Errorf("/api/config withheld %q, which is not a credential:\n%s", keep, body)
		}
	}
	if strings.Contains(body, inheritedKey) {
		t.Errorf("/api/config rendered the INHERITED environment, which it must never read:\n%s", body)
	}
}

// TestConfigIsGuardedLikeEveryOtherRoute keeps the new route inside the
// controls the package documentation promises for all of them.
func TestConfigIsGuardedLikeEveryOtherRoute(t *testing.T) {
	t.Parallel()
	s := configServer(t, twoServices(), nil, "/repo/.dev")

	// A forged Host is refused before the router decides what was asked for.
	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/config", nil)
	req.Header.Set(tokenHeader, s.Token())
	req.Host = "attacker.example:7999"
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("GET /api/config with a rebinding Host = %d, want 403", rec.Code)
	}

	// So is a cross-origin read, and no CORS header ever invites one.
	req = httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/config", nil)
	req.Header.Set(tokenHeader, s.Token())
	req.Header.Set("Origin", "http://evil.example")
	rec = httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("GET /api/config with a foreign Origin = %d, want 403", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/config", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec = httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	for h := range rec.Header() {
		if strings.HasPrefix(strings.ToLower(h), "access-control-") {
			t.Errorf("/api/config carries CORS header %s", h)
		}
	}

	// The route is a read, and the mux says so by method rather than by hoping
	// the handler ignores a body.
	req = httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+"/api/config", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec = httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/config = %d, want 405", rec.Code)
	}
}

// TestConfigWithoutAConfigFileIsStillWellFormed covers the case the Controller
// contract allows: Config may be nil. The view is then empty — there is no
// resolution to explain — but it is still an object with an empty ARRAY of
// services, because a page handed null where it expected a list renders nothing
// and reports nothing.
func TestConfigWithoutAConfigFileIsStillWellFormed(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	ctrl.cfg = nil

	s := configServer(t, ctrl, nil, "")
	req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/api/config", nil)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"services": null`) {
		t.Errorf("services is null rather than []: %s", rec.Body.String())
	}

	got := getConfig(t, configServer(t, ctrl, nil, ""))
	if got.Services == nil {
		t.Error("services decoded as null; the page has two empty shapes to defend against")
	}
	if len(got.Services) != 0 || got.Source.Path != "" {
		t.Errorf("a nil config produced a non-empty view: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// the page
// ---------------------------------------------------------------------------

// TestConsolePageHasAConfigPanel checks the browser half exists and is wired to
// the route, including a distinct class per precedence level: the source is the
// point of the panel, and four sources that render identically report nothing.
func TestConsolePageHasAConfigPanel(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		`id="config-view"`, // the panel
		`id="btn-config"`,  // the way in
		`"/api/config"`,    // wired to the route
		`id="cfg-body"`,    // where it renders
		".src-flag", ".src-env", ".src-runenv", ".src-default", ".src-unknown",
		"port_source",   // the field it reads
		"port_override", // and the trap it warns about
		"port_declared",
		"state_dir", "stop_grace_ms", "ready_timeout_ms", "console_addr",
	} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("console.html does not carry %q; the config panel is not wired", want)
		}
	}
}

// TestConsolePageReadsTheSameFieldsTheConfigViewEmits is the drift guard
// between the two halves of this feature. The page is plain JavaScript and
// cannot import internal/ui, so it names the JSON keys as string literals; a
// key renamed on the Go side would leave the panel silently blank, which is the
// failure mode nobody notices because nothing errors.
func TestConsolePageReadsTheSameFieldsTheConfigViewEmits(t *testing.T) {
	t.Parallel()
	body, err := ui.ConfigJSON(configServer(t, twoServices(), []service.Origin{
		{Service: "backend", Port: 7999, Source: service.FromRunEnv, Declared: 7100, Override: true},
	}, "/repo/.dev").configView())
	if err != nil {
		t.Fatalf("ui.ConfigJSON: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	src, _ := decoded["source"].(map[string]any)
	if len(src) == 0 {
		t.Fatal("the view has no source object for the page to read")
	}
	for key := range src {
		// "explicit" is read as src.explicit; every other key is read by name.
		if !strings.Contains(consoleHTML, "src."+key) {
			t.Errorf("console.html never reads source.%s", key)
		}
	}
}

// TestConsolePageKeepsTheMasterDetailLayout is the guard on the constraint that
// does not move: the config view is a SEPARATE view, so the service list still
// fits a five-service stack on one screen in the default view.
func TestConsolePageKeepsTheMasterDetailLayout(t *testing.T) {
	t.Parallel()
	for _, want := range []string{`id="split"`, `id="svc-list"`, `id="detail"`, `class="panel master"`} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("console.html lost %q; the master/detail layout was broken", want)
		}
	}

	// The panel starts hidden, so the page still opens on the service list.
	at := strings.Index(consoleHTML, `id="config-view"`)
	if at < 0 {
		t.Fatal("console.html has no config view")
	}
	tag := consoleHTML[at:]
	end := strings.Index(tag, ">")
	if end < 0 {
		t.Fatal("the config view's tag is unterminated")
	}
	if !strings.Contains(tag[:end], "hidden") {
		t.Errorf("the config view is not hidden by default: <%s>", tag[:end])
	}

	// Hiding a flex or grid container needs saying: an author display beats the
	// user agent's [hidden] rule, so without these the two views would overlap.
	for _, rule := range []string{".split[hidden]", ".panel[hidden]"} {
		if !strings.Contains(consoleHTML, rule) {
			t.Errorf("console.html has no %s rule, so toggling the view would not hide anything", rule)
		}
	}
}

// TestConsolePageMakesNoExternalRequest re-checks the property the CSP enforces,
// against the markup the config panel added. The panel is JSON on a loopback
// socket; a stylesheet or font pulled from a CDN would be mabo-ctl's first
// outbound call that was not a health probe.
//
// The check is on FETCHING POSITIONS rather than on the string "https://"
// anywhere in the file. It used to be the latter, which was a fine proxy while
// the page never had cause to say the word — and then the origins editor needed
// to show the operator what an origin looks like, and a placeholder in an input
// box tripped a control about outbound requests. A guard that fires on prose
// teaches people to loosen it; this one fires on the thing that actually loads
// something. The runtime enforcement is unchanged and is the real control:
// default-src 'none' with connect-src 'self', asserted by the CSP test.
func TestConsolePageMakesNoExternalRequest(t *testing.T) {
	t.Parallel()

	// Every way a page can be made to fetch a resource from somewhere else.
	for _, bad := range []string{
		`src="http`, `src='http`, `src=http`, // scripts, images, iframes
		`href="http`, `href='http`, // stylesheets and, less dangerously, links
		"url(http", "url('http", `url("http`, // CSS: fonts, background images
		"@import",                 // CSS pulling in another sheet
		`<link rel="stylesheet"`,  // an external sheet, however spelled
		"//cdn.", "http://fonts.", // the usual suspects, wherever they appear
		"integrity=", "crossorigin=", // attributes that only make sense remotely
	} {
		if strings.Contains(consoleHTML, bad) {
			t.Errorf("console.html contains %q, which would fetch from outside this process", bad)
		}
	}

	// Belt and braces on the JS side: the page talks to its own origin with
	// relative paths, so an absolute URL passed to fetch or EventSource would
	// be a new outbound channel no matter how it was spelled.
	for _, bad := range []string{
		`fetch("http`, `fetch('http`,
		`EventSource("http`, `EventSource('http`,
		`open("GET", "http`, `open('GET', 'http`,
	} {
		if strings.Contains(consoleHTML, bad) {
			t.Errorf("console.html contains %q, which would be an outbound request", bad)
		}
	}
}
