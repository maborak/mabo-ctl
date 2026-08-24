package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// subscriberBuffer is the depth of one /api/events subscriber's channel. It is
// generous because the cost of a drop is a console that missed a transition,
// and cheap because an event is three short strings.
const subscriberBuffer = 256

// broker fans one stream of supervisor events out to every open console.
//
// It exists because supervisor.Event sends are NON-BLOCKING by design: the
// supervisor drops an event rather than let a slow reader wedge a start
// half-way through. That makes a single channel read by one HTTP handler
// actively wrong here — the first console to open would consume every event and
// every other console would show a frozen, silent stack while services came up
// around it. A subscriber that cannot keep up loses its own events and nobody
// else's, which is the only fair way to spend a full buffer.
type broker struct {
	mu     sync.Mutex
	subs   map[chan supervisor.Event]struct{}
	closed bool
	// dropped counts events that did not fit in some subscriber's buffer. It
	// is diagnostic only, but a console that has silently missed transitions
	// should be able to say so rather than look merely quiet.
	dropped int64
}

// newBroker returns an empty broker.
func newBroker() *broker {
	return &broker{subs: make(map[chan supervisor.Event]struct{})}
}

// subscribe registers a new subscriber and returns its channel and the function
// that releases it. The release function is idempotent and must be called — it
// is what stops a closed browser tab from being fanned out to forever.
//
// A subscription taken after the broker is closed yields an already-closed
// channel, so a handler that races shutdown ends immediately instead of waiting
// on something nothing will ever write to.
func (b *broker) subscribe() (<-chan supervisor.Event, func()) {
	ch := make(chan supervisor.Event, subscriberBuffer)

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if _, ok := b.subs[ch]; ok {
				delete(b.subs, ch)
				close(ch)
			}
		})
	}
}

// publish delivers e to every subscriber that has room for it.
//
// The send is non-blocking, and the mutex is held across the fan-out so a
// subscriber cannot be closed underneath an in-flight send. Publish therefore
// never blocks on a reader, which matters because its caller is the goroutine
// draining a live start.
func (b *broker) publish(e supervisor.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
			b.dropped++
		}
	}
}

// close releases every subscriber. It is idempotent.
func (b *broker) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for ch := range b.subs {
		delete(b.subs, ch)
		close(ch)
	}
}

// subscribers reports how many consoles are attached. Tests read it.
func (b *broker) subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// sseWriter writes one server-sent-events response.
//
// Every write is followed by a flush. Without the flush the body sits in Go's
// bufio writer until it fills, which for a log stream means the page shows
// nothing for minutes and then everything at once — a live view that is not
// live is worse than no live view, because it is believed.
type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

// newSSE writes the SSE response headers and returns a writer, or false when
// the response writer cannot flush and a stream is therefore impossible.
func newSSE(w http.ResponseWriter) (*sseWriter, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Nothing proxies this in the intended deployment, but a developer running
	// mabo-ctl behind a local nginx should still see their logs move.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &sseWriter{w: w, f: f}, true
}

// retry tells the browser how soon to reconnect after a drop.
func (s *sseWriter) retry(d time.Duration) error {
	return s.write("retry: " + fmt.Sprint(d.Milliseconds()) + "\n\n")
}

// comment sends a comment line. It is the heartbeat: it keeps an idle
// connection from being reaped and, more usefully, it fails as soon as the peer
// has gone, which is how an idle handler notices a closed tab.
func (s *sseWriter) comment(text string) error {
	return s.write(": " + text + "\n\n")
}

// send writes one unnamed event whose data is v as a single line of JSON.
//
// Unnamed on purpose: a browser's EventSource routes unnamed events to
// onmessage, so the page works whether it uses onmessage or an explicit
// addEventListener. JSON on purpose too: it cannot contain a raw newline, so
// one event is always exactly one "data:" line and no log line can ever forge a
// second event by containing one.
func (s *sseWriter) send(v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.write("data: " + string(body) + "\n\n")
}

// write emits raw bytes and flushes.
func (s *sseWriter) write(text string) error {
	if _, err := s.w.Write([]byte(text)); err != nil {
		return err
	}
	s.f.Flush()
	return nil
}

// logLine is one line of a service's output on the wire.
type logLine struct {
	Service string `json:"service"`
	Line    string `json:"line"`
}

// streamEnd tells the page why a stream stopped, so a dead pane is labelled
// rather than merely silent.
type streamEnd struct {
	Service string `json:"service"`
	End     bool   `json:"end"`
	Error   string `json:"error,omitempty"`
}

// handleStream is the live log stream for one service.
//
// The handler's whole shape is dictated by one requirement: when the tab
// closes, the supervisor.Tail behind it must stop. r.Context() is cancelled by
// net/http when the client goes away, the tail runs under that context, and
// before returning the handler cancels, drains the line channel and waits for
// the tail goroutine to publish its result. The drain is not optional — a tail
// blocked mid-send on a channel nobody is reading would never observe the
// cancellation, and one leaked goroutine per opened tab is the defining bug of
// this package.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.serviceParam(w, r)
	if !ok {
		return
	}
	sse, ok := newSSE(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	s.streams.Add(1)
	defer s.streams.Add(-1)

	ctx, cancel := context.WithCancel(r.Context())
	lines := make(chan string, streamBuffer)
	done := make(chan error, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				done <- fmt.Errorf("web: tailing %s panicked: %v", svc, rec)
			}
		}()
		done <- s.ctrl.Tail(ctx, svc, tailCount(r.URL.Query().Get("tail")), true, lines)
	}()

	// reaped records that the tail's outcome has already been collected below,
	// so the cleanup does not wait for a value that was consumed.
	reaped := false
	defer func() {
		cancel()
		for range lines { //nolint:revive // draining, the values are dead
		}
		if !reaped {
			<-done
		}
	}()

	_ = sse.retry(2 * time.Second)

	hb := time.NewTicker(s.heartbeat)
	defer hb.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-lines:
			if !ok {
				// Tail closed the channel, so it has returned; report why
				// rather than leaving the pane looking merely quiet.
				end := streamEnd{Service: svc, End: true}
				err := <-done
				reaped = true
				if err != nil && ctx.Err() == nil {
					end.Error = err.Error()
				}
				_ = sse.send(end)
				return
			}
			if err := sse.send(logLine{Service: svc, Line: line}); err != nil {
				return
			}
		case <-hb.C:
			if err := sse.comment("heartbeat"); err != nil {
				return
			}
		}
	}
}

// handleEvents is the lifecycle event stream.
//
// It reads a broker subscription rather than a supervisor channel; see [broker]
// for why a single channel would starve every console but the first.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	sse, ok := newSSE(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	s.streams.Add(1)
	defer s.streams.Add(-1)

	events, release := s.events.subscribe()
	defer release()

	_ = sse.retry(2 * time.Second)

	ctx := r.Context()
	hb := time.NewTicker(s.heartbeat)
	defer hb.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-events:
			if !ok {
				// The broker closed: the server is shutting down.
				return
			}
			if err := sse.send(toEventJSON(e)); err != nil {
				return
			}
		case <-hb.C:
			if err := sse.comment("heartbeat"); err != nil {
				return
			}
		}
	}
}
