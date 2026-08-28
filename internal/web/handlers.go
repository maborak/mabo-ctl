package web

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maborak/mabo-ctl/internal/redact"
	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/ui"
)

// consoleHTML is the whole console: one self-contained page, embedded in the
// binary. It is embedded rather than read from disk so that mabo-ctl remains a
// single file to copy around, and it is self-contained — no CDN, no font
// service, no remote anything — because mabo-ctl ships no telemetry and a console
// that phones out to load a stylesheet would be its first.
//
//go:embed console.html
var consoleHTML string

// contentSecurityPolicy is served with the page. It permits the inline style
// and script the single-file page is made of, and same-origin fetch and
// EventSource, and nothing else at all: no remote script, no remote style, no
// remote font, no image beyond a data: URI, no framing, no form submission.
// It is the mechanical enforcement of "the page makes no external requests".
const contentSecurityPolicy = "default-src 'none'; " +
	"script-src 'unsafe-inline'; " +
	"style-src 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"font-src 'self' data:; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'"

// unlockCSP is served with the unlock page, and differs from
// [contentSecurityPolicy] in exactly two ways, both forced by what that page is.
//
// form-action 'self' — because the page IS a form, and the console policy says
// form-action 'none'. That is right for the console, which has no forms and
// should never grow one; under it the unlock form silently could not submit at
// all. The button looked fine and did nothing, which no assertion about the
// markup could have caught and a browser found immediately.
//
// script-src is absent entirely: the unlock page carries no script, so it opts
// out of the one relaxation the console needs. A page whose only job is to
// accept a credential should be the most locked-down thing mabo-ctl serves.
const unlockCSP = "default-src 'none'; " +
	"style-src 'unsafe-inline'; " +
	"base-uri 'none'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// serviceInfo is one declared service as /api/services renders it.
//
// It answers the half of the user's request the terminal shows worst: what
// command is this thing actually running, from which directory, under which
// runtime, with which variables set.
type serviceInfo struct {
	// Name is the declared service name.
	Name string `json:"name"`
	// Dir is the resolved absolute working directory.
	Dir string `json:"dir"`
	// Port is the resolved port, 0 when the service declares none.
	Port int `json:"port"`
	// Health is the expanded readiness URL, "" when there is no probe.
	Health string `json:"health"`
	// Runtime is the declared runtime ("", "system", "conda:x", "node:x").
	Runtime string `json:"runtime"`
	// Cmd is the expanded argv, Cmd[0] absolute.
	Cmd []string `json:"cmd"`
	// CmdLine is Cmd rendered as one shell-quoted line, for copying.
	CmdLine string `json:"cmd_line"`
	// CmdError is why Cmd[0] could not be resolved, when it could not be. The
	// service is displayable but not startable in that state, and saying so is
	// better than rendering a command that will never run.
	CmdError string `json:"cmd_error,omitempty"`
	// Env is the DECLARED environment, values redacted by key. It is never the
	// resolved environment; see redact.Env.
	Env []redact.Var `json:"env"`
	// DependsOn lists the services that start first.
	DependsOn []string `json:"depends_on"`
	// Color is the label colour declared in mabo-ctl.yaml, "" when none.
	Color string `json:"color"`
}

// logsResponse is the body of /api/logs/{svc}.
type logsResponse struct {
	Service string   `json:"service"`
	Lines   []string `json:"lines"`
	Count   int      `json:"count"`
}

// eventJSON is one supervisor.Event on the wire, shared by the mutation
// responses and the /api/events stream so a page parses one shape, not two.
type eventJSON struct {
	Service string `json:"service"`
	Phase   string `json:"phase"`
	Msg     string `json:"msg"`
	Error   string `json:"error,omitempty"`
}

// mutationResponse is the body of a start, stop or restart.
type mutationResponse struct {
	// Operation is "start", "stop" or "restart".
	Operation string `json:"operation"`
	// Services are the services asked for; empty means every service.
	Services []string `json:"services"`
	// OK reports that the supervisor completed the operation without
	// reporting a failure.
	OK bool `json:"ok"`
	// Error is the supervisor's error text when OK is false. A service that
	// refused to start is an expected outcome and not an HTTP error, so this
	// travels in a 200 body: the request succeeded, the start did not.
	Error string `json:"error,omitempty"`
	// Events is what the supervisor reported while it worked, capped at
	// maxCollectedEvents. The same events also go to /api/events.
	Events []eventJSON `json:"events"`
}

// errorResponse is the body of every JSON error this package returns.
type errorResponse struct {
	Error string   `json:"error"`
	Valid []string `json:"valid,omitempty"`
}

// handleIndex serves the console page to a caller that has the session token,
// and an unlock page to one that does not.
//
// This route is deliberately NOT wrapped in [Server.requireSession], and the
// reason is the whole difference between a control and an obstacle. The API
// routes are wrapped, because a bare 403 is the right answer to a script. A
// person who opened a bookmark, or reloaded after the page stripped the token
// out of the address bar, gets a box to paste it into instead — while the
// response still contains no token, which is the property that matters and the
// one the regression test pins.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	ok, fromQuery := s.session(r)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if !ok {
		// 401 and not 403: the request may well succeed once a credential is
		// supplied, which is exactly what this page is for.
		w.Header().Set("Content-Security-Policy", unlockCSP)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(unlockHTML))
		return
	}
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	if fromQuery {
		s.setSessionCookie(w)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(s.renderPage())
}

// unlockHTML is the page an unauthenticated browser gets: a box for the session
// token and nothing else.
//
// It carries NO token, which is the point — the hole this whole session layer
// exists to close was the console page handing its own credential to anyone who
// asked. It also carries no script: the form is a plain GET to "/", so the
// browser itself turns the field into the ?token= the printed URL uses. A page
// whose only job is to accept a credential should not need JavaScript to work.
const unlockHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>mabo-ctl console — session token</title>
<style>
  :root { color-scheme: light dark; --bg:#f1f4f7; --fg:#12161b; --dim:#58626d;
          --line:#d3dae1; --card:#fff; --accent:#1f6feb; }
  @media (prefers-color-scheme: dark) {
    :root { --bg:#0d1117; --fg:#e6edf3; --dim:#8b949e; --line:#30363d; --card:#161b22; }
  }
  * { box-sizing: border-box; }
  body { margin:0; min-height:100vh; display:grid; place-items:center;
         background:var(--bg); color:var(--fg); padding:24px;
         font:14px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; }
  .card { width:100%; max-width:30rem; background:var(--card); border:1px solid var(--line);
          border-radius:8px; padding:24px; }
  h1 { margin:0 0 4px; font-size:16px; }
  p { margin:0 0 16px; color:var(--dim); }
  code { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size:12px; }
  form { display:flex; gap:8px; }
  /* Masked with CSS rather than type="password".
     A password input invites the browser's password manager to offer to SAVE
     this value and sync it to other devices. The session token is an ephemeral
     capability that dies with the mabo-ctl process — storing it durably in a
     credential vault is the opposite of what it is for, and Chrome ignores
     autocomplete="off" on password fields specifically. text-security gives the
     same shoulder-surfing protection with none of that. */
  input { flex:1 1 auto; min-width:0; padding:8px 10px; border:1px solid var(--line);
          border-radius:6px; background:var(--bg); color:var(--fg);
          font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size:12px;
          -webkit-text-security: disc; text-security: disc; }
  button { padding:8px 14px; border:1px solid var(--accent); border-radius:6px;
           background:var(--accent); color:#fff; font-weight:600; cursor:pointer; }
  .hint { margin:16px 0 0; font-size:12px; }
</style>
</head>
<body>
  <main class="card">
    <h1>mabo-ctl console</h1>
    <p>This console starts and stops the commands in your <code>mabo-ctl.yaml</code>,
       so it needs the session token mabo-ctl printed when it began serving.</p>
    <form method="get" action="/">
      <input type="text" name="token" autocomplete="off" autofocus
             spellcheck="false" autocapitalize="off" autocorrect="off"
             aria-label="session token" placeholder="session token">
      <button type="submit">Unlock</button>
    </form>
    <p class="hint">The token is on the line mabo-ctl printed, after
       <code>?token=</code>. It changes every time mabo-ctl restarts. Opening that
       whole URL works too.</p>
  </main>
</body>
</html>
`

// renderPage executes the embedded page and guarantees the token is in it.
//
// The page is written by a different hand than this file, so the token is
// delivered two ways rather than one. If the page uses {{.Token}} the template
// fills it in; if it does not — or if it is not a valid template at all — the
// token is injected as a meta tag and a window global instead. A console whose
// buttons all 403 because the two halves disagreed about how the token arrives
// is a failure worth twenty lines of belt and braces.
func (s *Server) renderPage() []byte {
	data := map[string]any{
		"Token":    s.token,
		"Addr":     s.Addr(),
		"URL":      s.URL(),
		"Services": s.names,
	}
	if cfg := s.ctrl.Config(); cfg != nil {
		data["Root"] = cfg.Root
		data["ConfigPath"] = cfg.Path
	}

	out := []byte(consoleHTML)
	if s.tmpl != nil {
		var buf bytes.Buffer
		if err := s.tmpl.Execute(&buf, data); err == nil {
			out = buf.Bytes()
		}
	}
	if bytes.Contains(out, []byte(s.token)) {
		return out
	}
	return injectToken(out, s.token)
}

// injectToken puts the token into a page that did not ask for it, immediately
// after <head> when there is one and at the very top otherwise. The token is
// hex, so it needs no escaping in either an attribute or a string literal.
func injectToken(page []byte, token string) []byte {
	snippet := []byte(`<meta name="mabo-ctl-token" content="` + token + `">` + "\n" +
		`<script>window.maboCtlToken="` + token + `";</script>` + "\n")

	lower := bytes.ToLower(page)
	if i := bytes.Index(lower, []byte("<head>")); i >= 0 {
		at := i + len("<head>")
		out := make([]byte, 0, len(page)+len(snippet))
		out = append(out, page[:at]...)
		out = append(out, '\n')
		out = append(out, snippet...)
		return append(out, page[at:]...)
	}
	return append(snippet, page...)
}

// handleServices renders every declared service: command, directory, runtime,
// ports, health URL, dependencies and declared environment.
func (s *Server) handleServices(w http.ResponseWriter, _ *http.Request) {
	insts := s.ctrl.Instances()
	cfg := s.ctrl.Config()

	out := make([]serviceInfo, 0, len(insts))
	for _, in := range insts {
		info := serviceInfo{
			Name: in.Name,
			Dir:  in.Dir,
			Port: in.Port,
			// health and cmd are redacted for the same reason declared env is:
			// these routes are readable by any local process, and a health URL
			// or a command argument carries credentials just as often as an
			// environment variable does.
			Health:    redact.URL(in.Health),
			Runtime:   in.Runtime,
			Cmd:       redact.Args(in.Cmd),
			CmdLine:   ui.ShellLine(redact.Args(in.Cmd)),
			DependsOn: append([]string(nil), in.DependsOn...),
			Color:     in.Color,
			Env:       []redact.Var{},
		}
		if in.CmdErr != nil {
			info.CmdError = in.CmdErr.Error()
		}
		// The declared environment comes from the config, never from
		// Instance.Env, which is the caller's whole environment.
		if cfg != nil {
			if spec, ok := cfg.Service(in.Name); ok {
				info.Env = redact.Env(spec.Env)
			}
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleStatus emits exactly what `mabo-ctl status --json` emits.
//
// It calls ui.StatusJSON rather than serialising supervisor.Status here. A
// second serialiser would drift from the first, and `--json` is a stable
// contract that other tools parse: the console showing a field the CLI does not
// have, or spelling one differently, would make the two disagree about the same
// supervisor.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// One deliberate divergence from the CLI: the health URL is redacted here.
	// `mabo-ctl status --json` prints to the operator's own terminal, where their
	// own credentials are theirs to see; this route answers any local process,
	// so a health URL carrying userinfo or an api_key query parameter must not
	// be handed out in full. The field names and shape stay identical, which is
	// what the stable contract actually promises.
	//
	// Detail gets the same treatment on the same value, because a probe failure
	// quotes the URL it dialled VERBATIM — `health: HEAD http://user:pw@…: dial
	// tcp …` — so redacting only the Health field would hand out the credential
	// through the field next to it. Redacting one channel and not the other is
	// how the first version of this control was got wrong.
	sts := s.ctrl.Status(ctx)
	for i := range sts {
		raw := sts[i].Health
		safe := redact.URL(raw)
		sts[i].Health = safe
		if raw != "" && raw != safe {
			sts[i].Detail = strings.ReplaceAll(sts[i].Detail, raw, safe)
		}
	}

	body, err := ui.StatusJSON(sts)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleLogs returns the last N lines of one service's log.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.serviceParam(w, r)
	if !ok {
		return
	}
	n := tailCount(r.URL.Query().Get("tail"))

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	lines := make(chan string, streamBuffer)
	done := make(chan error, 1)
	go func() { done <- s.ctrl.Tail(ctx, svc, n, false, lines) }()

	collected := make([]string, 0, 64)
	for line := range lines {
		if len(collected) < maxLogTail {
			collected = append(collected, line)
		}
	}
	if err := <-done; err != nil && ctx.Err() == nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, logsResponse{Service: svc, Lines: collected, Count: len(collected)})
}

// The mutating handlers. Each one is registered POST-only and wrapped in
// requireToken; the wrapper, not the handler, is what enforces the token, so a
// route added later without it is visible as a missing wrapper at the mux.
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	s.mutateOne(w, r, opStart)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.mutateOne(w, r, opStop)
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	s.mutateOne(w, r, opRestart)
}

// handleStartAll starts every declared service, autostart or not.
//
// It passes the names EXPLICITLY rather than nil. Nil means "the default
// selection", which `autostart: false` is designed to narrow — and a button
// labelled "Start all" that starts nothing because every service opted out is a
// broken control, however true its explanation. Clicking it is as explicit an
// instruction as typing the names.
func (s *Server) handleStartAll(w http.ResponseWriter, r *http.Request) {
	s.mutate(w, r, opStart, append([]string(nil), s.names...))
}

func (s *Server) handleStopAll(w http.ResponseWriter, r *http.Request) {
	s.mutate(w, r, opStop, nil)
}

// opKind names one supervisor operation.
type opKind string

// The operations the console can request.
const (
	opStart   opKind = "start"
	opStop    opKind = "stop"
	opRestart opKind = "restart"
)

// mutateOne validates {svc} and runs kind over just that service.
func (s *Server) mutateOne(w http.ResponseWriter, r *http.Request, kind opKind) {
	svc, ok := s.serviceParam(w, r)
	if !ok {
		return
	}
	s.mutate(w, r, kind, []string{svc})
}

// mutate runs one supervisor operation and reports what happened.
//
// The operation's context is derived from context.Background(), NOT from
// r.Context(). That is deliberate: a user who closes the tab half way through a
// start must not leave a stack that is half up, with some services spawned,
// some not, and nothing recorded about which. The request may go away; the
// operation finishes and its events still reach every other open console
// through the broker.
//
// Concurrency is bounded by a semaphore. A start blocks for the readiness
// timeout, so an unbounded number of them is an unbounded number of goroutines
// each holding real processes — reachable by anyone holding the token, which
// after a start is anyone with the page open.
func (s *Server) mutate(w http.ResponseWriter, r *http.Request, kind opKind, names []string) {
	select {
	case s.inflight <- struct{}{}:
		defer func() { <-s.inflight }()
	default:
		writeJSON(w, http.StatusTooManyRequests, errorResponse{
			Error: fmt.Sprintf("too many operations in flight (limit %d); try again when one finishes",
				maxInflightMutations),
		})
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.opTimeout())
	defer cancel()

	ev := make(chan supervisor.Event, streamBuffer)
	var (
		mu        sync.Mutex
		collected []eventJSON
	)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for e := range ev {
			s.events.publish(e)
			mu.Lock()
			if len(collected) < maxCollectedEvents {
				collected = append(collected, toEventJSON(e))
			}
			mu.Unlock()
		}
	}()

	var err error
	switch kind {
	case opStart:
		err = s.ctrl.Start(ctx, names, ev)
	case opStop:
		err = s.ctrl.Stop(ctx, names, ev)
	case opRestart:
		err = s.ctrl.Restart(ctx, names, ev)
	default:
		err = fmt.Errorf("web: unknown operation %q", kind)
	}
	close(ev)
	<-drained

	// A final event so a console that is only watching the stream sees the
	// outcome, not just the transitions leading up to it.
	final := supervisor.Event{Msg: string(kind) + " finished", Err: err}
	if len(names) == 1 {
		final.Service = names[0]
	}
	s.events.publish(final)

	resp := mutationResponse{
		Operation: string(kind),
		Services:  names,
		OK:        err == nil,
		Events:    collected,
	}
	if resp.Services == nil {
		resp.Services = []string{}
	}
	if resp.Events == nil {
		resp.Events = []eventJSON{}
	}
	if err != nil {
		resp.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// serviceParam reads and validates {svc}.
//
// Validation happens here, before the name reaches the supervisor, the state
// directory or anything that composes a path out of it. config already refuses
// a name that could traverse a path, but a front end that forwards whatever
// arrived in a URL is exactly how that guarantee gets bypassed later, so the
// name is checked against the DECLARED set and nothing else is accepted.
func (s *Server) serviceParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	svc := r.PathValue("svc")
	if _, ok := s.known[svc]; ok {
		return svc, true
	}
	writeJSON(w, http.StatusNotFound, errorResponse{
		Error: "unknown service",
		Valid: s.names,
	})
	return "", false
}

// tailCount parses ?tail=N, clamped so one request cannot ask for an unbounded
// read.
func tailCount(raw string) int {
	if raw == "" {
		return defaultLogTail
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultLogTail
	}
	if n > maxLogTail {
		return maxLogTail
	}
	return n
}

// toEventJSON converts one supervisor event to its wire form.
func toEventJSON(e supervisor.Event) eventJSON {
	out := eventJSON{Service: e.Service, Phase: string(e.Phase), Msg: e.Msg}
	if e.Err != nil {
		out.Error = e.Err.Error()
	}
	return out
}

// writeJSON writes v as the whole response body.
func writeJSON(w http.ResponseWriter, code int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error encoding response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// originsResponse is the body of both /api/origins routes.
type originsResponse struct {
	// Trusted is the editable allowlist, canonical and sorted.
	Trusted []string `json:"trusted"`
	// Implicit names the address mabo-ctl bound. It is always accepted, is not
	// part of Trusted, and cannot be removed — which is what makes a lockout
	// recoverable and is worth saying in the UI rather than leaving a reader to
	// wonder why an empty list still works.
	Implicit string `json:"implicit"`
	// Origin echoes the requester's own Origin, so the page can show which
	// entry it is currently relying on and refuse to let the user delete it.
	Origin string `json:"origin,omitempty"`
	// Max is the cap on Trusted.
	Max int `json:"max"`
}

// handleGetOrigins reports the origins the console accepts.
func (s *Server) handleGetOrigins(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, originsResponse{
		Trusted:  s.TrustedOrigins(),
		Implicit: "http://" + s.Addr(),
		Origin:   r.Header.Get("Origin"),
		Max:      maxTrustedOrigins,
	})
}

// handleHistory serves the recorded phase history — the same event shape the
// mutation responses and /api/events use, oldest first, capped at the
// broker's ring. Read-only: it never touches supervisor state.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	events := s.events.history()
	out := make([]eventJSON, len(events))
	for i, e := range events {
		out[i] = toEventJSON(e)
	}
	writeJSON(w, http.StatusOK, historyResponse{Events: out})
}

// historyResponse is the body of GET /api/history.
type historyResponse struct {
	// Events is the ring contents, oldest first.
	Events []eventJSON `json:"events"`
}

// handleSetOrigins replaces the trusted list.
//
// The whole list is sent rather than an add/remove verb, so the page never has
// to reason about what the server already had: what the operator sees is what
// they submit. The lockout guard lives in [Server.setTrustedOrigins] and refuses
// a change that would strip the origin this very request came from.
func (s *Server) handleSetOrigins(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Trusted []string `json:"trusted"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{
			Error: fmt.Sprintf("body must be {\"trusted\": [\"https://host\", …]}: %v", err)})
		return
	}

	next, err := s.setTrustedOrigins(req.Trusted, r.Header.Get("Origin"))
	if err != nil {
		// A lockout is a conflict with the state of THIS session, not a
		// malformed request; a bad origin is the request's own fault. The page
		// renders either verbatim, so the distinction is only in the code.
		code := http.StatusBadRequest
		if errors.Is(err, ErrOriginLockout) {
			code = http.StatusConflict
		}
		writeJSON(w, code, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, originsResponse{
		Trusted:  next,
		Implicit: "http://" + s.Addr(),
		Origin:   r.Header.Get("Origin"),
		Max:      maxTrustedOrigins,
	})
}
