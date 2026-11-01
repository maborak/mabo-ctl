package supervisor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/maborak/mabo-ctl/internal/service"
)

// ReloadReport says what [Supervisor.ApplyReload] did to the running stack.
type ReloadReport struct {
	// Applied reports that the new supervisor is authoritative from now on. It
	// is false only when ApplyReload refused, and then the previous
	// configuration is still the one in force.
	Applied bool
	// Stopped names the running services the new configuration no longer
	// declares. Each was stopped under its previous definition.
	Stopped []string
	// Restarted names the services started under their new definition: the
	// running ones whose process would differ, and any an earlier reload took
	// down and could not bring back. A start that failed is reported in the
	// error ApplyReload returns, not left out of this list.
	Restarted []string
}

// ApplyReload moves the running stack from s's definitions to next's. Running
// services the new configuration removed are stopped; running services whose
// PROCESS would differ — see [sameProcess] — are stopped under their old
// definition and started under the new one; everything else keeps running
// untouched. Callers must exclude every other lifecycle operation while it
// runs.
//
// It refuses, with Applied false and nothing changed, when a service it would
// start cannot be (an unresolvable runtime, a dependency cycle). It also
// refuses after stopping the removed services if one of them survives its
// stop: once next is adopted that service is unnameable, so the previous
// configuration stays in force while it still runs.
//
// From there on next is authoritative, and a failure is reported without
// being hidden: a changed service that survives its stop is NOT started (a
// start would report it "already running" and the reload would claim a
// success the old process belies), and a start that fails is returned with
// every per-service reason on ev. A service the reload stopped and could not
// bring back is remembered on next, so the next reload starts it again
// instead of skipping it as "not running".
//
// ctx's cancellation is deliberately ignored. Every step has its own bound —
// stop grace, ready timeout, hook budgets — and a reload abandoned half way
// leaves a stack running neither definition: stopped services nobody starts
// and, before the stop path was fixed, a SIGKILL for every service after the
// interruption. See docs/LANDMINES.md §18.
func (s *Supervisor) ApplyReload(ctx context.Context, next *Supervisor, ev chan<- Event) (ReloadReport, error) {
	ctx = context.WithoutCancel(ctx)

	running := s.liveNames()
	owed := s.owedNames()
	ignore := membershipPortVars(s.byName, next.byName)

	var removed, changed, start []string
	for _, in := range s.insts {
		cand, kept := next.byName[in.Name]
		switch {
		case running[in.Name] && !kept:
			removed = append(removed, in.Name)
		case running[in.Name] && !sameProcess(in, cand, ignore):
			changed = append(changed, in.Name)
		}
	}
	for _, in := range next.insts {
		if slices.Contains(changed, in.Name) || (owed[in.Name] && !running[in.Name]) {
			start = append(start, in.Name)
		}
	}

	// Refuse before touching anything when a service this reload would spawn
	// cannot be spawned. A dependency that is up and unchanged is not spawned
	// — Start finds it already running — so its runtime is not checked.
	if len(start) > 0 {
		levels, err := service.SelectLevels(next.insts, start)
		if err != nil {
			return ReloadReport{}, err
		}
		for _, level := range levels {
			for _, in := range level {
				if running[in.Name] && !slices.Contains(changed, in.Name) {
					continue
				}
				if err := in.Runnable(); err != nil {
					return ReloadReport{}, err
				}
			}
		}
	}

	if len(removed) > 0 {
		_ = s.Stop(ctx, removed, ev)
		if alive := s.survivors(removed); len(alive) > 0 {
			return ReloadReport{}, fmt.Errorf("could not stop %s, which the new configuration no longer declares; "+
				"the previous configuration stays in force so it can still be stopped by name",
				strings.Join(alive, ", "))
		}
	}

	var failures []error
	if len(changed) > 0 {
		_ = s.Stop(ctx, changed, ev)
		if alive := s.survivors(changed); len(alive) > 0 {
			failures = append(failures, fmt.Errorf("could not stop %s, so %s still running the previous definition",
				strings.Join(alive, ", "), isAre(len(alive))))
			start = slices.DeleteFunc(start, func(n string) bool { return slices.Contains(alive, n) })
		}
	}
	if len(start) > 0 {
		if err := next.Start(ctx, start, ev); err != nil {
			failures = append(failures, err)
		}
	}

	// What the reload took down and could not bring back is owed a start by
	// the next reload; see the doc comment.
	next.oweRestart(next.notRunning(start))

	return ReloadReport{Applied: true, Stopped: removed, Restarted: start}, errors.Join(failures...)
}

// sameProcess reports whether a service defined as a and as b would run the
// same process: the same program and arguments, in the same directory, with
// the same environment, on the same port, attached the same way.
//
// That is the whole test, deliberately. Colour, the health probe, the open
// target, the readiness window, dependencies, autostart and the hooks change
// what the new supervisor does AROUND the process and take effect without a
// restart; comparing them restarted a service — and rotated away its log —
// for a colour change.
//
// ignore names environment variables to leave out of the comparison: the
// <NAME>_PORT of a service declared on only one side. Every child's
// environment carries every ported service's variable, so adding one service
// changed every other service's environment and the comparison restarted the
// whole stack for it. A process cannot depend on a variable that did not exist
// when it started, and one naming a service that is gone has nothing left to
// point at; a sibling's port that CHANGED is still compared, because a process
// holding the old value would talk to the wrong port.
func sameProcess(a, b service.Instance, ignore map[string]bool) bool {
	return a.Dir == b.Dir && a.Port == b.Port && a.TTY == b.TTY &&
		slices.Equal(a.Cmd, b.Cmd) &&
		slices.Equal(envWithout(a.Env, ignore), envWithout(b.Env, ignore))
}

// membershipPortVars names the <NAME>_PORT variables of services declared in
// exactly one of the two sets; see [sameProcess].
func membershipPortVars(old, next map[string]service.Instance) map[string]bool {
	out := make(map[string]bool)
	for n := range old {
		if _, ok := next[n]; !ok {
			out[service.PortEnvVar(n)] = true
		}
	}
	for n := range next {
		if _, ok := old[n]; !ok {
			out[service.PortEnvVar(n)] = true
		}
	}
	return out
}

// envWithout returns env without the KEY=VALUE entries whose key is in ignore.
func envWithout(env []string, ignore map[string]bool) []string {
	if len(ignore) == 0 {
		return env
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if !ignore[key] {
			out = append(out, kv)
		}
	}
	return out
}

// liveNames reports which services have a live process mabo-ctl started. It
// reads pid files only: deciding what a reload must restart needs no health
// probe and no port-holder lookup. A pid file that cannot be read counts as
// live, because a process that may be there must not be started a second time;
// a recycled pid provably is not ours and does not count.
func (s *Supervisor) liveNames() map[string]bool {
	out := make(map[string]bool, len(s.insts))
	for _, in := range s.insts {
		rec, err := s.liveRecord(in.Name)
		if errors.Is(err, ErrStalePID) {
			continue
		}
		out[in.Name] = err != nil || rec.PID > 0
	}
	return out
}

// survivors returns the names in names that still have a live process.
func (s *Supervisor) survivors(names []string) []string {
	live := s.liveNames()
	var out []string
	for _, n := range names {
		if live[n] {
			out = append(out, n)
		}
	}
	return out
}

// notRunning returns the names in names that have no live process.
func (s *Supervisor) notRunning(names []string) []string {
	live := s.liveNames()
	var out []string
	for _, n := range names {
		if !live[n] {
			out = append(out, n)
		}
	}
	return out
}

// oweRestart records services a reload stopped and could not bring back.
func (s *Supervisor) oweRestart(names []string) {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	for _, n := range names {
		if s.owed == nil {
			s.owed = make(map[string]bool)
		}
		s.owed[n] = true
	}
}

// settleOwed forgets that a reload owes svc a start. An explicit stop does
// it: the operator has said what they want running.
func (s *Supervisor) settleOwed(svc string) {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	delete(s.owed, svc)
}

// owedNames returns a copy of the services a reload owes a start.
func (s *Supervisor) owedNames() map[string]bool {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	out := make(map[string]bool, len(s.owed))
	for n := range s.owed {
		out[n] = true
	}
	return out
}

// isAre agrees a verb with a count, for the one message that needs it.
func isAre(n int) string {
	if n == 1 {
		return "it is"
	}
	return "they are"
}
