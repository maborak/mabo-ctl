package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func accessRequest(s *Server, method, path, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+recorderAddr+path, strings.NewReader(body))
	if key != "" {
		r.Header.Set(tokenHeader, key)
	}
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	return w
}

func createTestShare(t *testing.T, s *Server, role string) (string, string) {
	t.Helper()
	key := "shared-" + role + "-0123456789abcdef"
	w := accessRequest(s, "POST", "/api/access/shares", s.Token(),
		fmt.Sprintf(`{"name":"Alice","role":%q,"token":%q}`, role, key))
	if w.Code != http.StatusCreated {
		t.Fatalf("create %s share: %d %s", role, w.Code, w.Body.String())
	}
	var out struct {
		Share sharedAccess `json:"share"`
		Token string       `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Token != key || out.Share.Role != role || out.Share.ID == "" {
		t.Fatalf("unexpected create response: %s", w.Body.String())
	}
	return out.Share.ID, key
}

func TestSharedRolesAreServerEnforcedOnEveryRoute(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"read", "view", "write"} {
		t.Run(role, func(t *testing.T) {
			s := newRecorderServer(t, twoServices())
			_, key := createTestShare(t, s, role)
			for _, route := range Routes() {
				if route.Kind == RouteHealth {
					continue
				}
				r := httptest.NewRequest(route.Method, "http://"+recorderAddr+route.Path, nil)
				r.Pattern = route.Method + " " + route.Path
				allowed := mayAccess(accessGrant{Role: role}, r)
				// Independent permission matrix, not just tests of whatever
				// the implementation happens to allow this week.
				expected := false
				switch r.Pattern {
				case "GET /{$}", "GET /api/services", "GET /api/config", "GET /api/status",
					"GET /api/logs/{svc}", "GET /api/stream/all", "GET /api/stream/{svc}",
					"GET /api/events", "GET /api/history", "GET /api-docs", "GET /api/openapi.yaml", "GET /api-docs/rapidoc.js":
					expected = true
				case "GET /api/mirror":
					expected = role == "view"
				case "POST /api/start-all", "POST /api/stop-all", "POST /api/restart-all",
					"POST /api/{svc}/start", "POST /api/{svc}/stop", "POST /api/{svc}/restart":
					expected = role == "write"
				}
				if allowed != expected {
					t.Errorf("%s permission for %s = %v, want %v", role, r.Pattern, allowed, expected)
				}
				// Every terminal and administrative route must fail closed,
				// independently of whether the feature is enabled or has data.
				if strings.Contains(route.Path, "terminal") || strings.HasPrefix(route.Path, "/api/access") ||
					strings.Contains(route.Path, "origins") || route.Path == "/api/config/reload" {
					if allowed {
						t.Errorf("%s can access %s %s", role, route.Method, route.Path)
					}
				}
				if expected {
					continue // allowed streams are exercised separately; do not block here
				}
				path := strings.ReplaceAll(route.Path, "{svc}", "backend")
				path = strings.ReplaceAll(path, "{id}", "test-id")
				w := accessRequest(s, route.Method, path, key, `{}`)
				if w.Code != http.StatusForbidden {
					t.Errorf("%s %s with %s = %d, want 403: %s", route.Method, path, role, w.Code, w.Body.String())
				}
			}
			for _, path := range []string{"/api/services", "/api/status", "/api/config", "/api/history"} {
				if w := accessRequest(s, "GET", path, key, ""); w.Code != http.StatusOK {
					t.Errorf("%s read %s: %d %s", role, path, w.Code, w.Body.String())
				}
			}
			if role == "write" {
				for _, path := range []string{"/api/backend/start", "/api/backend/stop", "/api/backend/restart", "/api/start-all", "/api/stop-all", "/api/restart-all"} {
					if w := accessRequest(s, "POST", path, key, ""); w.Code != http.StatusOK {
						t.Errorf("WRITE %s: %d %s", path, w.Code, w.Body.String())
					}
				}
			}
		})
	}
}

func TestSharedPagesNeverExposeTheOperatorCredential(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())
	_, key := createTestShare(t, s, "read")
	for _, path := range []string{"/?token=" + key, "/api-docs?token=" + key, "/api/config", "/api/services"} {
		w := accessRequest(s, "GET", path, key, "")
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), s.Token()) {
			t.Fatalf("guest page leaks/refuses %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	w := accessRequest(s, "GET", "/?token="+key, key, "")
	if !strings.Contains(w.Body.String(), `name="mabo-ctl-role" content="read"`) {
		t.Fatal("guest page has wrong role")
	}
	if strings.Contains(w.Body.String(), `id="term-tabs"`) {
		t.Fatal("guest page enables the terminal")
	}
}

func TestRevocationClosesGuestStreamsAndPreventsRejoining(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())
	id, key := createTestShare(t, s, "read")
	r := httptest.NewRequest("GET", "http://"+recorderAddr+"/api/events?token="+key, nil)
	ctx, cancel, ok := s.streamContext(r)
	if !ok {
		t.Fatal("guest stream refused")
	}
	defer cancel()
	// Also verify a real event-stream handler drains on revocation.
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.h.ServeHTTP(httptest.NewRecorder(), r)
	}()
	waitFor(t, "guest event subscription", func() bool { return s.events.subscribers() == 1 })
	w := accessRequest(s, "POST", "/api/access/shares/"+id+"/revoke", s.Token(), `{}`)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	for _, c := range []<-chan struct{}{ctx.Done(), done} {
		select {
		case <-c:
		case <-time.After(2 * time.Second):
			t.Fatal("revoked guest retained a live stream")
		}
	}
	if w := accessRequest(s, "GET", "/?token="+key, "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("revoked URL = %d", w.Code)
	}
	// Never fall back from an invalid explicit credential to an owner cookie.
	for _, query := range []bool{false, true} {
		path := "/api/status"
		if query {
			path += "?token=" + key
		}
		req := httptest.NewRequest("GET", "http://"+recorderAddr+path, nil)
		if !query {
			req.Header.Set(tokenHeader, key)
		}
		req.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.Token()})
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("revoked credential fell back to owner cookie: %d", rec.Code)
		}
	}
	if err := s.SetAccessKey(key); err == nil {
		t.Error("revoked guest token was promoted to operator")
	}
	if w := accessRequest(s, "POST", "/api/access/shares", s.Token(), fmt.Sprintf(`{"name":"again","role":"write","token":%q}`, key)); w.Code != http.StatusConflict {
		t.Errorf("revoked token reuse = %d, want 409", w.Code)
	}
}

func TestGuestAccessHistoryAndRotation(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())
	shareID, key := createTestShare(t, s, "view")
	oldOwner := s.Token()
	r := httptest.NewRequest("GET", "http://"+recorderAddr+"/?token="+key, nil)
	r.Header.Set("User-Agent", "test guest browser")
	r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: oldOwner})
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `content="view"`) {
		t.Fatal("explicit share URL did not override owner cookie")
	}
	var visitor *http.Cookie
	for _, c := range w.Result().Cookies() {
		if strings.HasSuffix(c.Name, "_guest") {
			visitor = c
		} else if c.Value != key {
			t.Fatal("guest was issued the owner cookie")
		}
	}
	if visitor == nil || !visitor.HttpOnly || visitor.SameSite != http.SameSiteStrictMode {
		t.Fatal("missing safe guest tracking cookie")
	}
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "http://"+recorderAddr+"/?token="+key, nil)
		req.Header.Set("User-Agent", "test guest browser")
		req.AddCookie(visitor)
		s.h.ServeHTTP(httptest.NewRecorder(), req)
	}
	var overview accessOverview
	if err := json.Unmarshal(accessRequest(s, "GET", "/api/access", oldOwner, "").Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	if len(overview.Guests) != 1 || overview.Guests[0].Browser != "test guest browser" || overview.Guests[0].PageOpens != 3 || len(overview.Shares) != 1 {
		t.Fatalf("wrong access history: %+v", overview)
	}
	newOwner := "new-operator-credential"
	w = accessRequest(s, "POST", "/api/access/token", oldOwner, fmt.Sprintf(`{"token":%q}`, newOwner))
	if w.Code != http.StatusOK || s.Token() != newOwner {
		t.Fatalf("rotation failed: %d %s", w.Code, w.Body.String())
	}
	for _, old := range []string{oldOwner, key} {
		if w := accessRequest(s, "GET", "/api/status", old, ""); w.Code != http.StatusForbidden {
			t.Errorf("old token still works: %d", w.Code)
		}
	}
	if len(s.shares) != 0 || !s.guests[guestRecordID(visitor.Value, shareID)].Revoked {
		t.Fatal("operator rotation did not revoke guests and shares")
	}
}

func TestGuestSessionDetailsCountRequestsAndRealActionOutcomes(t *testing.T) {
	t.Parallel()
	ctrl := twoServices()
	s := newRecorderServer(t, ctrl)
	share, key := createTestShare(t, s, "write")
	page := accessRequest(s, "GET", "/?token="+key, "", "")
	var visitor *http.Cookie
	for _, c := range page.Result().Cookies() {
		if strings.HasSuffix(c.Name, "_guest") {
			visitor = c
		}
	}
	if visitor == nil {
		t.Fatal("missing guest session")
	}
	request := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://"+recorderAddr+path, nil)
		r.Header.Set(tokenHeader, key)
		r.AddCookie(visitor)
		w := httptest.NewRecorder()
		s.h.ServeHTTP(w, r)
		return w
	}
	request("GET", "/api/status?token="+key)
	if w := request("GET", "/api/access"); w.Code != http.StatusForbidden {
		t.Fatal("guest accessed session administration")
	}
	request("POST", "/api/backend/start")
	ctrl.mu.Lock()
	ctrl.opErr = fmt.Errorf("private failure contents")
	ctrl.mu.Unlock()
	// Supervisor failures use HTTP 200: outcome must come from err, not status.
	if w := request("POST", "/api/backend/restart"); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	w := accessRequest(s, "GET", "/api/access", s.Token(), "")
	var out accessOverview
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Guests) != 1 {
		t.Fatalf("guests: %+v", out.Guests)
	}
	g := out.Guests[0]
	if g.PageOpens != 1 || g.Requests != 4 || g.DeniedRequests != 1 || g.Actions != 2 {
		t.Fatalf("wrong counters: %+v", g)
	}
	if g.LastRequest != "POST /api/{svc}/restart" || g.LastAction == nil || g.LastAction.Operation != "restart" || g.LastAction.Outcome != "failed" || g.LastAction.FinishedAt == nil || len(g.LastAction.Services) != 1 || g.LastAction.Services[0] != "backend" {
		t.Fatalf("wrong last action: %+v", g)
	}
	if strings.Contains(w.Body.String(), key) || strings.Contains(w.Body.String(), "private failure contents") {
		t.Fatal("session details stored a credential/query or operation error body")
	}
	accessRequest(s, "POST", "/api/access/shares/"+share+"/revoke", s.Token(), "")
	s.mu.Lock()
	revoked := s.guests[guestRecordID(visitor.Value, share)].RevokedAt
	s.mu.Unlock()
	if revoked == nil {
		t.Fatal("revocation timestamp missing")
	}
	if err := s.SetAccessKey("replacement-owner-token"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.guests[guestRecordID(visitor.Value, share)].RevokedAt.Equal(*revoked) {
		t.Fatal("rotation overwrote original revocation time")
	}
}

func TestLatestGuestActionSurvivesOutOfOrderCompletionAndRevocation(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())
	id, key := createTestShare(t, s, "write")
	page := accessRequest(s, "GET", "/?token="+key, "", "")
	r := httptest.NewRequest("POST", "http://"+recorderAddr+"/api/backend/start", nil)
	r.Header.Set(tokenHeader, key)
	for _, c := range page.Result().Cookies() {
		r.AddCookie(c)
	}
	first := s.beginGuestAction(r, "start", []string{"backend"})
	last := s.beginGuestAction(r, "restart", []string{"frontend"})
	accessRequest(s, "POST", "/api/access/shares/"+id+"/revoke", s.Token(), "")
	last(true)
	first(false)
	var overview accessOverview
	if err := json.Unmarshal(accessRequest(s, "GET", "/api/access", s.Token(), "").Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	g := overview.Guests[0]
	if !g.Revoked || g.Actions != 2 || g.LastAction == nil || g.LastAction.Operation != "restart" || g.LastAction.Outcome != "completed" {
		t.Fatalf("older completion or revocation erased latest action: %+v", g)
	}
}

func TestGuestTrackingKeepsDifferentLinksSeparateInOneBrowser(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())
	readID, read := createTestShare(t, s, "read")
	writeID, write := createTestShare(t, s, "write")
	page := accessRequest(s, "GET", "/?token="+read, "", "")
	var visitor *http.Cookie
	for _, c := range page.Result().Cookies() {
		if strings.HasSuffix(c.Name, "_guest") {
			visitor = c
		}
	}
	if visitor == nil {
		t.Fatal("no browser cookie")
	}
	open := httptest.NewRequest("GET", "http://"+recorderAddr+"/?token="+write, nil)
	open.AddCookie(visitor)
	s.h.ServeHTTP(httptest.NewRecorder(), open)
	for _, key := range []string{read, write} {
		r := httptest.NewRequest("GET", "http://"+recorderAddr+"/api/status", nil)
		r.Header.Set(tokenHeader, key)
		r.AddCookie(visitor)
		s.h.ServeHTTP(httptest.NewRecorder(), r)
	}
	// The action's header is authoritative even if another valid link is in
	// the query. Both links share this browser's tracking cookie.
	action := httptest.NewRequest("POST", "http://"+recorderAddr+"/api/backend/start?token="+read, nil)
	action.Header.Set(tokenHeader, write)
	action.AddCookie(visitor)
	s.h.ServeHTTP(httptest.NewRecorder(), action)
	s.mu.Lock()
	for _, shareID := range []string{readID, writeID} {
		g := s.guests[guestRecordID(visitor.Value, shareID)]
		requests, actions := uint64(1), uint64(0)
		if shareID == writeID {
			requests, actions = 2, 1
		}
		if g == nil || g.Requests != requests || g.PageOpens != 1 || g.Actions != actions {
			t.Errorf("link %s lost its browser tracking: %+v", shareID, g)
		}
	}
	s.mu.Unlock()
	w := accessRequest(s, "GET", "/api/access", s.Token(), "")
	if strings.Contains(w.Body.String(), visitor.Value) {
		t.Fatal("browser cookie exposed as session ID")
	}
}

func TestGuestMutationStillRequiresAHeader(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())
	_, key := createTestShare(t, s, "write")
	r := httptest.NewRequest("POST", "http://"+recorderAddr+"/api/backend/start?token="+key, nil)
	r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: key})
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("guest mutation was authorized by query/cookie")
	}
}

func TestMirrorIsScopedValidatedAndExcludesTerminals(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())
	_, key := createTestShare(t, s, "view")
	in := mirrorView{Source: "operator-tab", Active: true, Service: "backend", Tab: "terminal", FontSize: 12, Scroll: .5}
	body, _ := json.Marshal(in)
	w := accessRequest(s, "POST", "/api/mirror", s.Token(), string(body))
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	var mirrored mirrorView
	w = accessRequest(s, "GET", "/api/mirror", key, "")
	if err := json.Unmarshal(w.Body.Bytes(), &mirrored); err != nil {
		t.Fatal(err)
	}
	if !mirrored.Active || mirrored.Tab != "output" || mirrored.Service != "backend" {
		t.Fatalf("wrong mirrored state: %+v", mirrored)
	}
	in.Source = "second-operator"
	body, _ = json.Marshal(in)
	if w := accessRequest(s, "POST", "/api/mirror", s.Token(), string(body)); w.Code != http.StatusConflict {
		t.Errorf("second operator stole broadcast: %d", w.Code)
	}
	in.Takeover = true
	body, _ = json.Marshal(in)
	if w := accessRequest(s, "POST", "/api/mirror", s.Token(), string(body)); w.Code != http.StatusOK {
		t.Errorf("explicit takeover failed: %d", w.Code)
	}
	s.mu.Lock()
	s.mirror.UpdatedAt = time.Now().Add(-6 * time.Second)
	s.mu.Unlock()
	w = accessRequest(s, "GET", "/api/mirror", key, "")
	if err := json.Unmarshal(w.Body.Bytes(), &mirrored); err != nil || mirrored.Active {
		t.Fatal("stale broadcast was not marked inactive")
	}
}

func TestShareLimitsAndUnsafeCustomTokens(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())
	for _, token := range []string{"short", "bad-token-<script>", s.Token()} {
		w := accessRequest(s, "POST", "/api/access/shares", s.Token(), fmt.Sprintf(`{"name":"guest","role":"read","token":%q}`, token))
		if w.Code < 400 {
			t.Fatalf("unsafe share token was accepted")
		}
	}
	for i := 0; i < 32; i++ {
		w := accessRequest(s, "POST", "/api/access/shares", s.Token(), `{"name":"guest","role":"read"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("share %d failed: %s", i, w.Body.String())
		}
	}
	if w := accessRequest(s, "POST", "/api/access/shares", s.Token(), `{"name":"guest","role":"read"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("share cap = %d, want 429", w.Code)
	}
}

func TestMirrorPointerIsBoundedAndNeverTracksSecurityOrTerminalRegions(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())
	_, viewer := createTestShare(t, s, "view")
	in := mirrorView{Source: "operator", Active: true, Tab: "output", FontSize: 12,
		Pointer: &mirrorPoint{Region: "output", X: .4, Y: .6},
		Click:   &mirrorClick{Point: mirrorPoint{Region: "sidebar", X: .2, Y: .3}, Sequence: 123}}
	post := func(in mirrorView) *httptest.ResponseRecorder {
		body, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		return accessRequest(s, "POST", "/api/mirror", s.Token(), string(body))
	}
	if w := post(in); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	var out mirrorView
	if err := json.Unmarshal(accessRequest(s, "GET", "/api/mirror", viewer, "").Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Pointer == nil || out.Pointer.X != .4 || out.Click == nil || out.Click.Sequence != 123 {
		t.Fatalf("pointer/click were not mirrored: %+v", out)
	}
	for _, point := range []mirrorPoint{
		{Region: "cfg-access"}, {Region: "cfg-origins"}, {Region: "terminal"}, {Region: "api"},
		{Region: "#detail"}, {Region: "output", X: -0.1}, {Region: "output", Y: 1.1},
	} {
		bad := in
		bad.Pointer = &point
		if w := post(bad); w.Code != http.StatusBadRequest {
			t.Errorf("unsafe pointer %+v accepted: %d", point, w.Code)
		}
		bad = in
		bad.Click = &mirrorClick{Point: point, Sequence: 124}
		if w := post(bad); w.Code != http.StatusBadRequest {
			t.Errorf("unsafe click %+v accepted: %d", point, w.Code)
		}
	}
	for _, sequence := range []uint64{0, 1 << 53} {
		bad := in
		bad.Click = &mirrorClick{Point: *in.Pointer, Sequence: sequence}
		if w := post(bad); w.Code != http.StatusBadRequest {
			t.Errorf("invalid click sequence %d accepted", sequence)
		}
	}
	for _, tab := range []string{"output", "terminal"} {
		in.Tab = tab
		in.Active = tab == "terminal" // both inactive sharing and terminal suppress pointers
		if w := post(in); w.Code != http.StatusOK {
			t.Fatal(w.Body.String())
		}
		var current mirrorView
		if err := json.Unmarshal(accessRequest(s, "GET", "/api/mirror", viewer, "").Body.Bytes(), &current); err != nil {
			t.Fatal(err)
		}
		if current.Pointer != nil || current.Click != nil {
			t.Fatalf("%s broadcast retained pointer/click", tab)
		}
	}
}

func TestMirrorConfigurationSectionsNeverRevealAdministrativeViews(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())
	_, viewer := createTestShare(t, s, "view")
	for _, section := range []string{"overview", "services", "sharing", "security", "unknown"} {
		in := mirrorView{Source: "operator", Active: true, Tab: "config", ConfigSection: section, FontSize: 12,
			Pointer: &mirrorPoint{Region: "cfg-source", X: .5, Y: .5}}
		body, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		w := accessRequest(s, "POST", "/api/mirror", s.Token(), string(body))
		if section == "unknown" {
			if w.Code != http.StatusBadRequest {
				t.Error("unknown CONFIG section accepted")
			}
			continue
		}
		if w.Code != http.StatusOK {
			t.Fatal(w.Body.String())
		}
		var out mirrorView
		if err := json.Unmarshal(accessRequest(s, "GET", "/api/mirror", viewer, "").Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if section == "sharing" || section == "security" {
			if out.ConfigSection != "overview" || out.Pointer != nil || out.Click != nil {
				t.Fatalf("private CONFIG section leaked: %+v", out)
			}
		} else if out.ConfigSection != section {
			t.Errorf("section %s did not mirror", section)
		}
	}
}

// TestRevokedShareCannotAcquireANewStreamContext rejects new revoked streams.
func TestRevokedShareCannotAcquireANewStreamContext(t *testing.T) {
	t.Parallel()
	s := newRecorderServer(t, twoServices())
	id, key := createTestShare(t, s, "read")
	accessRequest(s, "POST", "/api/access/shares/"+id+"/revoke", s.Token(), `{}`)
	r := httptest.NewRequest("GET", "http://"+recorderAddr+"/api/events?token="+key, nil).WithContext(context.Background())
	if _, _, ok := s.streamContext(r); ok {
		t.Fatal("revoked share acquired a new stream")
	}
}
