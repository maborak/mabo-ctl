// Package web serves mabo-ctl's browser console: one embedded HTML page plus a
// small JSON and server-sent-events API that the page drives.
//
// # This package is a remote-code-execution surface
//
// Until it existed mabo-ctl had no listening socket at all. It now has one, and
// three of its routes start, stop and restart the commands written in
// mabo-ctl.yaml. Anything that can reach the socket and satisfy its checks can
// run those commands. Every control in this package exists because of that
// sentence, and none of them is optional:
//
//   - The default bind address is loopback. [New] REFUSES a non-loopback
//     address unless Options.Force is set, because a dev supervisor reachable
//     from the LAN lets anyone on the same wifi run your build scripts.
//   - A 32-byte token is generated at startup, rendered into the page, and
//     required as an X-Mabo-Ctl-Token HEADER on every mutation. A cross-origin
//     page can neither read the token nor set a custom header without a CORS
//     preflight this server never answers.
//   - Every request's Host header must name the address mabo-ctl is bound to, and
//     a Host that is a DNS name other than "localhost" is refused outright.
//     That is the DNS-rebinding defence: rebinding works by making the victim's
//     browser treat attacker.example as 127.0.0.1, and the Host header still
//     says attacker.example.
//   - An Origin header, when present, must be this same origin.
//   - Mutations are POST only. A GET is reachable from an <img> tag.
//   - No response ever carries a CORS header, so a cross-origin reader cannot
//     see a body even on the read-only routes.
//   - /api/services and /api/config render the DECLARED environment of a
//     service, keys always and values only when the key does not look like a
//     credential. The resolved service.Instance.Env — the caller's whole
//     environment — is never rendered anywhere. Health URLs and command
//     arguments go through the same redaction on both routes: a credential is a
//     credential whichever field it was written in.
//
// # Layering
//
// Like internal/console this package imports neither os/exec nor syscall.
// Every process operation goes through the [Controller]. Options.Open is
// likewise NOT acted on here: opening a browser means spawning one, so the
// field records the caller's intent and cmd/mabo-ctl performs the open using the
// address [Server.URL] reports.
//
// # Streams
//
// Both SSE routes tie their lifetime to the request context, so a closed
// browser tab cancels the supervisor.Tail behind it. A tab-per-service that
// leaked a tail goroutine per open would be this package's defining bug; the
// stream handlers cancel, drain and then wait for the producer before they
// return, and server_test.go asserts the goroutine count comes back down.
//
// Lifecycle events fan out through a [broker] rather than being read straight
// off a supervisor channel: supervisor.Event sends are non-blocking, so a
// single channel drained by one handler would silently drop events for every
// other open tab.
//
// # Wire format
//
// Both SSE routes emit UNNAMED events — the browser's EventSource delivers them
// to onmessage — whose data is a single line of JSON. /api/stream/{svc} sends
// {"service":…,"line":…} per log line; /api/events sends
// {"service":…,"phase":…,"msg":…,"error":…}. A ": heartbeat" comment goes out
// every 15s so an idle stream is never mistaken for a dead one.
package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// DefaultAddr is where the console listens when Options.Addr is empty. It is a
// loopback address on purpose: see the package documentation.
const DefaultAddr = "127.0.0.1:7999"

// Tuning constants. They are constants rather than options because none of them
// is a decision a user of mabo-ctl should have to make.
const (
	// heartbeatInterval is how often an idle SSE stream emits a comment. A
	// stream that says nothing for minutes is indistinguishable from a broken
	// one, both to a proxy that reaps idle connections and to the page.
	heartbeatInterval = 15 * time.Second

	// shutdownTimeout bounds the graceful shutdown. Request contexts are
	// cancelled first, so this is the budget for in-flight handlers to notice,
	// not for them to finish work.
	shutdownTimeout = 5 * time.Second

	// readHeaderTimeout bounds how long a connection may take to send its
	// headers. Without it a single idle socket holds a goroutine forever.
	readHeaderTimeout = 10 * time.Second

	// idleTimeout closes keep-alive connections that go quiet. SSE streams are
	// never idle — they heartbeat — so this does not affect them.
	idleTimeout = 2 * time.Minute

	// defaultLogTail and maxLogTail bound ?tail=N. The cap exists so one
	// request cannot ask the supervisor to read an unbounded number of lines
	// into memory.
	defaultLogTail = 200
	maxLogTail     = 5000

	// streamBuffer is the channel depth between supervisor.Tail and an SSE
	// handler. A starting service emits a burst; the burst is absorbed here
	// rather than blocking the tail between two writes to the socket.
	streamBuffer = 256

	// maxInflightMutations bounds concurrent start/stop/restart operations. A
	// start blocks for the readiness timeout, so without a bound a page that
	// hammers the button would accumulate goroutines holding real processes.
	maxInflightMutations = 8

	// maxCollectedEvents caps what one mutation response carries back. The
	// events also go to /api/events, so truncating the response body loses
	// nothing that a connected page has not already seen.
	maxCollectedEvents = 500

	// defaultOpTimeout bounds one mutation when the config declares no
	// timeouts of its own.
	defaultOpTimeout = 2 * time.Minute
)

// Errors reported by this package.
var (
	// ErrNoController reports that a Server was asked for without anything to
	// supervise. It is returned rather than deferred to a nil dereference in a
	// handler, because a nil supervisor is a mistake in the caller and should
	// be reported before a socket is bound.
	ErrNoController = errors.New("web: no supervisor")

	// ErrUnsafeAddr reports a bind address that is not loopback, offered
	// without Options.Force. The message names the address and says what it
	// exposes; see the package documentation for why this is fatal by default.
	ErrUnsafeAddr = errors.New("web: refusing to serve on a non-loopback address")
)

// Controller is the slice of *supervisor.Supervisor that the console drives.
//
// It exists so the whole HTTP surface can be exercised without spawning a
// single process: the tests substitute a fake and assert on what it was, and
// was not, asked to do — including that an unknown service name never reaches
// it at all. *supervisor.Supervisor satisfies it by construction and the
// signatures are copied verbatim from the supervisor API.
type Controller interface {
	// Instances returns the resolved services in declaration order. The set is
	// read once, at construction: it is what {svc} is validated against.
	Instances() []service.Instance
	// Config returns the parsed mabo-ctl.yaml, or nil when there is none. It is
	// the source of the DECLARED environment; Instances carries the resolved
	// one, which must never be rendered.
	Config() *config.Config
	// Status reports the current phase of every service. It may issue health
	// probes and therefore may block.
	Status(ctx context.Context) []supervisor.Status
	// Start starts names (empty means every service), reporting progress on ev.
	Start(ctx context.Context, names []string, ev chan<- supervisor.Event) error
	// Stop stops names (empty means every service).
	Stop(ctx context.Context, names []string, ev chan<- supervisor.Event) error
	// Restart stops then starts names (empty means every service).
	Restart(ctx context.Context, names []string, ev chan<- supervisor.Event) error
	// Tail sends svc's log lines to out until ctx is done. It closes out as it
	// returns.
	Tail(ctx context.Context, svc string, n int, follow bool, out chan<- string) error
}

// Options configures the server.
type Options struct {
	// Addr is the TCP address to bind, "host:port". Empty means [DefaultAddr].
	// A non-loopback host requires Force.
	Addr string

	// Force permits a non-loopback bind. It is the programmatic form of
	// --i-know-this-is-dangerous and it means exactly what it says: every
	// machine that can route to Addr can run the commands in mabo-ctl.yaml,
	// subject only to knowing the session token.
	Force bool

	// Open records that the caller wants a browser opened once the socket is
	// listening. This package does NOT act on it: opening a browser means
	// spawning a process, and web imports neither os/exec nor syscall. The
	// caller reads the field itself and opens [Server.URL] after
	// [Server.Listen] returns.
	Open bool

	// Origins explains where every service's resolved port came from, as
	// service.Resolve reported it. /api/config renders it, and it is the whole
	// point of the console's config panel: port precedence has four levels and
	// nothing else in mabo-ctl shows an operator which one won.
	//
	// It is passed in rather than derived because the resolution happens once,
	// in cmd/mabo-ctl, with inputs this package never sees — the --ports flag and
	// the caller's captured <NAME>_PORT variables. Re-deriving it here would be
	// a second implementation of the precedence chain, which is how two answers
	// to one question get shipped. When it is empty /api/config reports an
	// empty port_source, which a reader renders as unknown rather than as a
	// default mabo-ctl never claimed.
	Origins []service.Origin

	// StateDir is the absolute path of the .dev/ state directory, for display
	// on /api/config. It is supplied rather than composed from the config root
	// because internal/state owns the layout under .dev/ and is the only
	// package that may name it.
	StateDir string

	// ExplicitConfig reports that the loaded mabo-ctl.yaml came from --config
	// rather than from walking up the directory tree. It is worth showing:
	// discovery walks UP, so the file that won can belong to a parent
	// repository the operator was not thinking of.
	ExplicitConfig bool

	// AllowedOrigins seeds the origins the console accepts IN ADDITION to the
	// address it binds. It is the programmatic form of --allow-origin.
	//
	// It exists for a console reached through a tunnel or a port forward. The
	// browser is then on another hostname, so the Origin it sends can never
	// equal the bound address and every mutation is refused while the page
	// itself works — which reads as a broken button, not as a control doing its
	// job. Entries are validated by [NormalizeOrigin]: https only, unless the
	// host is loopback, and never a wildcard.
	//
	// The list is editable at runtime through /api/origins, because a tunnel is
	// usually set up AFTER mabo-ctl is already supervising services and
	// restarting to add a hostname would stop the whole stack.
	AllowedOrigins []string
}

// Server serves the console over HTTP. Construct one with [New] or [NewWith];
// the zero value is not usable.
type Server struct {
	ctrl Controller
	opts Options

	// token is the session token: 32 random bytes, hex encoded. It is compared
	// in constant time and is the only thing standing between a page the user
	// happens to be visiting and a start/stop of their services.
	token string

	// tmpl is the parsed console page, or nil when the page could not be
	// parsed as a template and must be served verbatim instead.
	tmpl *template.Template

	// names and known are the declared service names, read once at
	// construction. {svc} is validated against known before it reaches
	// anything else.
	names []string
	known map[string]struct{}

	// origins, stateDir and explicitConfig back /api/config. They are the
	// matching Options fields verbatim, and are read-only after construction:
	// the resolution they describe ran once, before the socket was bound.
	origins        []service.Origin
	stateDir       string
	explicitConfig bool

	// trusted is the mutable allowlist of origins accepted on top of the bound
	// address. Unlike the fields above it is NOT read-only after construction:
	// /api/origins edits it while the console is running.
	trusted originSet

	h      http.Handler
	events *broker

	// heartbeat is the SSE keep-alive period. It is a field only so the tests
	// do not have to wait 15 seconds to observe one.
	heartbeat time.Duration

	// inflight is a counting semaphore over concurrent mutations.
	inflight chan struct{}

	// streams counts live SSE handlers. The leak test reads it.
	streams atomic.Int64

	mu   sync.Mutex
	ln   net.Listener
	addr string
}

// New returns a Server over sup. It generates the session token.
//
// It returns an error when sup is nil, when opt.Addr is not a valid host:port,
// or when opt.Addr is not loopback and opt.Force is false — the last wrapping
// [ErrUnsafeAddr] and explaining what the address would expose.
func New(sup *supervisor.Supervisor, opt Options) (*Server, error) {
	if sup == nil {
		return nil, ErrNoController
	}
	return NewWith(sup, opt)
}

// CheckAddr reports whether addr may be served, WITHOUT binding anything.
//
// It is the same decision [New] makes, exported so a caller can make it early.
// That matters because the check is a security control and the caller may have
// work to do first: `mabo-ctl start --web-console --web-addr 0.0.0.0:0` used to
// start every service and only then discover it would refuse to bind — and on a
// non-terminal it never reached the check at all, so the same command line was
// refused with exit 2 on a tty and accepted with exit 0 through a pipe.
//
// It is exported rather than duplicated on purpose. A second copy of a
// loopback predicate is how one of them silently stops matching, and the
// difference between the two would be a bind that should not have happened.
// [New] calls this, so there is exactly one implementation.
//
// force corresponds to Options.Force and to --i-know-this-is-dangerous: it
// permits a non-loopback address and nothing else. An empty addr means
// [DefaultAddr], which is loopback.
func CheckAddr(addr string, force bool) error {
	if addr == "" {
		addr = DefaultAddr
	}
	if err := validateAddr(addr); err != nil {
		return fmt.Errorf("web: invalid address %q: %w", addr, err)
	}
	loopback, err := isLoopbackAddr(addr)
	if err != nil {
		return fmt.Errorf("web: invalid address %q: %w", addr, err)
	}
	if !loopback && !force {
		return fmt.Errorf("%w: %s is reachable from other machines, which lets "+
			"anyone who can route to it run the commands in mabo-ctl.yaml; pass "+
			"--i-know-this-is-dangerous to override", ErrUnsafeAddr, addr)
	}
	return nil
}

// NewWith is [New] over any [Controller]. It exists so the console can be
// served over something other than a *supervisor.Supervisor — in practice, so
// the tests can assert on a fake without spawning processes.
func NewWith(ctrl Controller, opt Options) (*Server, error) {
	if ctrl == nil {
		return nil, ErrNoController
	}
	if opt.Addr == "" {
		opt.Addr = DefaultAddr
	}

	if err := CheckAddr(opt.Addr, opt.Force); err != nil {
		return nil, err
	}

	token, err := newToken()
	if err != nil {
		return nil, err
	}

	s := &Server{
		ctrl:      ctrl,
		opts:      opt,
		token:     token,
		addr:      opt.Addr,
		heartbeat: heartbeatInterval,
		events:    newBroker(),
		inflight:  make(chan struct{}, maxInflightMutations),
		origins:   append([]service.Origin(nil), opt.Origins...),
		stateDir:  opt.StateDir,

		explicitConfig: opt.ExplicitConfig,
	}

	// Seeded before the socket is bound, and fatal when invalid: a mistyped
	// --allow-origin must be reported as a usage error, not accepted silently
	// and then discovered as a button that does nothing.
	s.trusted.allowAny = opt.Force
	if err := s.trusted.add(opt.AllowedOrigins...); err != nil {
		return nil, err
	}

	for _, in := range ctrl.Instances() {
		s.names = append(s.names, in.Name)
	}
	s.known = make(map[string]struct{}, len(s.names))
	for _, n := range s.names {
		s.known[n] = struct{}{}
	}

	// A page that will not parse as a template is served verbatim with the
	// token injected instead of failing the whole command: the console is more
	// useful degraded than absent, and the page is written by hand.
	if t, err := templateFor(consoleHTML); err == nil {
		s.tmpl = t
	}

	s.h = s.guard(s.routes())
	return s, nil
}

// routes builds the mux FROM [consoleRoutes], the table that is also what
// `mabo-ctl schema --commands` documents. One list, two consumers: a route the
// mux serves is exactly a route the catalogue names, and neither can drift.
//
// Every pattern names its method, which is what makes a GET to a mutating path
// a 405 rather than a fallthrough.
//
// The page is registered at "/{$}" — the exact root — and NOT at "/". A "GET /"
// pattern matches every path, so a GET to /api/backend/start would render the
// console page with 200 instead of being refused as a method violation, and the
// "mutations are POST only" rule would be decoration.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// Every read route is wrapped too, not just the mutating ones. The index
	// carries the session token, so serving it unauthenticated handed the
	// write credential to anyone who could open a socket; and /api/status
	// performs health probes as a side effect, so leaving it open let any page
	// the developer visited use mabo-ctl as a request beacon. See
	// [Server.requireSession] for why its accepted credentials differ from
	// [Server.requireToken]'s.
	get := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.requireSession(h))
	}
	// Mutations take the stricter guard, per [RouteMutate].
	post := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.requireToken(http.HandlerFunc(h)))
	}

	// Keyed by method+path, because one path may legitimately serve two
	// different handlers under two different guards: GET /api/origins reads the
	// list, POST /api/origins replaces it.
	handlers := map[string]http.HandlerFunc{
		"GET /{$}":              s.handleIndex,
		"GET /health":           s.handleHealth,
		"GET /api/services":     s.handleServices,
		"GET /api/config":       s.handleConfig,
		"GET /api/status":       s.handleStatus,
		"GET /api/logs/{svc}":   s.handleLogs,
		"GET /api/stream/all":   s.handleStreamAll,
		"GET /api/stream/{svc}": s.handleStream,
		"GET /api/events":       s.handleEvents,
		"GET /api/origins":      s.handleGetOrigins,
		"GET /api/history":      s.handleHistory,

		"POST /api/origins":       s.handleSetOrigins,
		"POST /api/start-all":     s.handleStartAll,
		"POST /api/stop-all":      s.handleStopAll,
		"POST /api/{svc}/start":   s.handleStart,
		"POST /api/{svc}/stop":    s.handleStop,
		"POST /api/{svc}/restart": s.handleRestart,

		"GET /api-docs":            s.handleDocs,
		"GET /api/openapi.yaml":    s.handleOpenAPI,
		"GET /api-docs/rapidoc.js": s.handleRapidoc,
	}

	for _, r := range consoleRoutes {
		// Editing the trust list is a mutation and is guarded like one: POST
		// only, session token in the header. A cross-origin page that could add
		// its own origin would have promoted itself out of the check it was
		// failing — which is why GET and POST of /api/origins differ only in
		// their guard level, not in their presence in this table.
		var wrap func(pattern string, h http.HandlerFunc)
		switch r.Kind {
		case RouteIndex:
			// NOT wrapped: handleIndex does its own session check so it can
			// answer an unauthenticated person with a box to paste the token
			// into rather than a bare 403. It still never puts the token in
			// that response.
			wrap = func(pattern string, h http.HandlerFunc) {
				mux.HandleFunc(pattern, h)
			}
		case RouteHealth:
			// NOT wrapped: monitoring tools, load balancers and CI probes
			// need liveness without a session token. The handler must not
			// reveal process state, credentials or anything beyond liveness.
			wrap = func(pattern string, h http.HandlerFunc) {
				mux.HandleFunc(pattern, h)
			}
		case RouteRead:
			wrap = get
		case RouteMutate:
			wrap = post
		case RouteDocs:
			// NOT wrapped: like RouteIndex, the handler does its own
			// session check so it can show an unlock form to unauthenticated
			// visitors rather than a bare 403. The UNAUTHENTICATED response
			// contains no token and no process state; the authenticated one
			// carries the session token for the page's try-it playground,
			// exactly as the console page does.
			wrap = func(pattern string, h http.HandlerFunc) {
				mux.HandleFunc(pattern, h)
			}
		default:
			panic("web: console route " + r.Method + " " + r.Path + " has unknown kind " + string(r.Kind))
		}
		h := handlers[r.Method+" "+r.Path]
		if h == nil {
			panic("web: console route " + r.Method + " " + r.Path + " has no handler")
		}
		wrap(r.Method+" "+r.Path, h)
	}

	return mux
}

// Token returns the session token required on every mutating request.
//
// It is exported so the caller can print the URL that carries it. Treat it as a
// credential: anything holding it can start and stop the services.
func (s *Server) Token() string { return s.token }

// Addr reports the address actually bound. Before [Server.Listen] it reports
// the address that will be bound, which differs only when the configured port
// is 0.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// URL is the address a human should open, token included.
//
// A wildcard bind is reported as loopback: "0.0.0.0:7999" is not something a
// browser can open, and the whole point of printing a URL is that it works.
func (s *Server) URL() string {
	host, port, err := splitHostPort(s.Addr(), 80)
	if err != nil {
		return "http://" + s.Addr() + "/?token=" + s.token
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		host = "[" + host + "]"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/?token=" + s.token
}

// Listen binds the socket and records the resolved address.
//
// It is separate from [Server.ListenAndServe] so a caller can bind BEFORE it
// prints the URL: a URL printed from the requested address is a guess, and a
// guess is wrong exactly when port 0 was requested or the port was already
// taken. Listen is idempotent — the second call is a no-op.
func (s *Server) Listen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return nil
	}
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("web: listening on %s: %w", s.opts.Addr, err)
	}
	s.ln = ln
	s.addr = ln.Addr().String()
	return nil
}

// ListenAndServe blocks until ctx is done, then shuts down gracefully.
//
// Shutdown cancels every in-flight request context first and only then waits.
// http.Server.Shutdown on its own waits for handlers to return, and an SSE
// handler never returns on its own — it would hold the process open until the
// timeout expired on every single stream. Cancelling the base context is what
// turns that into an immediate, clean close.
//
// Shutting the console down does not stop any supervised service: they are
// spawned detached, and closing a window is not a shutdown request.
//
// A Server is single-use: shutdown releases every event subscription for good,
// so serve a fresh Server rather than reusing one.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.Listen(); err != nil {
		return err
	}

	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()

	base, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()

	srv := &http.Server{
		Handler:           s.h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// No WriteTimeout: an SSE stream is a response that legitimately never
		// ends, and a write deadline would sever every log pane on a timer.
		BaseContext: func(net.Listener) context.Context { return base },
		ErrorLog:    auditLog,
	}

	stopped := make(chan error, 1)
	go func() {
		<-ctx.Done()
		cancelBase()
		s.events.close()
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		stopped <- srv.Shutdown(sctx)
	}()

	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}

	// Serve returns as soon as Shutdown is called, so the shutdown result is
	// still in flight; waiting for it is what makes "ListenAndServe returned"
	// mean "the socket is closed and the handlers are done".
	select {
	case serr := <-stopped:
		if err == nil && serr != nil && !errors.Is(serr, context.DeadlineExceeded) {
			err = serr
		}
	case <-time.After(shutdownTimeout):
	}

	s.mu.Lock()
	s.ln = nil
	s.mu.Unlock()
	return err
}

// port reports the port the server is bound to, or will bind to.
func (s *Server) port() int {
	_, p, err := splitHostPort(s.Addr(), 0)
	if err != nil {
		return 0
	}
	return p
}

// opTimeout bounds one start/stop/restart. It is derived from the declared
// timeouts so a config with a long readiness budget is not cut short by a
// constant chosen here.
func (s *Server) opTimeout() time.Duration {
	cfg := s.ctrl.Config()
	if cfg == nil {
		return defaultOpTimeout
	}
	d := 2*cfg.ReadyTimeout + 2*cfg.StopGrace + 30*time.Second
	if d < defaultOpTimeout {
		return defaultOpTimeout
	}
	return d
}

// splitHostPort splits a "host:port" or bare "host", returning defPort when no
// port is present. An empty host is valid and means "every interface"; it is
// the caller's job to decide what that implies.
func splitHostPort(hostport string, defPort int) (host string, port int, err error) {
	h, p, err := net.SplitHostPort(hostport)
	if err != nil {
		// "Host: example.com" with no port is legal HTTP, and so is "[::1]".
		// Anything else containing a colon is malformed rather than portless.
		if strings.Contains(hostport, ":") && !strings.HasPrefix(hostport, "[") {
			return "", 0, err
		}
		h = strings.Trim(hostport, "[]")
		if h == "" {
			return "", 0, errors.New("empty host")
		}
		return h, defPort, nil
	}
	n, cerr := strconv.Atoi(p)
	if cerr != nil || n < 0 || n > 65535 {
		return "", 0, fmt.Errorf("invalid port %q", p)
	}
	return h, n, nil
}

// originHost extracts the host:port of an Origin header value, and reports
// whether it was a well-formed http(s) origin at all.
func originHost(origin string) (string, bool) {
	u, err := url.Parse(origin)
	if err != nil {
		return "", false
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return "", false
	}
	if u.Host == "" {
		return "", false
	}
	return u.Host, true
}

// templateFor parses the console page. html/template rather than text/template
// so that anything the page interpolates is contextually escaped: the page is
// the one place in this package where a value becomes markup.
func templateFor(src string) (*template.Template, error) {
	return template.New("console").Parse(src)
}

// newToken returns 32 cryptographically random bytes, hex encoded.
//
// Hex rather than base64 because the token is printed in a URL, pasted into
// terminals, and injected into HTML: an alphabet of [0-9a-f] has no character
// that needs escaping in any of those places, which removes a whole class of
// "the token worked yesterday" bug.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("web: generating session token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
