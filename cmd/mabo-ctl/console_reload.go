package main

import (
	"context"

	"github.com/maborak/mabo-ctl/internal/console"
	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// runConsole hands the terminal to the full-screen console over sup, with
// configuration reload wired to [app.reloadConfig] — the same reload `serve`
// offers, so an edit to mabo-ctl.yaml means the same thing in both front ends.
// A test that substituted [Env.RunConsole] gets its fake instead.
func (a *app) runConsole(sup *supervisor.Supervisor) error {
	if a.env.RunConsole != nil {
		return a.env.RunConsole(sup)
	}
	return console.RunWith(sup, console.Options{
		Root: a.cfg.Root,
		Reload: func(ctx context.Context, ev chan<- supervisor.Event) (*console.Reloaded, error) {
			r, err := a.reloadConfig(ctx, ev)
			if r == nil {
				return nil, err
			}
			return &console.Reloaded{
				Controller: r.sup,
				Stopped:    r.report.Stopped,
				Restarted:  r.report.Restarted,
			}, err
		},
	})
}
