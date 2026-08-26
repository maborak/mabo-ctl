package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/maborak/mabo-ctl/internal/supervisor"
	"github.com/spf13/cobra"
)

// attachCmd builds `mabo-ctl attach`.
func (a *app) attachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <service>",
		Short: "Attach this terminal to a tty: service through its relay",
		Long: `Attach connects your terminal to a service declared with tty: true — the
broker mabo-ctl spawned beside it relays bytes both ways, so an interactive
process inside a supervised service becomes reachable without ever giving up
detachment: close the laptop, kill the shell that ran start, the service
survives; attach again from anywhere.

Detach with Ctrl-Q. Everything you type is forwarded one byte at a time and
nothing is echoed twice — the terminal is switched to raw mode for the session
and restored on every exit path. Only one attached terminal at a time: a second
one is told the seat is taken, not silently dropped.

The relay lives at .dev/tty/<service>.sock while the service runs. Attach
refuses a service without tty: in its declaration rather than pretending, and
names the missing socket when there is nothing to connect to yet.`,
		Args:          argsExactly(1, "attach needs one service name, e.g. mabo-ctl attach backend"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runAttach(cmd, args[0])
		},
	}
}

// runAttach resolves the name against the DECLARED services so a typo or a
// tty-less service is refused before any socket is touched.
func (a *app) runAttach(cmd *cobra.Command, name string) error {
	insts, err := a.resolve()
	if err != nil {
		return err
	}
	var found *struct {
		tty bool
	}
	for i := range insts {
		if insts[i].Name == name {
			found = &struct{ tty bool }{tty: insts[i].TTY}
			break
		}
	}
	if found == nil {
		return usageErrorf("unknown service %q; declared services are: %s", name, joinAnd(a.cfg.Names()))
	}
	if !found.tty {
		return usageErrorf("service %q does not declare tty: true; add it to make %q attachable", name, name)
	}

	sockPath := ""
	if a.st != nil {
		sockPath = a.st.TTYSockPath(name)
	}
	err = supervisor.AttachTTY(sockPath, stdinFile(a.env), a.env.Stdout)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, supervisor.ErrNoRelay):
		return fmt.Errorf("%w; start %q first — it must be running with tty: true", err, name)
	default:
		return err
	}
}

// stdinFile returns env's stdin as a file when it genuinely is one, which is
// AttachTTY's signal for whether raw mode may be attempted.
func stdinFile(env *Env) *os.File {
	if f, ok := env.Stdin.(*os.File); ok {
		return f
	}
	return nil
}

// ttyBrokerCmd registers the HIDDEN broker subcommand. Never documented, never
// completed: it exists so `mabo-ctl internal-tty-broker …` is dispatchable —
// the broker process is this same binary re-invoking itself — and invisible
// everywhere a user looks.
func (a *app) ttyBrokerCmd() *cobra.Command {
	return &cobra.Command{
		Use:    supervisor.TTYBrokerCommand,
		Hidden: true,
		Args:   cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			os.Exit(supervisor.RunTTYBroker(args, os.Stdout))
			return nil
		},
	}
}
