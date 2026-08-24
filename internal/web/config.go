package web

import (
	"net/http"

	"github.com/maborak/mabo-ctl/internal/ui"
)

// handleConfig serves the resolved configuration: which mabo-ctl.yaml was loaded,
// what every service resolved to, and — the reason the route exists — WHICH of
// the four precedence levels produced each port.
//
// Port precedence has four levels (--ports, then a <NAME>_PORT in the caller's
// environment, then the persisted .dev/run.env, then the declared default),
// template expansion rewrites cmd, env and health on the way past, and runtime:
// rewrites cmd[0] into an absolute interpreter path. Until this route existed
// nothing showed an operator any of it, so "why is backend on 7999?" was a
// source-reading exercise.
//
// It emits exactly what `mabo-ctl config --json` emits, for the same reason
// /api/status emits exactly what `mabo-ctl status --json` emits: it calls
// [ui.ConfigJSON] over a [ui.ConfigView] rather than serialising a second shape
// here. A console that spelled a field differently from the CLI would make the
// two disagree about the same resolution — which is the drift this whole
// feature was built to remove, not to reintroduce one layer up.
//
// Redaction is not applied here either, and that is deliberate rather than
// missing: ui.BuildConfigView redacts through internal/redact as it assembles,
// so there is no unredacted ConfigView for this handler to leak.
//
// The route is a read of state captured at construction. It issues no probe and
// blocks on nothing, so it can be opened while a start is in flight.
func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	body, err := ui.ConfigJSON(s.configView())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// configView assembles the view from what the Controller knows and what the
// caller supplied.
//
// The origins, the state directory and the discovery mode come from Options
// because none of them is derivable here: the precedence chain ran in
// cmd/mabo-ctl over the --ports flag and the captured <NAME>_PORT variables that
// this package never sees, internal/state owns the layout under .dev/, and only
// the flag parser knows whether --config was given.
func (s *Server) configView() ui.ConfigView {
	return ui.BuildConfigView(ui.ConfigInput{
		Config:    s.ctrl.Config(),
		Instances: s.ctrl.Instances(),
		Origins:   s.origins,
		StateDir:  s.stateDir,
		Explicit:  s.explicitConfig,
	})
}
