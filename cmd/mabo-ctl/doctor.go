package main

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"
	"time"

	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/state"
	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/ui"
	"github.com/spf13/cobra"
)

// diagFinding is one thing doctor noticed about one service (or about the
// state directory as a whole). The status is the worst of the checks that
// produced it, so the report stays one line per service; the detail names
// every check that had something to say.
type diagFinding struct {
	name   string // service name, or "state dir"
	status ui.DoctorStatus
	detail string // empty when status is ok
}

// doctorCmd builds `mabo-ctl doctor`.
func (a *app) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the stack: runtimes, pid files, ports, crash evidence, state permissions",
		Long: `Doctor examines what mabo-ctl knows on disk and on the machine, one line
per service, and never signals anything:

  runtime   can the declared interpreter still be resolved?
  pid file  is it present, alive, and still HONEST (not recycled)?
  port      is the declared port free, or held by whoever should hold it?
  crash     did a previous run die on its own, and is that still unsurfaced?
  state     are the .dev/ file permissions still private?

Exit codes: 0 when every finding is ok or a warning, 1 when any check FAILS.
A warning wants a look; a failure wants action before the next start.`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usageErrorf("doctor takes no arguments; it examines every declared service")
			}
			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runDoctor(cmd)
		},
	}
}

// runDoctor resolves the stack and reports one finding per service. The
// per-service checks are independent, so they run concurrently the way
// Status's probes do; the port-holder lookup forks lsof once per ported
// service and must not be paid serially.
func (a *app) runDoctor(cmd *cobra.Command) error {
	// Resolving also loads the config (exit 3 on a broken one) and surfaces
	// every deferred runtime failure as Instance.CmdErr, which is doctor's
	// first check.
	_, insts, err := a.supervisor()
	if err != nil {
		return err
	}

	findings := make([]diagFinding, len(insts)+1)
	var wg sync.WaitGroup
	for i := range insts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			findings[i] = a.diagnoseService(insts[i])
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		findings[len(insts)] = diagnoseStateDir(a.stateDir())
	}()
	wg.Wait()

	r := a.renderer()
	fails, warns := 0, 0
	for _, f := range findings {
		fmt.Fprintln(a.env.Stdout, r.DoctorLine(f.status, f.name, f.detail))
		switch f.status {
		case ui.DoctorWarn:
			warns++
		case ui.DoctorFail:
			fails++
		}
	}
	fmt.Fprintln(a.env.Stdout, r.DoctorSummary(fails, warns, len(insts)))
	if fails > 0 {
		return fmt.Errorf("doctor: %s failed", joinAnd(failedNames(findings)))
	}
	return nil
}

// failedNames lists the services with a FAIL finding, for the error message.
func failedNames(findings []diagFinding) []string {
	var out []string
	for _, f := range findings {
		if f.status == ui.DoctorFail {
			out = append(out, f.name)
		}
	}
	return out
}

// diagnoseService runs every per-service check and folds them into one finding
// whose status is the worst of them. Every check is read-only: doctor never
// signals a process, clears a pid file or kills a port holder — diagnosing and
// repairing are different commands, and only one of them is safe to run to see
// what is wrong.
func (a *app) diagnoseService(inst service.Instance) diagFinding {
	f := diagFinding{name: inst.Name}

	// Runtime: a deferred interpreter failure is a service that cannot start.
	if inst.CmdErr != nil {
		f.status = ui.DoctorFail
		f.detail = joinDetail(f.detail, "cannot start: "+inst.CmdErr.Error())
	}

	// Pid file: present, alive, and still honest. A stale file is a warning —
	// the supervisor diagnoses and clears it on the next start — while a live
	// pid that fails the identity check is a FAIL: something else owns that
	// number now, and signalling it would be a mistake.
	rec, err := a.st.ReadPIDRecord(inst.Name)
	switch {
	case err != nil:
		f.status = worst(f.status, ui.DoctorFail)
		f.detail = joinDetail(f.detail, "pid file unreadable: "+err.Error())
	case rec.PID > 0 && !state.Alive(rec.PID):
		f.status = worst(f.status, ui.DoctorWarn)
		f.detail = joinDetail(f.detail,
			fmt.Sprintf("stale pid file: pid %d is gone (a reboot or a kill outside mabo-ctl); clear it with `mabo-ctl reset`", rec.PID))
	case rec.PID > 0:
		if err := supervisor.CheckIdentity(rec.PID); err != nil {
			f.status = worst(f.status, ui.DoctorFail)
			f.detail = joinDetail(f.detail, err.Error())
		}
	}

	// Port: free, ours, or somebody else's. PortHolder forks lsof and answers
	// a zero Holder when nothing listens — the same generous reading start
	// makes, so doctor never calls a stopped service's free port a finding.
	if inst.Port > 0 {
		if h := supervisor.PortHolder(inst.Port); h.PID != 0 && h.PID != rec.PID {
			f.status = worst(f.status, ui.DoctorWarn)
			f.detail = joinDetail(f.detail, fmt.Sprintf(
				"port %d is held by pid %d (%s) — inspect with: %s",
				inst.Port, h.PID, h.Command, supervisor.LsofCommand(inst.Port)))
		}
	}

	// Crash evidence: a death mabo-ctl observed that was not a deliberate
	// stop. The status block already shows it for the exited phase; doctor
	// surfaces it even when a newer pid file makes the service look healthy.
	if rec2, exists, err := a.st.ReadExit(inst.Name); err == nil && exists && !rec2.Stopped {
		f.status = worst(f.status, ui.DoctorWarn)
		when := ""
		if !rec2.EndedAt.IsZero() {
			when = fmt.Sprintf(", %s ago", time.Since(rec2.EndedAt).Round(time.Second))
		}
		f.detail = joinDetail(f.detail, fmt.Sprintf(
			"previous run exited abnormally (exit code %d%s) — see %s",
			rec2.ExitCode, when, a.st.LogPath(inst.Name)))
	}

	return f
}

// diagnoseStateDir checks that nothing under .dev/ grew looser than the modes
// mabo-ctl writes: directories 0700, files 0600. A log readable by the group
// is how a credential a child printed leaves the machine in a backup.
func diagnoseStateDir(dir string) diagFinding {
	f := diagFinding{name: "state dir"}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // a missing .dev is fine; doctor says nothing about it
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Mode().Perm()&0o077 != 0 {
			f.status = worst(f.status, ui.DoctorWarn)
			f.detail = joinDetail(f.detail, fmt.Sprintf("%s is mode %o, want %o",
				path, info.Mode().Perm(), dirOrFilePerm(d.IsDir())))
		}
		return nil
	})
	if err != nil {
		f.status = worst(f.status, ui.DoctorWarn)
		f.detail = joinDetail(f.detail, "could not walk the state directory: "+err.Error())
	}
	return f
}

// dirOrFilePerm names the mode each kind of state entry should have.
func dirOrFilePerm(dir bool) fs.FileMode {
	if dir {
		return 0o700
	}
	return 0o600
}

// worst returns the more serious of two severities.
func worst(a, b ui.DoctorStatus) ui.DoctorStatus {
	if a > b {
		return a
	}
	return b
}

// joinDetail appends to a finding's detail, semicolon-separating the way the
// validation error list separates problems.
func joinDetail(have, add string) string {
	if have == "" {
		return add
	}
	return have + "; " + add
}
