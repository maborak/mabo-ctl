package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/state"
	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// activateHooks are the shell snippets `mabo-ctl activate` prints. Each one
// asks `mabo-ctl hook-status` on a directory change, passing the repository it
// last reported, and keeps the first output line (the current repository) to
// pass next time; any further line is the one-line summary, printed only when
// the repository changed. The hook never starts, stops or writes anything.
var activateHooks = map[string]string{
	"zsh": `_mabo_ctl_hook() {
  local out
  out="$(command mabo-ctl hook-status --last-root "$_MABO_CTL_ROOT" 2>/dev/null)" || return 0
  _MABO_CTL_ROOT="${out%%$'\n'*}"
  [[ "$out" == *$'\n'* ]] && print -r -- "${out#*$'\n'}"
  return 0
}
autoload -Uz add-zsh-hook
add-zsh-hook chpwd _mabo_ctl_hook
_mabo_ctl_hook
`,
	"bash": `_mabo_ctl_hook() {
  [[ "$PWD" == "$_MABO_CTL_PWD" ]] && return 0
  _MABO_CTL_PWD="$PWD"
  local out
  out="$(command mabo-ctl hook-status --last-root "$_MABO_CTL_ROOT" 2>/dev/null)" || return 0
  _MABO_CTL_ROOT="${out%%$'\n'*}"
  [[ "$out" == *$'\n'* ]] && printf '%s\n' "${out#*$'\n'}"
  return 0
}
case ";${PROMPT_COMMAND};" in
  *";_mabo_ctl_hook;"*) ;;
  *) PROMPT_COMMAND="_mabo_ctl_hook${PROMPT_COMMAND:+;$PROMPT_COMMAND}" ;;
esac
`,
	"fish": `function _mabo_ctl_hook --on-variable PWD
    set -l out (command mabo-ctl hook-status --last-root "$_MABO_CTL_ROOT" 2>/dev/null); or return 0
    set -g _MABO_CTL_ROOT $out[1]
    if test (count $out) -gt 1
        printf '%s\n' $out[2..-1]
    end
end
_mabo_ctl_hook
`,
}

// activateCmd builds `mabo-ctl activate`.
func (a *app) activateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "activate <zsh|bash|fish>",
		Short: "Print a shell hook that shows a repo's services when you cd into it",
		Long: `Activate prints a shell hook. Evaluated from your shell's rc file, it prints
one line when you enter a directory tree with a mabo-ctl.yaml:

  mabo-ctl: api, web running · worker stopped

It is display only: it never starts or stops anything, never creates
.mabo-ctl/, and reads nothing but the pid records — no YAML parsing, no health
probe, no port lookup — so it stays fast in a prompt.

  zsh:   eval "$(mabo-ctl activate zsh)"     # in ~/.zshrc
  bash:  eval "$(mabo-ctl activate bash)"    # in ~/.bashrc
  fish:  mabo-ctl activate fish | source     # in ~/.config/fish/config.fish`,
		Args:          argsOneOf("zsh", "bash", "fish"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, args []string) error {
			_, err := fmt.Fprint(a.env.Stdout, activateHooks[args[0]])
			return err
		},
	}
}

// hookStatusCmd builds the hidden `mabo-ctl hook-status` the activate hook
// calls. Its output is for the hook, not for people: the first line is the
// repository root ("" outside one), and a second line, printed only when that
// root differs from --last-root, is the summary.
func (a *app) hookStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "hook-status",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			last, _ := cmd.Flags().GetString("last-root")
			return a.runHookStatus(last)
		},
	}
	cmd.Flags().String("last-root", "", "the root the hook reported last time")
	return cmd
}

// runHookStatus answers the activate hook.
func (a *app) runHookStatus(last string) error {
	wd := a.env.Wd
	if wd == "" {
		var err error
		if wd, err = os.Getwd(); err != nil {
			return err
		}
	}
	path, _, err := config.DiscoverPath(wd)
	if err != nil {
		// Outside any repository: report no root, say nothing.
		fmt.Fprintln(a.env.Stdout)
		return nil
	}
	root := filepath.Dir(path)
	if !printable(root) {
		// The root line is a protocol field the hook captures; a path with a
		// line break or an escape sequence would split it or reach the
		// terminal. Such a checkout gets no hook output at all.
		fmt.Fprintln(a.env.Stdout)
		return nil
	}
	fmt.Fprintln(a.env.Stdout, root)
	if root == last {
		return nil
	}
	fmt.Fprintln(a.env.Stdout, "mabo-ctl: "+hookSummary(root))
	return nil
}

// hookName is the shape of a service name mabo-ctl itself writes a pid record
// for; anything else in pids/ is not echoed.
var hookName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// printable reports whether s holds no control character, C1 control or
// Unicode line break — nothing that could split a line or drive a terminal.
func printable(s string) bool {
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029 {
			return false
		}
	}
	return true
}

// hookSummary describes what runs in the checkout at root from its pid records
// alone. A record whose process is gone, or whose pid now belongs to something
// else, counts as stopped: the identity check is the same one doctor uses.
func hookSummary(root string) string {
	d := &state.Dir{Root: root} // read only: never state.New, which creates .mabo-ctl/
	entries, err := os.ReadDir(d.PIDsDir())
	if errors.Is(err, os.ErrNotExist) {
		return "nothing started here yet · mabo-ctl start"
	}
	var running, stopped []string
	for _, e := range entries {
		svc, ok := strings.CutSuffix(e.Name(), ".pid")
		if !ok || e.IsDir() || !hookName.MatchString(svc) {
			// Not a name mabo-ctl writes: never echo it to the terminal.
			continue
		}
		rec, err := d.ReadPIDRecord(svc)
		if err == nil && rec.PID > 0 && supervisor.CheckIdentity(rec.PID, rec.StartedAt) == nil {
			running = append(running, svc)
		} else {
			stopped = append(stopped, svc)
		}
	}
	sort.Strings(running)
	sort.Strings(stopped)
	switch {
	case len(running) == 0:
		return "nothing running · mabo-ctl start"
	case len(stopped) == 0:
		return strings.Join(running, ", ") + " running"
	default:
		return strings.Join(running, ", ") + " running · " + strings.Join(stopped, ", ") + " stopped"
	}
}
