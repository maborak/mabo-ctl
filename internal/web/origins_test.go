package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNormalizeOriginCanonicalises pins that comparison happens on a canonical
// form. Raw string equality is how allowlists get bypassed: "HTTPS://X" and
// "https://x/" are the same origin and would both miss an exact-match test.
func TestNormalizeOriginCanonicalises(t *testing.T) {
	t.Parallel()
	ok := map[string]string{
		"https://tunnel.example":      "https://tunnel.example",
		"HTTPS://Tunnel.Example":      "https://tunnel.example",
		"https://tunnel.example/":     "https://tunnel.example",
		"https://tunnel.example:8443": "https://tunnel.example:8443",
		"  https://tunnel.example  ":  "https://tunnel.example",
		"http://localhost:3000":       "http://localhost:3000",
		"http://127.0.0.1:7999":       "http://127.0.0.1:7999",
		"http://[::1]:7999":           "http://[::1]:7999",
	}
	for in, want := range ok {
		got, err := NormalizeOrigin(in)
		if err != nil {
			t.Errorf("NormalizeOrigin(%q) = error %v, want %q", in, err, want)
			continue
		}
		if got != want {
			t.Errorf("NormalizeOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNormalizeOriginRefusesTheDangerousShapes covers the values that would
// turn the origin check off while looking like configuration.
func TestNormalizeOriginRefusesTheDangerousShapes(t *testing.T) {
	t.Parallel()
	bad := []string{
		"",                            // nothing
		"*",                           // would disable the check
		"null",                        // a sandboxed iframe / file:// page
		"NULL",                        //   …in any spelling
		"http://tunnel.example",       // plaintext on a public name: spoofable
		"https://user:pw@tunnel.test", // credentials are not part of an origin
		"https://tunnel.example/path", // a path is not part of an origin
		"https://tunnel.example/?a=b", // nor a query
		"ftp://tunnel.example",        // not a browser origin
		"tunnel.example",              // no scheme
		"https://",                    // no host
	}
	for _, in := range bad {
		if got, err := NormalizeOrigin(in); err == nil {
			t.Errorf("NormalizeOrigin(%q) = %q, want an error", in, got)
		}
	}
}

// TestATrustedOriginMayDriveTheConsole is the whole point of the feature: a
// console reached through a tunnel sends an Origin that can never equal the
// bound address, so without this every button 403s while the page works.
func TestATrustedOriginMayDriveTheConsole(t *testing.T) {
	t.Parallel()
	const tunnel = "https://dev.tunnel.example"
	ctrl := twoServices()
	s, err := NewWith(ctrl, Options{Addr: recorderAddr, AllowedOrigins: []string{tunnel}})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+"/api/backend/start", nil)
	req.Header.Set("Origin", tunnel)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST from a trusted origin = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if n := ctrl.callCount(); n != 1 {
		t.Errorf("supervisor calls = %d, want 1", n)
	}
}

// TestAnUntrustedOriginIsStillRefused pins that the list is an allowlist and
// not an off switch.
func TestAnUntrustedOriginIsStillRefused(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	s, err := NewWith(ctrl, Options{Addr: recorderAddr, AllowedOrigins: []string{"https://dev.tunnel.example"}})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+"/api/backend/start", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST from an untrusted origin = %d, want 403", rec.Code)
	}
	if n := ctrl.callCount(); n != 0 {
		t.Errorf("%d mutations reached the supervisor, want 0", n)
	}
}

// TestATrustedOriginStillNeedsTheToken keeps the two controls independent. The
// origin list is the layer BEHIND the token, not a replacement for it.
func TestATrustedOriginStillNeedsTheToken(t *testing.T) {
	t.Parallel()
	const tunnel = "https://dev.tunnel.example"
	ctrl := twoServices()
	s, err := NewWith(ctrl, Options{Addr: recorderAddr, AllowedOrigins: []string{tunnel}})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+"/api/backend/start", nil)
	req.Header.Set("Origin", tunnel)
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST from a trusted origin without a token = %d, want 403", rec.Code)
	}
	if n := ctrl.callCount(); n != 0 {
		t.Errorf("%d unauthenticated mutations reached the supervisor, want 0", n)
	}
}

// TestNewRejectsAMistypedAllowOrigin: a bad --allow-origin must fail before the
// socket is bound, not become a button that silently does nothing.
func TestNewRejectsAMistypedAllowOrigin(t *testing.T) {
	t.Parallel()
	if _, err := NewWith(twoServices(), Options{Addr: recorderAddr, AllowedOrigins: []string{"*"}}); err == nil {
		t.Fatal("NewWith accepted a wildcard origin")
	}
}

// postOrigins performs POST /api/origins with the given Origin header.
func postOrigins(t *testing.T, s *Server, origin string, list []string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string][]string{"trusted": list})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+"/api/origins", strings.NewReader(string(body)))
	req.Header.Set(tokenHeader, s.Token())
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	return rec
}

// TestOriginsCanBeEditedAtRuntime covers why the route exists: a tunnel is set
// up AFTER mabo-ctl is already supervising a stack, and restarting mabo-ctl to add
// a hostname would stop every service.
func TestOriginsCanBeEditedAtRuntime(t *testing.T) {
	t.Parallel()
	const tunnel = "https://dev.tunnel.example"
	ctrl := twoServices()
	s, err := NewWith(ctrl, Options{Addr: recorderAddr})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}

	// Before: refused.
	req := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+"/api/backend/start", nil)
	req.Header.Set("Origin", tunnel)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("before the edit, POST = %d, want 403", rec.Code)
	}

	// Add it from the loopback console.
	if got := postOrigins(t, s, "", []string{tunnel}); got.Code != http.StatusOK {
		t.Fatalf("POST /api/origins = %d, want 200 (body %s)", got.Code, got.Body.String())
	}

	// After: accepted, with no restart.
	req2 := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+"/api/backend/start", nil)
	req2.Header.Set("Origin", tunnel)
	req2.Header.Set(tokenHeader, s.Token())
	rec2 := httptest.NewRecorder()
	s.h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("after the edit, POST = %d, want 200", rec2.Code)
	}
}

// TestEditingOriginsCannotLockTheEditorOut is the guard the operator asked for.
// Deleting the entry you are currently browsing from would leave you looking at
// a console whose every button 403s, including the one that would put it back.
func TestEditingOriginsCannotLockTheEditorOut(t *testing.T) {
	t.Parallel()
	const tunnel = "https://dev.tunnel.example"
	s, err := NewWith(twoServices(), Options{Addr: recorderAddr, AllowedOrigins: []string{tunnel}})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}

	got := postOrigins(t, s, tunnel, []string{})
	if got.Code != http.StatusConflict {
		t.Fatalf("removing your own origin = %d, want 409 (body %s)", got.Code, got.Body.String())
	}
	if !strings.Contains(got.Body.String(), tunnel) {
		t.Errorf("the refusal does not name the origin at risk: %s", got.Body.String())
	}
	// And it really did not apply — all or nothing.
	if list := s.TrustedOrigins(); len(list) != 1 || list[0] != tunnel {
		t.Errorf("TrustedOrigins = %v, want the list unchanged", list)
	}
}

// TestLoopbackCanAlwaysRecoverALockout is the other half of the guard: the
// bound address is accepted unconditionally, so a browser on 127.0.0.1 can
// always edit the list back — which is why the refusal above can be a
// convenience rather than the only thing standing between the operator and a
// console they cannot drive.
func TestLoopbackCanAlwaysRecoverALockout(t *testing.T) {
	t.Parallel()
	const tunnel = "https://dev.tunnel.example"
	s, err := NewWith(twoServices(), Options{Addr: recorderAddr, AllowedOrigins: []string{tunnel}})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}

	// From the bound loopback address, clearing the list is allowed.
	got := postOrigins(t, s, "http://"+recorderAddr, []string{})
	if got.Code != http.StatusOK {
		t.Fatalf("clearing the list from loopback = %d, want 200 (body %s)", got.Code, got.Body.String())
	}
	if list := s.TrustedOrigins(); len(list) != 0 {
		t.Errorf("TrustedOrigins = %v, want empty", list)
	}
}

// TestSettingOriginsIsAllOrNothing: one bad entry must not half-apply a list.
func TestSettingOriginsIsAllOrNothing(t *testing.T) {
	t.Parallel()
	const good = "https://a.example"
	s, err := NewWith(twoServices(), Options{Addr: recorderAddr, AllowedOrigins: []string{good}})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}

	got := postOrigins(t, s, "", []string{"https://b.example", "*"})
	if got.Code != http.StatusBadRequest {
		t.Fatalf("a list containing a wildcard = %d, want 400", got.Code)
	}
	if list := s.TrustedOrigins(); len(list) != 1 || list[0] != good {
		t.Errorf("TrustedOrigins = %v, want the original list untouched", list)
	}
}

// TestOriginsRouteIsTokenGuarded: a page that could add its own origin would
// have promoted itself out of the very check it was failing.
func TestOriginsRouteIsTokenGuarded(t *testing.T) {
	t.Parallel()
	s, err := NewWith(twoServices(), Options{Addr: recorderAddr})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}

	body := strings.NewReader(`{"trusted":["https://evil.example"]}`)
	req := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+"/api/origins", body)
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated POST /api/origins = %d, want 403", rec.Code)
	}
	if list := s.TrustedOrigins(); len(list) != 0 {
		t.Errorf("TrustedOrigins = %v, want empty", list)
	}
}

// TestSubdomainPatternsCoverEphemeralTunnelHosts is why patterns exist. Tunnel
// providers hand out a fresh subdomain per run, so naming one host means
// editing the list every morning.
func TestSubdomainPatternsCoverEphemeralTunnelHosts(t *testing.T) {
	t.Parallel()
	const pattern = "https://*.tunnel.example"
	s, err := NewWith(twoServices(), Options{Addr: recorderAddr, AllowedOrigins: []string{pattern}})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}

	covered := []string{
		"https://a.tunnel.example",
		"https://wilmer-mabo-ctl.tunnel.example",
		"https://deep.nested.tunnel.example",
	}
	for _, o := range covered {
		if !s.allowedOrigin(o) {
			t.Errorf("%s is not covered by %s, but should be", o, pattern)
		}
	}

	// The leading dot is load-bearing: a pattern must not become a substring
	// match that a look-alike domain can satisfy.
	rejected := []string{
		"https://eviltunnel.example",      // no dot boundary
		"https://tunnel.example.evil.com", // suffix in the wrong place
		"http://a.tunnel.example",         // wrong scheme
		"https://a.tunnel.example:8443",   // pattern named no port
		"https://tunnel.example",          // the bare domain is not a subdomain
	}
	for _, o := range rejected {
		if s.allowedOrigin(o) {
			t.Errorf("%s is covered by %s, but must not be", o, pattern)
		}
	}
}

// TestTooBroadAPatternIsRefused: "*.com" hands the trust to a registry.
func TestTooBroadAPatternIsRefused(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"https://*.com", "https://*.local", "https://*.", "https://*"} {
		if got, err := NormalizeOrigin(bad); err == nil {
			t.Errorf("NormalizeOrigin(%q) = %q, want an error", bad, got)
		}
	}
}

// TestBareWildcardNeedsTheDangerFlag: "*" is the one entry that stops the list
// being a list, so it is gated on the flag the operator uses to mean it.
func TestBareWildcardNeedsTheDangerFlag(t *testing.T) {
	t.Parallel()
	if _, err := NormalizeOrigin("*"); err == nil {
		t.Error("NormalizeOrigin accepted a bare wildcard")
	}
	if _, err := NormalizeOriginAllowingAny("*"); err != nil {
		t.Errorf("NormalizeOriginAllowingAny(*) = %v, want it accepted", err)
	}

	// Without Force the server refuses it, with Force it takes it.
	if _, err := NewWith(twoServices(), Options{Addr: recorderAddr, AllowedOrigins: []string{"*"}}); err == nil {
		t.Error("NewWith accepted * without Force")
	}
	s, err := NewWith(twoServices(), Options{Addr: recorderAddr, Force: true, AllowedOrigins: []string{"*"}})
	if err != nil {
		t.Fatalf("NewWith with Force: %v", err)
	}
	if !s.allowedOrigin("https://anything.example") {
		t.Error("* does not match an arbitrary origin")
	}
}

// TestAPatternKeepsTheLockoutGuardHonest: the guard asks whether the caller is
// still COVERED, not whether its exact string survived, or removing a redundant
// exact entry beside a pattern would be refused for no reason.
func TestAPatternKeepsTheLockoutGuardHonest(t *testing.T) {
	t.Parallel()
	s, err := NewWith(twoServices(), Options{
		Addr:           recorderAddr,
		AllowedOrigins: []string{"https://*.tunnel.example", "https://a.tunnel.example"},
	})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}

	// Dropping the exact entry is fine: the pattern still covers the caller.
	got := postOrigins(t, s, "https://a.tunnel.example", []string{"https://*.tunnel.example"})
	if got.Code != http.StatusOK {
		t.Fatalf("dropping a redundant exact entry = %d, want 200 (body %s)", got.Code, got.Body.String())
	}

	// Dropping the pattern too is the real lockout.
	got2 := postOrigins(t, s, "https://a.tunnel.example", []string{})
	if got2.Code != http.StatusConflict {
		t.Fatalf("dropping the last covering entry = %d, want 409", got2.Code)
	}
}

// TestCookieIsSecureOnlyWhenANonLoopbackOriginIsTrusted covers both halves of a
// conditional that has to be a conditional.
//
// Unconditionally Secure breaks the default console: it is plain HTTP on
// loopback, a Secure cookie is never stored, and the reload the cookie exists
// for stops working. Never Secure breaks the tunnel case: --allow-origin exists
// to put the console behind HTTPS, and a cookie carrying the session token
// would then be transportable over cleartext — withholding from the cookie the
// exact guarantee NormalizeOrigin enforces https to provide.
func TestCookieIsSecureOnlyWhenANonLoopbackOriginIsTrusted(t *testing.T) {
	t.Parallel()
	cookieFor := func(t *testing.T, allowed ...string) *http.Cookie {
		t.Helper()
		s, err := NewWith(twoServices(), Options{Addr: recorderAddr, AllowedOrigins: allowed})
		if err != nil {
			t.Fatalf("NewWith: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "http://"+recorderAddr+"/?token="+s.Token(), nil)
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, req)
		for _, c := range rec.Result().Cookies() {
			if c.Name == s.cookieName() {
				return c
			}
		}
		t.Fatal("no session cookie was set")
		return nil
	}

	if c := cookieFor(t); c.Secure {
		t.Error("the loopback console sets a Secure cookie, which a browser will not store over http — the reload would 403")
	}
	if c := cookieFor(t, "https://dev.tunnel.example"); !c.Secure {
		t.Error("a console trusting an https tunnel origin sets a cookie that may travel in cleartext")
	}
	// A trusted loopback origin is still this machine; no TLS is involved.
	if c := cookieFor(t, "http://localhost:3000"); c.Secure {
		t.Error("trusting a loopback origin should not make the cookie Secure")
	}
}

// TestATrustedHostPassesTheHostCheck is what makes --allow-origin reachable.
//
// The Host check runs first and refuses any DNS name that is not "localhost".
// nginx, Caddy and Cloudflare all forward the original Host by default, so the
// tunnel hostname was rejected before the Origin check it was configured for
// ever ran — the operator saw a DNS-rebinding refusal for a name they had
// themselves added to the trust list.
func TestATrustedHostPassesTheHostCheck(t *testing.T) {
	t.Parallel()
	const tunnel = "https://dev.tunnel.example"
	ctrl := twoServices()
	s, err := NewWith(ctrl, Options{Addr: recorderAddr, AllowedOrigins: []string{tunnel}})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+"/api/backend/start", nil)
	req.Host = "dev.tunnel.example" // the proxy forwarded the original Host
	req.Header.Set("Origin", tunnel)
	req.Header.Set(tokenHeader, s.Token())
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST with the trusted tunnel Host = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// An untrusted DNS name is still refused: this widens the check for names
	// the operator wrote down, and for nothing else.
	req2 := httptest.NewRequest(http.MethodPost, "http://"+recorderAddr+"/api/backend/start", nil)
	req2.Host = "evil.example"
	req2.Header.Set(tokenHeader, s.Token())
	rec2 := httptest.NewRecorder()
	s.h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("POST with an untrusted Host = %d, want 403", rec2.Code)
	}
}

// TestUnlockPageDoesNotInviteAPasswordManager: the session token is an
// ephemeral capability that dies with the process, and a password input asks
// the browser to save and sync it. Chrome ignores autocomplete="off" there.
func TestUnlockPageDoesNotInviteAPasswordManager(t *testing.T) {
	t.Parallel()
	// Check the INPUT, not the file: the stylesheet comment explains why a
	// password field is not used, and a naive substring search matches that
	// explanation — a test that fails on its own rationale.
	for _, line := range strings.Split(unlockHTML, "\n") {
		if strings.Contains(line, "<input") && strings.Contains(line, `type="password"`) {
			t.Errorf("the unlock page uses a password input, so the browser will offer to "+
				"store the session token: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(unlockHTML, `name="token"`) {
		t.Error("the unlock page has no token field at all")
	}
	if !strings.Contains(unlockHTML, "text-security") {
		t.Error("the unlock page no longer masks the token as it is typed")
	}
}
