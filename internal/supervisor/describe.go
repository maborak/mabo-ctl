package supervisor

import "github.com/maborak/mabo-ctl/internal/config"

// Config returns the parsed mabo-ctl.yaml this Supervisor was built from, or nil
// when it was built without one.
//
// It exists so a front end can show what a service DECLARES, which is a
// different question from what it resolved to. service.Instance carries the
// fully expanded child environment — the caller's own environment included, and
// therefore the caller's real tokens — so a front end that wants to display
// "which variables does this service set" must read config.Spec.Env and never
// Instance.Env. Handing out the config is what makes that possible without also
// handing out the resolved environment.
//
// The returned pointer is the Supervisor's own: treat it as read-only.
// config.Config.Service already returns deep copies of the specs it hands back,
// so the normal display path cannot mutate anything by accident.
func (s *Supervisor) Config() *config.Config { return s.cfg }
