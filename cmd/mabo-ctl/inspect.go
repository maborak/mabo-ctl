package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/maborak/mabo-ctl/internal/ui"
)

// defaultTailLines is how much backlog `logs` shows before it starts following.
const defaultTailLines = 50

// statusCmd builds `mabo-ctl status`.
func (a *app) statusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print one line per service: phase, port, pid, probe time, detail",
		Long: `Status prints the state of every declared service.

--json is the STABLE integration contract. Its field names and their order are
part of mabo-ctl's public interface; only stdout carries it, so the port-override
notice and every other diagnostic go to stderr and cannot corrupt it.

Status always exits 0 when it managed to report. A service being down is
information, not a failure of the command.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runStatus(cmd, boolFlag(cmd, "json"))
		},
	}
	cmd.Flags().Bool("json", false, "emit the stable machine-readable status contract on stdout")
	return cmd
}

// runStatus prints the status block, or the stable JSON contract when asJSON.
func (a *app) runStatus(cmd *cobra.Command, asJSON bool) error {
	// Recorded before anything resolves: --json on a TTY must not grow the
	// port-drift prompt, because ui.StatusJSON is the machine contract and
	// nothing human may interleave with it.
	a.jsonContract = asJSON
	sup, _, err := a.supervisor()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible(cmd.Context())
	defer cancel()

	sts := sup.Status(ctx)
	if !asJSON {
		a.printStatus(sts)
		return nil
	}
	b, err := ui.StatusJSON(sts)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.env.Stdout, string(b))
	return nil
}

// healthCmd builds `mabo-ctl health`.
func (a *app) healthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Probe every declared health URL in parallel",
		Long: `Health probes every service that declares a health URL, all at once.

The probe sends HEAD, falling back to GET only on 405 or 501, and never reads
the response body: receiving the response headers IS readiness. Any HTTP status
counts as answering, including 4xx and 5xx — the question is whether the server
is up, not whether one route is happy.

Health reports exactly the phases "mabo-ctl status" reports, because it asks the
supervisor the same question rather than probing separately. It differs only in
what it does with the answer: status always exits 0 because a service being down
is information, and health exits 4 when any declared health URL did not answer,
because that is the question it was asked.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sup, _, err := a.supervisor()
			if err != nil {
				return err
			}
			ctx, cancel := interruptible(cmd.Context())
			defer cancel()

			sts := healthStatus(sup.Status(ctx))
			a.printStatus(sts)

			var down []string
			for _, st := range sts {
				if st.Health != "" && st.Phase != supervisor.PhaseReady {
					down = append(down, st.Name)
				}
			}
			if len(down) > 0 {
				return withCode(exitNotReady,
					fmt.Errorf("%s did not answer their health URL", joinAnd(down)))
			}
			return nil
		},
	}
}

// healthStatus is the supervisor's statuses with one annotation `mabo-ctl health`
// adds and `mabo-ctl status` does not: naming the services there was nothing to
// ask about.
//
// It adds no phase and overrides no detail. That matters more than it looks:
// this command used to derive its OWN phases from its OWN probe loop, so one
// service, at one instant, read "slow" from status and "failed" from health —
// two answers and two exit codes for a script to branch on. The derivation now
// lives in exactly one place, and this function is deliberately too small to
// grow into a second one.
func healthStatus(sts []supervisor.Status) []supervisor.Status {
	out := make([]supervisor.Status, len(sts))
	copy(out, sts)
	for i := range out {
		// Keyed off the status's own Health field rather than a second lookup
		// of the same fact, so the note can never contradict the URL printed
		// beside it.
		if out[i].Health == "" && out[i].Detail == "" {
			out[i].Detail = "no health check declared"
		}
	}
	return out
}

// logsCmd builds `mabo-ctl logs`, aliased `tailf`.
func (a *app) logsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "logs [service|all]",
		Aliases: []string{"tailf"},
		Short:   "Show the last lines of a service log, optionally following",
		Long: `Logs prints the tail of a service's log from .dev/logs/<service>.log.

With no argument, or with "all", it interleaves every service's log and prefixes
each line with the service label so the streams stay distinguishable. Following
stops on Ctrl-C.

--timestamps (follow only) prefixes each line with the time it was READ. For a
live follow that is honest to within a heartbeat; replayed against a historical
tail it would be a lie, so historical tails refuse the flag.

Logs are truncated when a service starts, so what is here belongs to the current
run.`,
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := cmd.Flags().GetInt("tail")
			if err != nil {
				return usageError(err)
			}
			if n < 0 {
				return usageErrorf("--tail must not be negative, got %d", n)
			}
			var names []string
			if len(args) == 1 && args[0] != "all" {
				if err := a.validateNames(cmd, args); err != nil {
					return err
				}
				names = args
			}
			follow := boolFlag(cmd, "follow")
			stamp := boolFlag(cmd, "timestamps")
			if stamp && !follow {
				// A read-time stamp on a HISTORICAL tail is the time the tailer
				// read the line, which has nothing to do with when the service
				// wrote it. Presenting that as a timestamp is a small lie, and
				// small lies in a tool whose value is not lying compound.
				return usageErrorf("--timestamps is follow-only (-f): stamps on a historical tail would be read times, not write times")
			}
			return a.runTail(cmd, names, n, follow, stamp)
		},
	}
	cmd.Flags().Int("tail", defaultTailLines, "how many trailing lines to show before following")
	cmd.Flags().BoolP("follow", "f", false, "keep streaming new lines until interrupted")
	cmd.Flags().Bool("timestamps", false,
		"prefix each followed line with the time it was read (requires -f); format HH:MM:SS.mmm")
	return cmd
}

// runTail streams the logs of names — every service when names is empty — to
// stdout.
//
// Each service gets a goroutine reading from the supervisor and a second one
// forwarding its lines into a shared channel; all of them have finished before
// runTail returns. When more than one service is being followed each line is
// prefixed with that service's coloured, fixed-width label, because interleaved
// logs with no attribution are worse than no logs.
//
// stamp (only reachable with follow, enforced at the flag) prefixes each line
// with the moment THIS process read it — near-honest for a live follow,
// meaningless for anything older, which is why the historical tail refuses it.
//
// It blocks until every stream ends, or until ctx is cancelled by Ctrl-C.
func (a *app) runTail(cmd *cobra.Command, names []string, n int, follow, stamp bool) error {
	sup, insts, err := a.supervisor()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		names = make([]string, 0, len(insts))
		for _, in := range insts {
			names = append(names, in.Name)
		}
	}
	if len(names) == 0 {
		return errors.New("mabo-ctl: no services to tail")
	}

	ctx, cancel := interruptible(cmd.Context())
	defer cancel()

	type labelled struct {
		svc, text string
	}
	merged := make(chan labelled, 256)
	errs := make([]error, len(names))

	var wg sync.WaitGroup
	for i, name := range names {
		// Tail owns lines and closes it when it returns, so the forwarder below
		// terminates on its own and this side must never close it.
		lines := make(chan string, 128)
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs[i] = sup.Tail(ctx, name, n, follow, lines)
		}()
		go func() {
			defer wg.Done()
			for line := range lines {
				merged <- labelled{svc: name, text: line}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(merged)
	}()

	prefix := len(names) > 1
	r := a.renderer()
	for l := range merged {
		text := l.text
		if stamp {
			text = time.Now().Format("15:04:05.000") + " " + text
		}
		if prefix {
			fmt.Fprintln(a.env.Stdout, r.ServiceLabel(l.svc)+"  "+text)
			continue
		}
		fmt.Fprintln(a.env.Stdout, text)
	}

	// Safe to read errs: every writer finished before merged was closed.
	if err := errors.Join(errs...); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// openCmd builds `mabo-ctl open`.
func (a *app) openCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "open",
		Short: "Open every running service's URL in the default browser",
		Long: `Open hands the base URL of each running service to the platform opener — open
on macOS, xdg-open on Linux.

The URL is derived from the service's health URL, or from http://localhost:<port>
when it declares no health check, and is passed to the opener as a separate
argument. It is never interpolated into a shell command line, and a URL whose
scheme is not http or https is refused: the value comes from mabo-ctl.yaml, and
handing an arbitrary scheme to the desktop's URL handler is not something a
process supervisor should do.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          a.runOpen,
	}
}

// runOpen opens the URL of every service that is currently up.
func (a *app) runOpen(cmd *cobra.Command, _ []string) error {
	sup, insts, err := a.supervisor()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible(cmd.Context())
	defer cancel()

	byName := make(map[string]service.Instance, len(insts))
	for _, in := range insts {
		byName[in.Name] = in
	}

	opened := 0
	var problems []error
	for _, st := range sup.Status(ctx) {
		// Every phase with a live process behind it, degraded included: a
		// service that is up and not answering is precisely the one whose page
		// the developer wants to look at.
		switch st.Phase {
		case supervisor.PhaseReady, supervisor.PhaseRunning,
			supervisor.PhaseSlow, supervisor.PhaseDegraded:
		default:
			continue
		}
		url, err := browseURL(byName[st.Name])
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if url == "" {
			continue
		}
		fmt.Fprintf(a.env.Stderr, "opening %s for %s\n", url, st.Name)
		if err := a.env.OpenURL(ctx, url); err != nil {
			problems = append(problems, fmt.Errorf("open %s for %s: %w", url, st.Name, err))
			continue
		}
		opened++
	}

	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	if opened == 0 {
		fmt.Fprintln(a.env.Stderr, "no running service has a URL to open")
	}
	return nil
}

// browseURL derives the URL to hand the platform opener for one instance.
//
// An explicit `open:` wins: an absolute http(s) URL is used as-is, and a path
// such as "/docs" is joined against the service's derived origin — that origin
// being the health URL's, or http://localhost:<port>, exactly what open would
// have used before the field existed. A tcp or exec probe is not a page anyone
// can open, so such a service falls back to its port's origin.
//
// It returns an error for an open target or health URL whose scheme is not
// http or https. Those values are attacker-influenced in the sense that
// whoever writes mabo-ctl.yaml controls them, and the platform opener will
// happily launch a handler for any scheme at all.
func browseURL(in service.Instance) (string, error) {
	origin := ""
	switch in.Readiness().Kind {
	case service.ProbeNone, service.ProbeHTTP:
		if in.Health != "" {
			u, err := parseHTTPURL(in.Health)
			if err != nil {
				return "", fmt.Errorf("service %q: health URL %q cannot be opened: %w", in.Name, in.Health, err)
			}
			u.Path, u.RawQuery, u.Fragment = "/", "", ""
			origin = u.String()
		}
	}
	if origin == "" {
		origin = openPortOrigin(in)
	}

	if in.Open != "" {
		if strings.HasPrefix(in.Open, "/") {
			if origin == "" {
				return "", fmt.Errorf("service %q: open %q is a path but the service has no origin to join it against; "+
					"use a full http(s) URL or give it a port", in.Name, in.Open)
			}
			return strings.TrimRight(origin, "/") + in.Open, nil
		}
		u, err := parseHTTPURL(in.Open)
		if err != nil {
			return "", fmt.Errorf("service %q: open %q must be a path starting with / or an absolute http(s) URL: %w",
				in.Name, in.Open, err)
		}
		return u.String(), nil
	}
	return origin, nil
}

// openPortOrigin is browseURL's fallback: the service's own port as a loopback
// origin, or "" when it declares no port.
func openPortOrigin(in service.Instance) string {
	if in.Port > 0 {
		return fmt.Sprintf("http://localhost:%d/", in.Port)
	}
	return ""
}

// checkNames is a small helper used by the shell command to list what a user
// could have typed instead.
func checkNames(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
