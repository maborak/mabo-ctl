package web

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// xtermJS is the terminal emulator the Terminal tab renders with: xterm.js
// and its fit and web-links addons, vendored verbatim with their licence.
// It is served from this origin because the console loads nothing remote.
//
//go:embed xterm-bundle.js
var xtermJS string

// maxTerminalInput bounds one input request. A paste larger than this arrives
// as several requests, in order, from the page's input queue.
const maxTerminalInput = 1 << 20

// terminalInfo is one open terminal as the console renders it.
type terminalInfo struct {
	ID        string `json:"id"`
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

// terminalChunk is one SSE frame on a terminal stream: output bytes, base64
// because pty output is not guaranteed to be valid UTF-8 at a chunk boundary,
// or the end of the terminal.
//
// Replay marks the scrollback frame. The page must not answer anything in it:
// the scrollback holds the queries programs sent to the terminal they had then
// (device attributes, colour reports, mode requests), and an emulator that
// answers them again on reattach types the answers into whatever is reading
// the pty now — usually the shell prompt.
type terminalChunk struct {
	Data   string `json:"data,omitempty"`
	Replay bool   `json:"replay,omitempty"`
	End    bool   `json:"end,omitempty"`
}

func toTerminalInfo(t supervisor.TerminalInfo) terminalInfo {
	return terminalInfo{ID: t.ID, PID: t.PID, StartedAt: t.StartedAt.UTC().Format(time.RFC3339)}
}

// terminalAllowed is the gate every terminal route passes after the session or
// token check its route kind already applied.
//
// A terminal is a shell, which is strictly more than the declared commands the
// rest of the console can run, so it is held to a stricter rule than the rest:
// it exists only when the operator opted in, and only for a browser on THIS
// machine talking to the loopback address directly. The Host must be the bound
// loopback address itself — not a name an operator trusted for a tunnel — an
// Origin, which every POST carries, must be that same address, and the request
// must not carry the headers a relaying proxy adds. A console
// reached through --allow-origin keeps every other tab and loses this one.
func (s *Server) terminalAllowed(w http.ResponseWriter, r *http.Request) bool {
	if s.terminals == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "the terminal is off; start the console with " +
			"`mabo-ctl serve --terminal` or set console_terminal: true in mabo-ctl.yaml"})
		return false
	}
	local := s.allowedHost(r.Host) && !proxied(r)
	if origin := r.Header.Get("Origin"); origin != "" {
		canon, err := normalizeBoundOrigin(origin)
		local = local && err == nil && s.allowedOriginImplicit(canon)
	}
	if !local {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "forbidden: the terminal is served only to " +
			"a browser on this machine using the console's loopback address"})
		return false
	}
	return true
}

// forwardedHeaders are the headers a proxy adds when it relays a request. A
// tunnel daemon on this machine connects from loopback and may rewrite Host to
// the loopback address, and an EventSource GET carries no Origin, so without
// this check a browser behind such a tunnel could still read a terminal's
// output. It is defence in depth, not a proof: a proxy that adds none of them
// is indistinguishable from a local browser.
var forwardedHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip", "Cf-Connecting-Ip"}

func proxied(r *http.Request) bool {
	for _, h := range forwardedHeaders {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	return false
}

// terminalParam resolves {id} to an open terminal.
func (s *Server) terminalParam(w http.ResponseWriter, r *http.Request) (*supervisor.Terminal, bool) {
	term, err := s.terminals.Get(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "unknown terminal"})
		return nil, false
	}
	return term, true
}

// handleXterm serves the terminal emulator bundle.
func (s *Server) handleXterm(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xtermJS))
}

// handleTerminals lists the open terminals, oldest first.
func (s *Server) handleTerminals(w http.ResponseWriter, r *http.Request) {
	if !s.terminalAllowed(w, r) {
		return
	}
	list := s.terminals.List()
	out := make([]terminalInfo, 0, len(list))
	for _, t := range list {
		out = append(out, toTerminalInfo(t))
	}
	writeJSON(w, http.StatusOK, out)
}

// terminalSize is the body of open and resize.
type terminalSize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

func decodeSize(w http.ResponseWriter, r *http.Request) (terminalSize, bool) {
	sz := terminalSize{Cols: 80, Rows: 24}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&sz); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf(`body must be {"cols": N, "rows": N}: %v`, err)})
		return sz, false
	}
	return sz, true
}

// handleOpenTerminal starts a new shell in the repository root.
func (s *Server) handleOpenTerminal(w http.ResponseWriter, r *http.Request) {
	if !s.terminalAllowed(w, r) {
		return
	}
	sz, ok := decodeSize(w, r)
	if !ok {
		return
	}
	info, err := s.terminals.Open(sz.Cols, sz.Rows)
	if errors.Is(err, supervisor.ErrTooManyTerminals) {
		writeJSON(w, http.StatusTooManyRequests, errorResponse{Error: err.Error() + "; close one first"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toTerminalInfo(info))
}

// handleTerminalInput writes keystrokes or a paste to the shell.
func (s *Server) handleTerminalInput(w http.ResponseWriter, r *http.Request) {
	if !s.terminalAllowed(w, r) {
		return
	}
	term, ok := s.terminalParam(w, r)
	if !ok {
		return
	}
	var req struct {
		Data string `json:"data"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTerminalInput)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf(`body must be {"data": "…"}: %v`, err)})
		return
	}
	if err := term.Write([]byte(req.Data)); err != nil {
		writeJSON(w, http.StatusGone, errorResponse{Error: "the terminal has ended"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTerminalResize tells the shell its window changed size.
func (s *Server) handleTerminalResize(w http.ResponseWriter, r *http.Request) {
	if !s.terminalAllowed(w, r) {
		return
	}
	term, ok := s.terminalParam(w, r)
	if !ok {
		return
	}
	sz, ok := decodeSize(w, r)
	if !ok {
		return
	}
	if err := term.Resize(sz.Cols, sz.Rows); err != nil {
		writeJSON(w, http.StatusGone, errorResponse{Error: "the terminal has ended"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCloseTerminal hangs up a shell: SIGHUP to its session, then SIGKILL.
func (s *Server) handleCloseTerminal(w http.ResponseWriter, r *http.Request) {
	if !s.terminalAllowed(w, r) {
		return
	}
	if err := s.terminals.Close(r.PathValue("id")); err != nil {
		if errors.Is(err, supervisor.ErrNoTerminal) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "unknown terminal"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTerminalStream is a terminal's output: the scrollback first, then
// everything the shell writes, then an end frame when it exits. A browser that
// reconnects gets the scrollback again, so the page resets its screen on every
// open before writing what arrives.
func (s *Server) handleTerminalStream(w http.ResponseWriter, r *http.Request) {
	if !s.terminalAllowed(w, r) {
		return
	}
	term, ok := s.terminalParam(w, r)
	if !ok {
		return
	}
	ctx, cancel, ok := s.streamContext(r)
	if !ok {
		http.Error(w, "forbidden: the session token was rotated", http.StatusForbidden)
		return
	}
	defer cancel()
	sse, ok := newSSE(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	s.streams.Add(1)
	defer s.streams.Add(-1)

	backlog, out, detach := term.Attach()
	defer detach()

	_ = sse.retry(2 * time.Second)
	if len(backlog) > 0 {
		if err := sse.send(terminalChunk{Data: base64.StdEncoding.EncodeToString(backlog), Replay: true}); err != nil {
			return
		}
	}

	hb := time.NewTicker(s.heartbeat)
	defer hb.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case chunk, ok := <-out:
			if !ok {
				// Ended, or this client fell too far behind. Only the former
				// is the end of the terminal; the latter reconnects.
				select {
				case <-term.Done():
					_ = sse.send(terminalChunk{End: true})
				default:
				}
				return
			}
			if err := sse.send(terminalChunk{Data: base64.StdEncoding.EncodeToString(chunk)}); err != nil {
				return
			}
		case <-hb.C:
			if err := sse.comment("heartbeat"); err != nil {
				return
			}
		}
	}
}

// terminalChatter reports a request the audit log should not record when it
// succeeds: keystrokes and resizes arrive by the hundred, and an accepted one
// says nothing an operator needs. Refusals are still recorded.
func terminalChatter(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/terminals/") {
		return false
	}
	return strings.HasSuffix(r.URL.Path, "/input") || strings.HasSuffix(r.URL.Path, "/resize")
}
