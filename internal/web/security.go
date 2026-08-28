package web

import (
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// tokenHeader carries the session token on every mutating request.
//
// It is a HEADER and never a query parameter or a form field, and that is the
// entire CSRF defence rather than a stylistic choice. A cross-origin page can
// make the browser send a POST — an HTML form does it without any script at all
// — but it cannot attach a custom header without triggering a CORS preflight,
// and this server answers no preflight. So the only requests that can carry
// this header are same-origin ones, i.e. the console page itself, which is the
// only thing that has ever been shown the token.
const tokenHeader = "X-Mabo-Ctl-Token"

// validateAddr rejects anything that is not host:port with a numeric port.
//
// net.SplitHostPort happily accepts a /etc/services name such as "http", and
// net.Listen resolves it. Refusing it here keeps the port a number everywhere
// else in the package — the Host check compares ports numerically, and a Host
// check that silently compares 0 to 0 would accept every host there is.
func validateAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("port %q is not a number in 0-65535", port)
	}
	return nil
}

// isLoopbackAddr reports whether addr binds only to a loopback interface.
//
// An empty host — ":7999" — is a wildcard bind and is NOT loopback, which is
// the case most likely to be typed by accident and the most dangerous one:
// it publishes the console on every interface the machine has.
//
// A name is resolved and must resolve entirely to loopback addresses. A name
// that does not resolve is reported as non-loopback with the resolution error,
// because "I could not tell" must fail closed on a control like this one.
func isLoopbackAddr(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, err
	}
	if host == "" {
		return false, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback(), nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return false, err
	}
	if len(ips) == 0 {
		return false, nil
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return false, nil
		}
	}
	return true, nil
}

// auditLog is where the console records its activity. A refused probe and a
// blocked attack used to be indistinguishable afterwards because neither left
// a trace: the guards themselves were silent on every branch, and net/http's
// own error log was pointed at io.Discard. Every refusal now lands here, as
// does every accepted mutation; the console's read-only polling does not, or
// the log would be the poller's transcript. It writes to stderr because serve
// runs in the foreground and stderr is the operator's channel; nothing here
// ever quotes a request body or a token value.
var auditLog = log.New(os.Stderr, "mabo-ctl serve ", log.LstdFlags)

// statusRecorder captures the status code a handler wrote so the audit line
// can name it. It forwards Flush because the SSE routes type-assert
// http.Flusher on the writer they are handed.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader records code and passes it through unchanged.
func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the wrapped writer when it supports flushing.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// guard is the middleware every request passes through: security headers, then
// the Host check, then the Origin check.
//
// It runs BEFORE the mux, so a request with a forged Host is refused without
// the router ever deciding what it was asking for. None of its refusals echoes
// any part of the request back to the client — a message that quotes the
// attacker's Host header is a small reflection bug for no benefit, since the
// only reader who ever sees this text is the attacker.
//
// Every refusal is recorded on [auditLog], whatever route refused it — the
// recorder sees the final status, so a bad token on a handler route and a
// forged Host here leave the same kind of trace. An accepted mutation is
// recorded too: the one network surface that can start and stop processes must
// be able to answer "who asked for that, when".
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			switch {
			case rec.status >= 400:
				auditLog.Printf("refused %d %s %s from %s",
					rec.status, r.Method, r.URL.Path, r.RemoteAddr)
			case r.Method == http.MethodPost:
				auditLog.Printf("accepted %d %s %s from %s",
					rec.status, r.Method, r.URL.Path, r.RemoteAddr)
			}
		}()
		h := rec.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		// No Access-Control-Allow-* header is ever set, here or anywhere else
		// in this package. That is what stops a cross-origin script from
		// reading the body of even the read-only routes.

		if !s.allowedHost(r.Host) && !s.trustedHost(r.Host) {
			http.Error(rec, "forbidden: the Host header does not name the address mabo-ctl is "+
				"bound to; this request looks like DNS rebinding", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !s.allowedOrigin(origin) {
			http.Error(rec, "forbidden: cross-origin request", http.StatusForbidden)
			return
		}
		next.ServeHTTP(rec, r)
	})
}

// allowedHost reports whether the Host header may be served.
//
// The rule that does the work is that a Host which is a DNS NAME is refused
// unless it is exactly "localhost". DNS rebinding works by having the victim
// visit attacker.example, whose record is flipped to 127.0.0.1 so the browser
// treats requests to it as same-origin; the connection really does arrive on
// loopback, and the only thing that still says otherwise is the Host header.
// Refusing names is therefore the whole defence, and refusing an empty Host
// with it — HTTP/1.1 requires one — closes the trivial way around it.
func (s *Server) allowedHost(hostport string) bool {
	if hostport == "" {
		return false
	}
	host, port, err := splitHostPort(hostport, 80)
	if err != nil {
		return false
	}
	if port != s.port() {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	bound, _, err := splitHostPort(s.Addr(), 0)
	if err != nil {
		return false
	}
	boundIP := net.ParseIP(bound)
	// A wildcard or loopback bind accepts any loopback literal, so ::1 and
	// 127.0.0.1 both work regardless of which family the socket sits on.
	if bound == "" || boundIP == nil || boundIP.IsLoopback() || boundIP.IsUnspecified() {
		return ip.IsLoopback()
	}
	return ip.Equal(boundIP)
}

// allowedOrigin reports whether an Origin header may drive this console.
//
// Two ways to pass. The IMPLICIT one is being the address mabo-ctl bound, which
// covers every ordinary run and is not configurable. The explicit one is being
// on the trusted list, which exists for a console reached through a tunnel or a
// port forward: the browser is then on some other hostname, so the Origin it
// sends can never equal the bound address, and every mutation would 403 with
// the page itself working perfectly — a failure that looks like a broken button
// rather than a security control.
//
// Widening this list is not what stands between a foreign page and the
// supervisor. A mutation still requires the session token in a header, which no
// cross-origin page can set without a preflight this server never answers. The
// Origin check is the layer BEHIND that one, and trusting a named https origin
// keeps it meaningful — unlike a wildcard, which NormalizeOrigin refuses.
func (s *Server) allowedOrigin(origin string) bool {
	canon, err := NormalizeOrigin(origin)
	if err != nil {
		// Not a usable origin at all — "null" from a sandboxed iframe or a
		// file:// page, a wildcard, anything with a path. Refused with
		// everything else that is not us.
		return false
	}
	if s.allowedOriginImplicit(canon) {
		return true
	}
	return s.trusted.has(canon)
}

// allowedOriginImplicit reports whether a canonical origin is the address mabo-ctl
// is bound to. It is the half of [Server.allowedOrigin] that no configuration
// can turn off, which is what makes a lockout recoverable.
func (s *Server) allowedOriginImplicit(canonical string) bool {
	host, ok := originHost(canonical)
	if !ok {
		return false
	}
	return s.allowedHost(host)
}

// trustedHost reports whether a Host header names a host the operator has
// explicitly trusted through --allow-origin or the console's origin editor.
//
// Without it the whole feature was unreachable for most tunnels. The Host check
// runs FIRST and refuses any DNS name that is not "localhost", so a proxy that
// forwards the original Host — nginx, Caddy and Cloudflare all do by default —
// was rejected before the Origin check it was configured for ever ran. The
// operator saw a DNS-rebinding refusal for a hostname they had named themselves.
//
// This does not weaken the rebinding defence. Rebinding works because the victim
// never chose attacker.example; here the host is on a list the operator wrote,
// and an entry only reaches that list through NormalizeOrigin, which refuses
// plaintext http for anything but a loopback host. The port is deliberately NOT
// compared: a tunnel terminates on 443 and forwards to mabo-ctl's own port, so
// requiring them to match would refuse every real tunnel.
func (s *Server) trustedHost(hostport string) bool {
	if hostport == "" {
		return false
	}
	host, _, err := splitHostPort(hostport, 0)
	if err != nil {
		host = hostport
	}
	host = strings.ToLower(host)
	for _, o := range s.trusted.snapshot() {
		if o == AnyOrigin {
			return true
		}
		oh, ok := originHost(o)
		if !ok {
			continue
		}
		// Compare the host alone; originHost keeps any port the entry declared.
		if h, _, err := net.SplitHostPort(oh); err == nil {
			oh = h
		}
		oh = strings.ToLower(strings.Trim(oh, "[]"))
		if oh == host {
			return true
		}
		// A "*.suffix" pattern covers a host the same way it covers an origin.
		if strings.HasPrefix(oh, "*.") && strings.HasSuffix(host, oh[1:]) {
			return true
		}
	}
	return false
}

// requireToken wraps a mutating handler with the session-token check.
//
// The comparison is constant time, and the token is read from the HEADER ALONE
// — never from the query string and never from the cookie [Server.sessionCookie]
// sets. That asymmetry with [Server.requireSession] is the CSRF defence: a
// cross-origin page can make the browser issue a POST, and a cookie would ride
// along with it, but no cross-origin page can attach a custom header without a
// preflight this server never answers.
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(tokenHeader)
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "forbidden: missing or invalid "+tokenHeader+" header; the session "+
				"token is printed with the console URL and rendered into the page",
				http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// cookieName is the per-run session cookie, named with the bound port.
//
// The port is in the NAME because cookies are not port-scoped: a cookie set by
// 127.0.0.1:7955 is sent to 127.0.0.1:7999 as well. Two consoles sharing one
// name would overwrite each other, and opening the second would silently log the
// user out of the first.
func (s *Server) cookieName() string { return "mabo-ctl_console_" + strconv.Itoa(s.port()) }

// requireSession gates the console page and the read-only routes.
//
// It exists because the page used to be served, token and all, to anyone who
// asked. Every mutating route was wrapped in [Server.requireToken], and then the
// index handed that very token to any unauthenticated caller in a meta tag — so
// a process running as another uid on a shared host could GET /, scrape the
// token and drive start/stop/restart as the developer. The token guarded the
// door and was posted on it.
//
// Three sources are accepted, and ALL of them are checked before refusing,
// rather than the first one present winning: the browser sends the cookie of
// every console on this loopback host, so a stale cookie from another run must
// not veto a perfectly good token in the query.
//
//   - the query parameter, which is what the printed URL carries and the only
//     credential a browser navigation can present;
//   - the cookie, which is what makes a page RELOAD work — the page strips the
//     token from the address bar on load, so without this an F5 would 403;
//   - the header, for scripts and curl.
//
// The cookie is SameSite=Strict, which is what makes it safe here and answers
// the objection recorded on requireToken: a Strict cookie is not attached to
// cross-site requests at all, so it cannot be used by a foreign page to read
// these routes. It is also HttpOnly — the page reads its token from the meta
// tag, never from document.cookie.
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, fromQuery := s.session(r)
		if !ok {
			http.Error(w, "forbidden: this console requires its session token. Open the URL "+
				"mabo-ctl printed, which carries it, or send it as the "+tokenHeader+" header.",
				http.StatusForbidden)
			return
		}
		// Only a credential that arrived in the URL mints the cookie. Refreshing
		// it from a request that already had one would extend a session on every
		// poll for no benefit.
		if fromQuery {
			s.setSessionCookie(w)
		}
		next.ServeHTTP(w, r)
	})
}

// session reports whether r carries the session token, and whether it arrived in
// the QUERY — which is the only case that mints a cookie.
//
// All three sources are checked before refusing, rather than the first one
// present winning: the browser sends the cookie of every console on this
// loopback host, so a stale cookie from another run must not veto a perfectly
// good token in the query.
//
// It is separate from [Server.requireSession] because the console page does not
// want that middleware's refusal. A 403 with no body is the right answer for an
// API route and the wrong one for a person who opened a bookmark without the
// token on it; [Server.handleIndex] asks this question itself and answers with a
// page they can type into.
func (s *Server) session(r *http.Request) (ok, fromQuery bool) {
	fromQuery = s.tokenMatches(r.URL.Query().Get("token"))
	if fromQuery || s.tokenMatches(r.Header.Get(tokenHeader)) {
		return true, fromQuery
	}
	if c, err := r.Cookie(s.cookieName()); err == nil && s.tokenMatches(c.Value) {
		return true, false
	}
	return false, false
}

// tokenMatches compares got against the session token in constant time.
func (s *Server) tokenMatches(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

// setSessionCookie issues the cookie that lets a reload of the console page
// succeed after the page has stripped the token from the address bar.
//
// Secure is set exactly when a non-loopback origin is trusted, and the
// conditional is the whole point. On the default loopback console there is no
// TLS at all and an unconditional Secure cookie would simply never be stored,
// breaking the reload this cookie exists for. But --allow-origin exists to put
// the console behind a tunnel, NormalizeOrigin refuses plaintext http for any
// non-loopback host precisely because "anyone on the network could claim it",
// and this cookie carries the token that authenticates that session — so
// leaving it transportable over cleartext would withhold from the cookie the
// one guarantee that check was written to provide. One plaintext request to the
// tunnel hostname would hand over the whole session.
//
// MaxAge is left at zero so it is a session cookie, gone when the browser
// closes — the token it carries dies with the mabo-ctl process anyway.
func (s *Server) setSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    s.token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.trustsANonLoopbackOrigin(),
		SameSite: http.SameSiteStrictMode,
	})
}

// trustsANonLoopbackOrigin reports whether any trusted origin names something
// other than this machine — which is the case where the session can travel over
// a network and therefore must not travel in cleartext.
func (s *Server) trustsANonLoopbackOrigin() bool {
	for _, o := range s.trusted.snapshot() {
		if o == AnyOrigin {
			return true
		}
		host, ok := originHost(o)
		if !ok || !isLoopbackHostPort(host) {
			return true
		}
	}
	return false
}
