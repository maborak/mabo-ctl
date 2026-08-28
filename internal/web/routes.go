package web

import "net/http"

// RouteKind classifies how a console route guards itself. The classification is
// what makes the route table below both the thing the mux is BUILT from and the
// thing `mabo-ctl schema --commands` DOCUMENTS, so an agent reading the catalogue
// and an attacker probing the socket see exactly the same set.
type RouteKind string

const (
	// RouteIndex marks the console page itself. It is deliberately not wrapped
	// in [Server.requireSession]: an unauthenticated person opening it is more
	// likely a developer than an attacker, so it answers with a page carrying a
	// box to paste the token into rather than a bare refusal. It still never
	// puts the token in that response.
	RouteIndex RouteKind = "index"
	// RouteRead marks a GET route that reads state. Every one is wrapped in
	// [Server.requireSession], not just the mutating ones: the page carries the
	// session credential, and /api/status performs health probes as a side
	// effect, so an unwrapped read would hand strangers both information and a
	// request beacon.
	RouteRead RouteKind = "read"
	// RouteMutate marks a POST route that starts, stops or reconfigures the
	// stack. Wrapped in [Server.requireToken], the stricter of the two guards,
	// because anything holding such a token drives the dev stack.
	RouteMutate RouteKind = "mutate"
	// RouteHealth marks an unauthenticated health-check endpoint. Monitoring
	// tools, load balancers and CI probes need to check whether mabo-ctl's
	// web server is alive without holding a session token. The handler must
	// reveal no process state, no credentials and no information beyond
	// the fact that the server is up.
	RouteHealth RouteKind = "health"
	// RouteDocs marks the API reference page. Like [RouteIndex], it handles
	// its own session check so it can show an unlock form to an unauthenticated
	// visitor rather than a bare 403. The handler must not leak process state
	// or credentials — only the API shape.
	RouteDocs RouteKind = "docs"
)

// Route describes one HTTP route the console serves.
//
// Method names its verb explicitly — the whole reason every pattern registered
// from this table is a method-specific pattern ("GET /api/status", not "/") is
// that a GET to a mutating path must answer 405, never fall through into
// something that looks like success. Path is the ServeMux pattern without the
// method prefix; {svc} is a placeholder for a declared service name.
type Route struct {
	// Method is "GET" or "POST".
	Method string
	// Path is the route pattern, such as "/api/{svc}/start".
	Path string
	// Desc says what the route does, phrased for a machine reader.
	Desc string
	// Kind selects the guarding middleware.
	Kind RouteKind
	// Stream reports a Server-Sent-Events response: the body never ends while
	// the server runs, so a client must consume incrementally or give up.
	Stream bool
}

// consoleRoutes is the single source of truth for the console's HTTP surface.
//
// [Server.routes] registers every entry here against its handler, and
// [Routes] hands the same list to the CLI for documentation. A route that
// existed in the mux but not here would work for attackers and stay invisible
// to integrators — which is precisely the kind of drift this table exists to
// make impossible.
//
// /api/stream/all and /api/stream/{svc} are both registered verbatim from this
// table. Go's ServeMux prefers the more specific pattern, so "all" reaches the
// merged handler while every other name reaches the per-service one, which
// still rejects an unknown service — registration order plays no part.
var consoleRoutes = []Route{
	{http.MethodGet, "/{$}", "the web console page", RouteIndex, false},
	{http.MethodGet, "/health",
		"unauthenticated server liveness check; returns 200 when the web server is up",
		RouteHealth, false},
	{http.MethodGet, "/api/services",
		"every declared service as resolved: name, dir, cmd, runtime, port, health URL, dependencies, colours",
		RouteRead, false},
	{http.MethodGet, "/api/config",
		"the loaded mabo-ctl.yaml plus where each resolved port came from",
		RouteRead, false},
	{http.MethodGet, "/api/status",
		"one status per service, shaped exactly like `mabo-ctl status --json`; running probes is a side effect",
		RouteRead, false},
	{http.MethodGet, "/api/logs/{svc}",
		"the last N lines of a service log; ?tail=N chooses N",
		RouteRead, false},
	{http.MethodGet, "/api/stream/all",
		"SSE: merged log tail of every running service",
		RouteRead, true},
	{http.MethodGet, "/api/stream/{svc}",
		"SSE: live log tail of one service",
		RouteRead, true},
	{http.MethodGet, "/api/events",
		"SSE: lifecycle events (phase transitions) for the whole stack",
		RouteRead, true},
	{http.MethodGet, "/api/origins",
		"the browser origins currently trusted for cross-origin access",
		RouteRead, false},
	{http.MethodGet, "/api/history",
		"the most recent lifecycle events recorded since the console started, oldest first",
		RouteRead, false},
	{http.MethodPost, "/api/origins",
		"replace the trusted-origin list; body {\"trusted\": [...]}",
		RouteMutate, false},
	{http.MethodPost, "/api/start-all", "start every service", RouteMutate, false},
	{http.MethodPost, "/api/stop-all", "stop every service", RouteMutate, false},
	{http.MethodPost, "/api/{svc}/start", "start one named service", RouteMutate, false},
	{http.MethodPost, "/api/{svc}/stop", "stop one named service", RouteMutate, false},
	{http.MethodPost, "/api/{svc}/restart", "restart one named service", RouteMutate, false},

	// API reference and OpenAPI spec — documentation routes, not process state.
	{http.MethodGet, "/api-docs",
		"interactive API reference page",
		RouteDocs, false},
	{http.MethodGet, "/api/openapi.yaml",
		"the OpenAPI 3.0 specification in YAML",
		RouteDocs, false},
}

// Routes returns a copy of the documented route table.
//
// It is exported so `mabo-ctl schema --commands` can print the console's HTTP
// surface straight from this table: the same slice routes() registers from.
func Routes() []Route {
	return append([]Route(nil), consoleRoutes...)
}
