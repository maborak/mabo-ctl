package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTheDocumentedRoutesAreTheServedRoutes pins the contract between
// [consoleRoutes] and the mux [Server.routes] builds from it.
//
// The table is documentation — `mabo-ctl schema --commands` prints it to any
// agent that asks — so it lying is worse than a route being missing: an
// integrator would be told a mutation exists when its guard refuses, or that a
// read needs nothing when it needs a session. These probes go through s.h so
// they exercise exactly what a real socket answers, guards included.
func TestTheDocumentedRoutesAreTheServedRoutes(t *testing.T) {
	t.Parallel()
	s, err := NewWith(twoServices(), Options{Addr: recorderAddr})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}

	table := Routes()
	if len(table) == 0 {
		t.Fatal("consoleRoutes is empty")
	}
	seen := map[string]bool{}
	for _, r := range table {
		key := r.Method + " " + r.Path
		if seen[key] {
			t.Errorf("route %s documented twice", key)
		}
		seen[key] = true

		switch r.Method {
		case http.MethodGet, http.MethodPost:
		default:
			t.Errorf("route %s uses method %q; the console speaks only GET and POST", key, r.Method)
		}
		// Streams are SSE GETs; claiming stream on anything else is a lie an
		// integrator's HTTP client would discover mid-body.
		if r.Stream && r.Kind != RouteRead {
			t.Errorf("route %s claims stream but is kind %q", key, r.Kind)
		}
	}

	documented := func(method, path string) bool { return seen[method+" "+path] }
	for _, r := range table {
		path := strings.ReplaceAll(r.Path, "{svc}", "backend")
		switch r.Kind {
		case RouteIndex:
			// The pattern /{$} matches EXACTLY "/", which is what a browser
			// opening the printed URL sends.
			if code := do(s, http.MethodGet, "/", false); code != http.StatusUnauthorized {
				t.Errorf("GET / unauthenticated = %d, want 401", code)
			}
			if r.Method != http.MethodGet || r.Path != "/{$}" {
				t.Errorf("index route is %s; the page lives at exactly GET /{$}", r.Method+" "+r.Path)
			}
		case RouteHealth:
			// Health routes must be accessible without authentication.
			if code := do(s, http.MethodGet, path, false); code != http.StatusOK {
				t.Errorf("%s unauthenticated = %d, want 200 (health must be open)", key2(r), code)
			}
		case RouteRead:
			// Every read route is session-gated; no credential means no data.
			if code := do(s, http.MethodGet, path, false); code != http.StatusForbidden {
				t.Errorf("%s unauthenticated = %d, want 403", key2(r), code)
			}
			// A wrong verb answers the mux's method mismatch, unless the very
			// same path is itself documented under another verb (/api/origins).
			if !documented(http.MethodPost, r.Path) {
				if code := do(s, http.MethodPost, path, true); code != http.StatusMethodNotAllowed {
					t.Errorf("POST %s = %d, want 405 (reads are GET-only)", key2(r), code)
				}
			}
		case RouteMutate:
			// Mutations are POST-only: the wrong verb must be refused by the
			// mux itself, never fall through into a handler.
			if !documented(http.MethodGet, r.Path) {
				if code := do(s, http.MethodGet, path, true); code != http.StatusMethodNotAllowed {
					t.Errorf("GET %s = %d, want 405 (mutations are POST-only)", key2(r), code)
				}
			}
			// POST without the token is refused before anything runs.
			if code := do(s, http.MethodPost, path, false); code != http.StatusForbidden {
				t.Errorf("POST %s without token = %d, want 403", key2(r), code)
			}
		default:
			t.Errorf("route %s has unknown kind %q", r.Method+" "+r.Path, r.Kind)
		}
	}
}

// key2 names one route in failure output.
func key2(r Route) string { return r.Method + " " + r.Path }

// do issues one request through the server's own handler chain, optionally
// carrying the session token (header or query).
func do(s *Server, method, path string, withToken bool) int {
	target := "http://" + recorderAddr + path
	req := httptest.NewRequest(method, target, nil)
	if withToken {
		req.Header.Set(tokenHeader, s.Token())
	} else {
		req.Header.Set(tokenHeader, "not-the-token")
	}
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	return rec.Code
}
