package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// The two config-watch announcements on /api/events. The page keys its reload
// banner on them, so they are the wire contract and must not be reworded
// independently of internal/web/console.html.
const (
	// configChangedMsg says mabo-ctl.yaml, or an env_file it names, no longer
	// matches the configuration in force.
	configChangedMsg = "config change detected; reload configuration?"
	// configCurrentMsg says the files match the configuration in force again —
	// an edit was reverted — so the offer to reload is withdrawn.
	configCurrentMsg = "configuration file matches the running configuration again"
)

// configPollInterval is how often the console compares mabo-ctl.yaml with the
// configuration in force. Half a second announces an edit while the operator
// is still looking at the editor, and costs one small file read.
const configPollInterval = 500 * time.Millisecond

// Reloaded is what [Options.Reload] hands back when it applied a configuration.
type Reloaded struct {
	// Controller is the supervisor of the new configuration; the console
	// adopts it for every later request.
	Controller Controller
	// Origins explains the new configuration's resolved ports, for /api/config.
	Origins []service.Origin
	// Stopped names the running services the new configuration removed.
	Stopped []string
	// Restarted names the services started under their new definition.
	Restarted []string
}

// reloadResponse is the body of a POST /api/config/reload that applied the new
// configuration. Like a mutation, a service that would not start is an outcome
// carried in a 200 body, not an HTTP error: the configuration WAS applied.
type reloadResponse struct {
	// OK reports that every affected service was stopped and started cleanly.
	OK bool `json:"ok"`
	// Error names what failed when OK is false.
	Error string `json:"error,omitempty"`
	// Stopped names the running services the new configuration removed.
	Stopped []string `json:"stopped"`
	// Restarted names the services started under their new definition.
	Restarted []string `json:"restarted"`
	// Events is what the supervisor reported while it worked, capped like a
	// mutation's. The same events also go to /api/events.
	Events []eventJSON `json:"events"`
}

// handleReloadConfig applies mabo-ctl.yaml as it now reads on disk.
//
// Three outcomes, each with its own status, because "did my edit take?" has
// three answers. 409: a start, stop or restart is in flight and the reload was
// not attempted. 422: nothing was applied — the file did not load, validate or
// resolve, or a service it needs cannot run — and the previous configuration
// is still in force. 200: the new configuration is in force, and the body says
// whether every affected service came back.
//
// The reload runs to completion even if the browser goes away; see
// [supervisor.Supervisor.ApplyReload] for why it cannot be abandoned half way.
func (s *Server) handleReloadConfig(w http.ResponseWriter, r *http.Request) {
	if s.reload == nil {
		writeJSON(w, http.StatusUnprocessableEntity, errorResponse{
			Error: "this console cannot reload its configuration; restart it to apply edits to mabo-ctl.yaml"})
		return
	}
	if !s.lifecycleMu.TryLock() {
		writeJSON(w, http.StatusConflict, errorResponse{
			Error: "a start, stop or restart is in progress; reload when it finishes"})
		return
	}
	defer s.lifecycleMu.Unlock()

	ev, finish := s.collectEvents()
	res, err := s.reload(context.WithoutCancel(r.Context()), ev)
	events := finish()
	if res == nil {
		if err == nil {
			err = errors.New("the configuration was not applied")
		}
		s.events.publish(supervisor.Event{
			Msg: "configuration reload refused; the previous configuration stays in force", Err: err})
		writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: err.Error()})
		return
	}

	s.adopt(res)
	s.events.publish(supervisor.Event{Msg: "configuration reloaded", Err: err})
	resp := reloadResponse{
		OK:        err == nil,
		Stopped:   nonNil(res.Stopped),
		Restarted: nonNil(res.Restarted),
		Events:    events,
	}
	if err != nil {
		resp.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// adopt makes res the configuration every later request sees. The pending
// announcement is cleared with it; the watcher re-announces on its next tick
// if the file moved on again while the reload ran.
func (s *Server) adopt(res *Reloaded) {
	insts := res.Controller.Instances()
	names := make([]string, 0, len(insts))
	known := make(map[string]struct{}, len(insts))
	for _, in := range insts {
		names = append(names, in.Name)
		known[in.Name] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctrl, s.names, s.known = res.Controller, names, known
	s.origins = append([]service.Origin(nil), res.Origins...)
	s.ctrlGen++
	s.configPending = false
}

// watchConfig announces, on /api/events, every time mabo-ctl.yaml — or an
// env_file it names — stops or starts matching the configuration in force.
//
// It never reloads by itself. Editing YAML is often several writes, and
// applying an intermediate document could restart the stack while the
// operator is still typing; the page asks, and applies the final document.
// The comparison is against the configuration in force rather than the last
// thing read, so reverting an edit withdraws the offer.
func (s *Server) watchConfig(ctx context.Context) {
	ticker := time.NewTicker(s.configPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			ctrl, gen := s.ctrl, s.ctrlGen
			s.mu.Unlock()
			changed, _ := ctrl.Config().Changed()

			s.mu.Lock()
			if gen != s.ctrlGen {
				// A reload swapped the configuration under this comparison;
				// the next tick compares against the new one.
				s.mu.Unlock()
				continue
			}
			was := s.configPending
			s.configPending = changed
			s.mu.Unlock()

			switch {
			case changed && !was:
				s.events.publish(supervisor.Event{Msg: configChangedMsg})
			case !changed && was:
				s.events.publish(supervisor.Event{Msg: configCurrentMsg})
			}
		}
	}
}

// collectEvents starts the goroutine a lifecycle operation reports through: it
// publishes every event to /api/events and keeps up to maxCollectedEvents for
// the response body. finish closes the channel — the operation must have
// returned by then — and returns what was kept, never nil.
func (s *Server) collectEvents() (chan<- supervisor.Event, func() []eventJSON) {
	ev := make(chan supervisor.Event, streamBuffer)
	var collected []eventJSON
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for e := range ev {
			s.events.publish(e)
			if len(collected) < maxCollectedEvents {
				collected = append(collected, toEventJSON(e))
			}
		}
	}()
	return ev, func() []eventJSON {
		close(ev)
		<-drained
		if collected == nil {
			return []eventJSON{}
		}
		return collected
	}
}

// nonNil returns names, or an empty slice for nil, so a JSON reader sees []
// rather than null.
func nonNil(names []string) []string {
	if names == nil {
		return []string{}
	}
	return names
}
