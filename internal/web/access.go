package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/maborak/mabo-ctl/internal/redact"
)

// accessGrant is a capability, not a user account. Only the operator token is
// an owner; shared credentials can never acquire administrative privileges.
type accessGrant struct {
	Role       string
	Credential string
	ShareID    string
	Life       context.Context
}

type sharedAccess struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	hash      [32]byte
	life      context.Context
	cancel    context.CancelFunc
}

type guestAccess struct {
	ID             string       `json:"id"`
	ShareID        string       `json:"share_id"`
	Name           string       `json:"name"`
	Role           string       `json:"role"`
	Address        string       `json:"address"`
	Browser        string       `json:"browser"`
	JoinedAt       time.Time    `json:"joined_at"`
	LastSeen       time.Time    `json:"last_seen"`
	Revoked        bool         `json:"revoked"`
	RevokedAt      *time.Time   `json:"revoked_at,omitempty"`
	PageOpens      uint64       `json:"page_opens"`
	Requests       uint64       `json:"requests"`
	DeniedRequests uint64       `json:"denied_requests"`
	Actions        uint64       `json:"actions"`
	LastRequest    string       `json:"last_request,omitempty"`
	LastAction     *guestAction `json:"last_action,omitempty"`
}

// guestAction is the latest accepted lifecycle action, not an activity history.
// Instances are immutable once published so an access snapshot can marshal
// outside the server lock while an operation completes.
type guestAction struct {
	Operation  string     `json:"operation"`
	Services   []string   `json:"services"`
	Outcome    string     `json:"outcome"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// mirrorView excludes terminal state, credentials, clipboard and security forms.
type mirrorView struct {
	Source        string       `json:"source"`
	Active        bool         `json:"active"`
	Takeover      bool         `json:"takeover,omitempty"`
	UpdatedAt     time.Time    `json:"updated_at"`
	Service       string       `json:"service"`
	Tab           string       `json:"tab"`
	ConfigSection string       `json:"config_section,omitempty"`
	All           bool         `json:"all"`
	Filter        string       `json:"filter"`
	Level         string       `json:"level"`
	Paused        bool         `json:"paused"`
	Follow        bool         `json:"follow"`
	Scroll        float64      `json:"scroll"`
	Separators    bool         `json:"separators"`
	Pretty        bool         `json:"pretty"`
	FontSize      int          `json:"font_size"`
	Details       bool         `json:"details"`
	Pointer       *mirrorPoint `json:"pointer,omitempty"`
	Click         *mirrorClick `json:"click,omitempty"`
}

// mirrorPoint uses a closed set of non-security regions, never DOM selectors,
// element text or input values. Coordinates are normalized within the region.
type mirrorPoint struct {
	Region string  `json:"region"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
}

type mirrorClick struct {
	Point    mirrorPoint `json:"point"`
	Sequence uint64      `json:"sequence"`
}

func validMirrorPoint(p *mirrorPoint) bool {
	if p == nil {
		return true
	}
	if p.X < 0 || p.X > 1 || p.Y < 0 || p.Y > 1 {
		return false
	}
	switch p.Region {
	case "sidebar", "tabs", "output", "cfg-source", "cfg-ports", "cfg-services":
		return true
	default:
		return false
	}
}

type accessOverview struct {
	Shares []sharedAccess `json:"shares"`
	Guests []guestAccess  `json:"guests"`
}

// credentialAccess checks and captures a credential's lifetime under the same
// lock that rotates/revokes it. Share tokens are stored as hashes, not plaintext.
func (s *Server) credentialAccess(key string) (accessGrant, bool) {
	if key == "" {
		return accessGrant{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if subtle.ConstantTimeCompare([]byte(key), []byte(s.token)) == 1 {
		return accessGrant{Role: "owner", Credential: key, Life: s.tokenLife}, true
	}
	hash := sha256.Sum256([]byte(key))
	for _, share := range s.shares {
		if subtle.ConstantTimeCompare(hash[:], share.hash[:]) == 1 {
			return accessGrant{Role: share.Role, Credential: key, ShareID: share.ID, Life: share.life}, true
		}
	}
	return accessGrant{}, false
}

func (s *Server) requestAccess(r *http.Request) (accessGrant, bool, bool) {
	// A valid explicit share URL must win over an existing operator cookie.
	if key := r.URL.Query().Get("token"); key != "" {
		a, ok := s.credentialAccess(key)
		return a, true, ok
	}
	if key := r.Header.Get(tokenHeader); key != "" {
		a, ok := s.credentialAccess(key)
		return a, false, ok
	}
	if c, err := r.Cookie(s.cookieName()); err == nil {
		if a, ok := s.credentialAccess(c.Value); ok {
			return a, false, true
		}
	}
	return accessGrant{}, false, false
}

// mayAccess defaults new routes to owner-only access.
func mayAccess(a accessGrant, r *http.Request) bool {
	if a.Role == "owner" {
		return true
	}
	switch r.Pattern {
	case "GET /{$}", "GET /api/services", "GET /api/config", "GET /api/status",
		"GET /api/logs/{svc}", "GET /api/stream/all", "GET /api/stream/{svc}",
		"GET /api/events", "GET /api/history", "GET /api-docs",
		"GET /api/openapi.yaml", "GET /api-docs/rapidoc.js":
		return true
	case "GET /api/mirror":
		return a.Role == "view"
	case "POST /api/start-all", "POST /api/stop-all", "POST /api/restart-all",
		"POST /api/{svc}/start", "POST /api/{svc}/stop", "POST /api/{svc}/restart":
		return a.Role == "write"
	default:
		return false
	}
}

func (s *Server) touchGuest(r *http.Request, a accessGrant) {
	s.noteGuestRequest(r, a, false)
}

// guestRecordID scopes the browser cookie to one capability without exposing
// that cookie in the access list. Different links cannot displace one another's
// tracking, even when their pages share the same browser cookie jar.
func guestRecordID(browserID, shareID string) string {
	hash := sha256.Sum256([]byte(browserID + ":" + shareID))
	return hex.EncodeToString(hash[:])
}

func (s *Server) requestGuestID(r *http.Request, shareID string) (string, bool) {
	c, err := r.Cookie(s.cookieName() + "_guest")
	if err != nil || len(c.Value) != 64 || validateAccessKey(c.Value) != nil {
		return "", false
	}
	return guestRecordID(c.Value, shareID), true
}

func peerAddress(r *http.Request) string {
	address, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return address
}

func (s *Server) noteGuestRequest(r *http.Request, a accessGrant, denied bool) {
	if a.ShareID == "" {
		return
	}
	id, ok := s.requestGuestID(r, a.ShareID)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.guests[id]; g != nil && g.ShareID == a.ShareID && !g.Revoked {
		g.LastSeen = time.Now().UTC()
		g.Address = peerAddress(r)
		g.Requests++
		if denied {
			g.DeniedRequests++
		}
		g.LastRequest = r.Pattern // route template only: never query strings or raw user paths
	}
}

// beginGuestAction attributes an accepted operation before it starts. The
// completion callback still works if access is revoked while the operation
// finishes, and an older completion cannot overwrite a newer action.
func (s *Server) beginGuestAction(r *http.Request, operation string, services []string) func(bool) {
	// Mutations authenticate the header alone. A query token must not change
	// which guest gets credited for the action authorized by that header.
	a, ok := s.credentialAccess(r.Header.Get(tokenHeader))
	if !ok || a.ShareID == "" {
		return func(bool) {}
	}
	id, ok := s.requestGuestID(r, a.ShareID)
	if !ok {
		return func(bool) {}
	}
	action := &guestAction{Operation: operation, Services: append([]string{}, services...), Outcome: "running", StartedAt: time.Now().UTC()}
	s.mu.Lock()
	g := s.guests[id]
	if g == nil || g.ShareID != a.ShareID || g.Revoked {
		s.mu.Unlock()
		return func(bool) {}
	}
	g.Actions++
	g.LastAction = action
	s.mu.Unlock()
	return func(ok bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		g := s.guests[id]
		if g == nil || g.LastAction != action {
			return
		}
		finished := *action
		now := time.Now().UTC()
		finished.FinishedAt = &now
		finished.Outcome = "completed"
		if !ok {
			finished.Outcome = "failed"
		}
		g.LastAction = &finished
	}
}

// revokeGuestsLocked marks the affected visitor history without changing a
// previous revocation timestamp. An empty share ID revokes every guest.
func (s *Server) revokeGuestsLocked(shareID string) {
	now := time.Now().UTC()
	for _, g := range s.guests {
		if !g.Revoked && (shareID == "" || g.ShareID == shareID) {
			g.Revoked = true
			g.RevokedAt = &now
		}
	}
}

func (s *Server) joinGuest(w http.ResponseWriter, r *http.Request, a accessGrant) error {
	if a.ShareID == "" {
		return nil
	}
	cookieName := s.cookieName() + "_guest"
	secure := s.trustsANonLoopbackOrigin()
	var browserID string
	if c, err := r.Cookie(cookieName); err == nil && len(c.Value) == 64 && validateAccessKey(c.Value) == nil {
		browserID = c.Value
	}
	if browserID == "" {
		var err error
		browserID, err = newToken()
		if err != nil {
			return err
		}
	}
	id := guestRecordID(browserID, a.ShareID)
	s.mu.Lock()
	defer s.mu.Unlock()
	share := s.shares[a.ShareID]
	if share == nil || a.Life.Err() != nil {
		return fmt.Errorf("share access was revoked")
	}
	if g := s.guests[id]; g != nil && g.ShareID == a.ShareID && !g.Revoked {
		g.LastSeen = time.Now().UTC()
		g.PageOpens++
		g.Address = peerAddress(r)
		g.Browser = redact.Value("browser", truncate(r.UserAgent(), 256))
		return nil
	}
	if s.guests == nil {
		s.guests = make(map[string]*guestAccess)
	}
	// Bounded connection history, including visitors who have left the page.
	if len(s.guests) >= 256 {
		var oldest *guestAccess
		for _, g := range s.guests {
			if oldest == nil || g.LastSeen.Before(oldest.LastSeen) {
				oldest = g
			}
		}
		delete(s.guests, oldest.ID)
	}
	now := time.Now().UTC()
	s.guests[id] = &guestAccess{ID: id, ShareID: share.ID, Name: share.Name, Role: a.Role,
		Address: peerAddress(r), Browser: redact.Value("browser", truncate(r.UserAgent(), 256)), JoinedAt: now, LastSeen: now, PageOpens: 1}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: browserID, Path: "/",
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode})
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func decodeAccessBody(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return false
	}
	return true
}

func (s *Server) handleAccess(w http.ResponseWriter, _ *http.Request) {
	out := accessOverview{Shares: []sharedAccess{}, Guests: []guestAccess{}}
	s.mu.Lock()
	for _, share := range s.shares {
		out.Shares = append(out.Shares, *share)
	}
	for _, guest := range s.guests {
		out.Guests = append(out.Guests, *guest)
	}
	s.mu.Unlock()
	sort.Slice(out.Shares, func(i, j int) bool { return out.Shares[i].CreatedAt.Before(out.Shares[j].CreatedAt) })
	sort.Slice(out.Guests, func(i, j int) bool { return out.Guests[i].JoinedAt.Before(out.Guests[j].JoinedAt) })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name  string `json:"name"`
		Role  string `json:"role"`
		Token string `json:"token"`
	}
	if !decodeAccessBody(w, r, &in) {
		return
	}
	if in.Role != "read" && in.Role != "view" && in.Role != "write" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "role must be read, view or write"})
		return
	}
	if len(in.Name) == 0 || len(in.Name) > 80 || strings.ContainsAny(in.Name, "\r\n") {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "share name must be 1–80 characters on one line"})
		return
	}
	if in.Token == "" {
		var err error
		in.Token, err = newToken()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "could not generate share token"})
			return
		}
	}
	if len(in.Token) < 16 || len(in.Token) > 256 || validateAccessKey(in.Token) != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "share token must be 16–256 ASCII letters, digits, '.', '_', '~' or '-'"})
		return
	}
	id, err := newToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "could not generate share ID"})
		return
	}
	hash := sha256.Sum256([]byte(in.Token))
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.shares) >= 32 || len(s.usedShareHashes) >= 128 {
		writeJSON(w, http.StatusTooManyRequests, errorResponse{Error: "share limit reached (32 active, 128 created per serve run)"})
		return
	}
	if _, used := s.usedShareHashes[hash]; used || in.Token == s.token {
		writeJSON(w, http.StatusConflict, errorResponse{Error: "choose a new token; operator and previous share tokens cannot be reused"})
		return
	}
	if s.shares == nil {
		s.shares = make(map[string]*sharedAccess)
	}
	if s.usedShareHashes == nil {
		s.usedShareHashes = make(map[[32]byte]struct{})
	}
	life, cancel := context.WithCancel(context.Background())
	share := &sharedAccess{ID: id, Name: redact.Value("share name", in.Name), Role: in.Role,
		CreatedAt: time.Now().UTC(), hash: hash, life: life, cancel: cancel}
	s.shares[id] = share
	s.usedShareHashes[hash] = struct{}{}
	writeJSON(w, http.StatusCreated, struct {
		Share *sharedAccess `json:"share"`
		Token string        `json:"token"`
	}{share, in.Token})
}

func (s *Server) handleRevokeShare(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	share := s.shares[id]
	if share == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "unknown share"})
		return
	}
	share.cancel()
	delete(s.shares, id)
	s.revokeGuestsLocked(id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRotateToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !decodeAccessBody(w, r, &in) {
		return
	}
	if in.Token == "" {
		var err error
		in.Token, err = newToken()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "could not generate token"})
			return
		}
	}
	if len(in.Token) > 256 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "token is too long"})
		return
	}
	if in.Token == s.sessionToken() {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "the new token must differ from the current operator token"})
		return
	}
	if err := s.SetAccessKey(in.Token); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	s.setCredentialCookie(w, in.Token)
	writeJSON(w, http.StatusOK, struct {
		Token string `json:"token"`
	}{in.Token})
}

func (s *Server) handleGetMirror(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	out := s.mirror
	s.mu.Unlock()
	if time.Since(out.UpdatedAt) > 5*time.Second {
		out.Active = false
		out.Pointer, out.Click = nil, nil
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSetMirror(w http.ResponseWriter, r *http.Request) {
	var in mirrorView
	if !decodeAccessBody(w, r, &in) {
		return
	}
	if len(in.Source) == 0 || len(in.Source) > 80 || len(in.Filter) > 512 ||
		in.Scroll < 0 || in.Scroll > 1 || in.FontSize < 10 || in.FontSize > 18 ||
		!validMirrorPoint(in.Pointer) || (in.Click != nil &&
		(!validMirrorPoint(&in.Click.Point) || in.Click.Sequence == 0 || in.Click.Sequence > 1<<53-1)) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid mirror presentation state"})
		return
	}
	if in.Tab != "output" && in.Tab != "config" && in.Tab != "api" && in.Tab != "terminal" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid mirror tab"})
		return
	}
	switch in.ConfigSection {
	case "", "overview":
		in.ConfigSection = "overview"
	case "services":
	case "sharing", "security":
		// Guests never receive administrative sections, even their pointer
		// positions. They see the read-only overview while these are open.
		in.ConfigSection = "overview"
		if in.Tab == "config" {
			in.Pointer, in.Click = nil, nil
		}
	default:
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid mirror configuration section"})
		return
	}
	// Terminal/security settings are never broadcast, including when the
	// operator switches into the terminal while a VIEW guest is connected.
	if in.Tab == "terminal" {
		in.Tab = "output"
		in.Pointer, in.Click = nil, nil
	}
	if !in.Active {
		in.Pointer, in.Click = nil, nil
	}
	switch in.Level {
	case "", "error", "warn", "info", "debug":
	default:
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid mirror level"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.Service != "" {
		if _, ok := s.known[in.Service]; !ok {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "unknown mirrored service"})
			return
		}
	}
	if s.mirror.Active && s.mirror.Source != in.Source && time.Since(s.mirror.UpdatedAt) < 5*time.Second && !in.Takeover {
		writeJSON(w, http.StatusConflict, errorResponse{Error: "another operator tab is broadcasting; enable Broadcast this tab to take over"})
		return
	}
	in.UpdatedAt = time.Now().UTC()
	in.Takeover = false
	in.Filter = redact.Value("log filter", in.Filter)
	s.mirror = in
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
