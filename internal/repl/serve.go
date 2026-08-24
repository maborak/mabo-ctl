package repl

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// serving is the web console the session has bound, and everything needed to
// take it down again.
type serving struct {
	// url is the address to open, session token included, as the listener
	// reported it AFTER binding. It is what `serve` prints the second time.
	url string
	// cancel stops the serving goroutine.
	cancel context.CancelFunc
	// done receives the serve result exactly once, so unserve can wait for the
	// socket to be closed rather than assume it.
	done chan error
}

// serve binds the web console for this session and prints its URL.
//
// Running it a second time prints the URL that is already serving and binds
// NOTHING. That is the whole reason serve is native to this console rather than
// dispatched: `mabo-ctl serve` on the command line blocks until it is
// interrupted, which inside a prompt would mean a console you can only stop by
// losing the prompt, and a second bind of the same port fails with an error
// about an address in use when the honest answer is "it is already open, here
// is where".
//
// args is the rest of the typed line: an optional bind address, exactly as
// `mabo-ctl serve --addr` takes one.
func (s *session) serve(args []string) {
	if len(args) > 1 {
		s.out.line(`usage: serve [host:port] — one optional address, e.g. serve 127.0.0.1:0`)
		return
	}
	addr := ""
	if len(args) == 1 {
		addr = strings.TrimPrefix(args[0], "--addr=")
	}

	s.mu.Lock()
	existing := s.srv
	s.mu.Unlock()
	if existing != nil {
		s.out.line("the console is already serving at " + existing.url +
			"\nType \"unserve\" to stop it, or open the URL above.")
		return
	}

	if s.opt.NewListener == nil {
		s.out.line("this console was built without a web console; `serve` is unavailable here")
		return
	}
	l, err := s.opt.NewListener(addr)
	if err != nil {
		s.out.line(s.opt.FormatError(err))
		return
	}
	// Bind BEFORE printing anything. The address that was asked for and the
	// address that was bound differ whenever the port was 0 or the requested
	// port was taken, and a URL printed from the request is a guess that is
	// wrong exactly when it matters.
	if err := l.Listen(); err != nil {
		s.out.line(s.opt.FormatError(err))
		return
	}

	s.out.line("console serving at " + s.hold(l) + `
That URL carries a session token that can start, stop and restart every declared
service. Treat it as a password. Type "unserve" to stop it; leaving the console
stops it too.`)
}

// adopt starts serving the console the caller handed over in [Options.Console],
// if there is one.
//
// The caller bound it and printed its URL; from here it belongs to the session
// and is released by the same [session.shutdownServer] that `unserve` and
// quitting use. That is the whole reason for adopting rather than letting the
// caller serve it alongside the prompt: one listener with one shutdown path
// cannot be leaked by leaving through the other one.
func (s *session) adopt() {
	if s.opt.Console == nil {
		return
	}
	s.hold(s.opt.Console)
}

// hold serves l under the session's context and records it as the console this
// session owns, returning the URL the listener reports.
//
// Both ways a console can arrive — bound here by `serve`, or handed over
// already bound in [Options.Console] — go through it, so both are stopped by
// [session.shutdownServer] and neither can outlive the prompt.
func (s *session) hold(l Listener) string {
	ctx, cancel := context.WithCancel(s.ctx)
	done := make(chan error, 1)
	go func() { done <- l.ListenAndServe(ctx) }()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.srv = &serving{url: l.URL(), cancel: cancel, done: done}
	return s.srv.url
}

// unserve stops the served console and releases its port.
func (s *session) unserve(args []string) {
	if len(args) > 0 {
		s.out.line("usage: unserve — it takes no arguments")
		return
	}
	url, err, ok := s.shutdownServer()
	if !ok {
		s.out.line(`no console is running; type "serve" to start one`)
		return
	}
	if err != nil {
		s.out.line(s.opt.FormatError(fmt.Errorf("stopping the console at %s: %w", url, err)))
		return
	}
	s.out.line("console stopped; " + url + " is closed and the port is free again")
}

// shutdownServer stops the served console, waits for the listener to close, and
// reports its URL, the serve result, and whether there was anything to stop.
//
// Waiting is the point. A cancel that returns before the socket is closed makes
// `unserve` a lie — the next `serve` on the same port, or an unrelated process
// binding it, would race a listener that is still open. The graceful shutdown
// inside ListenAndServe is what closes it, and the send on done is what says so.
func (s *session) shutdownServer() (url string, err error, ok bool) {
	s.mu.Lock()
	srv := s.srv
	s.srv = nil
	s.mu.Unlock()
	if srv == nil {
		return "", nil, false
	}

	srv.cancel()
	err = <-srv.done
	// A server told to shut down reports the closure it was asked to perform.
	// That is the successful outcome, not a failure to report to the user.
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	return srv.url, err, true
}
