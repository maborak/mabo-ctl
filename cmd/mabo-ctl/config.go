package main

import (
	"fmt"
	"github.com/maborak/mabo-ctl/internal/config"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/maborak/mabo-ctl/internal/redact"
	"github.com/maborak/mabo-ctl/internal/ui"
)

// configLong is `mabo-ctl config`'s long help. It documents the redaction
// deliberately: a reader who does not know a value was withheld will read
// "[redacted]" as the value the service is actually configured with.
const configLong = `Config shows where mabo-ctl.yaml was loaded from and what it resolved to.

Three sections, in order:

  source     the absolute path of the mabo-ctl.yaml that won, whether it came from
             --config or from walking up the tree, the repo root, the state
             directory, and the effective stop_grace and ready_timeout
  services   per service: the resolved port AND THE PRECEDENCE LEVEL that
             produced it, the working directory, the resolved absolute command
             with the runtime that chose it, the expanded health URL, the
             declared environment and depends_on
  file       mabo-ctl.yaml itself, so nothing is hidden behind interpretation

The port source is the reason this command exists. Four levels resolve a port —
--ports, then <NAME>_PORT in the environment, then the persisted .dev/run.env,
then the value declared in mabo-ctl.yaml — and until now nothing printed which one
won, so "why is this service on 7999?" could only be answered by reading three
inputs by hand. A persisted port outranking a changed default is called out on
the port line itself.

Credential-shaped values in the health URL, the command arguments and the
declared environment are replaced with [redacted], by exactly the rules the web
console uses. --raw prints mabo-ctl.yaml byte for byte instead, unredacted: it is
the file already sitting in the working tree, and the point of --raw is to pipe
it somewhere.

  mabo-ctl config              source, resolved services, and the file
  mabo-ctl config backend      the same, narrowed to one service
  mabo-ctl config --json       the resolved view, machine-readable
  mabo-ctl config --raw        mabo-ctl.yaml verbatim, for piping`

// configCmd builds `mabo-ctl config`.
func (a *app) configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config [service]",
		Short: "Show the loaded mabo-ctl.yaml and everything it resolved to",
		Long:  configLong,
		// One optional service name. Resolution is repo-wide either way —
		// a template may read another service's port — and only the rendering
		// narrows.
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          a.runConfig,
	}
	cmd.Flags().Bool("json", false, "emit the resolved view as JSON")
	cmd.Flags().Bool("raw", false, "print mabo-ctl.yaml verbatim and nothing else; the file is NOT redacted")
	cmd.Flags().Var(&portsFlag{}, "ports", "resolve as if start had been given these port overrides, to preview what they would do")
	cmd.Flags().Var(&namedPortsFlag{}, "port", "named port override SERVICE=PORT, e.g. --port backend=7999; repeatable. Cannot be combined with --ports")
	return cmd
}

// runConfig prints the configuration in whichever of the three forms was asked
// for.
func (a *app) runConfig(cmd *cobra.Command, args []string) error {
	asJSON, asRaw := boolFlag(cmd, "json"), boolFlag(cmd, "raw")
	if asJSON && asRaw {
		return usageErrorf("--raw and --json cannot be combined: --raw prints mabo-ctl.yaml verbatim, which is not JSON")
	}

	if asRaw {
		if len(args) > 0 {
			return usageErrorf(
				"--raw prints the whole mabo-ctl.yaml and cannot be narrowed to a service; drop --raw to see just %s", args[0])
		}
		// --raw deliberately does NOT validate. It reads a path and writes the
		// bytes back, and the moment you most need to look at a mabo-ctl.yaml is
		// when it does not parse — a `config --raw` that refuses to show you an
		// invalid file is useless in exactly its best use case.
		path, err := a.locateConfig()
		if err != nil {
			return err
		}
		return a.printRawConfig(path)
	}

	cfg, err := a.config()
	if err != nil {
		// The resolved view genuinely needs a valid config, but the Source
		// section does not — and knowing WHICH file failed is most of what a
		// reader wants here. Print it, then the problems.
		if path, perr := a.locateConfig(); perr == nil {
			fmt.Fprintf(a.env.Stdout, "config file  %s\n", path)
			fmt.Fprintf(a.env.Stdout, "state dir    %s\n\n", filepath.Join(filepath.Dir(path), ".dev"))
		}
		return err
	}

	if err := a.adoptPorts(cmd); err != nil {
		return err
	}
	if len(args) == 1 {
		if err := a.validateNames(cmd, args); err != nil {
			return err
		}
	}

	// Every service resolves, even when one is named: ports are resolved for
	// the whole repo before templates expand, because {{.Port "backend"}} in
	// one service reads another's resolved port. Only the rendering narrows.
	insts, err := a.resolve()
	if err != nil {
		return err
	}
	shown := insts
	if len(args) == 1 {
		shown = nil
		for _, in := range insts {
			if in.Name == args[0] {
				shown = append(shown, in)
			}
		}
	}

	view := ui.BuildConfigView(ui.ConfigInput{
		Config:    cfg,
		Instances: shown,
		Origins:   a.origins,
		StateDir:  a.stateDir(),
		Explicit:  a.configPath != "",
	})

	if asJSON {
		body, err := ui.ConfigJSON(view)
		if err != nil {
			return err
		}
		fmt.Fprintln(a.env.Stdout, string(body))
		return nil
	}

	// The file is shown whole or not at all: it is one document, and a
	// per-service slice of YAML would be mabo-ctl's guess at what the operator
	// wrote rather than what they wrote.
	raw := ""
	if len(args) == 0 {
		if b, err := os.ReadFile(cfg.Path); err == nil {
			raw = redact.YAML(string(b))
		} else {
			fmt.Fprintf(a.env.Stderr, "mabo-ctl: cannot read %s: %v\n", cfg.Path, err)
		}
	}
	fmt.Fprintln(a.env.Stdout, a.renderer().ConfigBlock(view, raw))
	return nil
}

// printRawConfig copies mabo-ctl.yaml to stdout unchanged.
//
// Byte for byte, with no trailing newline added and NO REDACTION: this is the
// file already in the operator's working tree, and the whole point of --raw is
// that `mabo-ctl config --raw | yq …` sees what a reader of the file would. Every
// other path through this command redacts.
func (a *app) printRawConfig(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("mabo-ctl: read %s: %w", path, err)
	}
	if _, err := a.env.Stdout.Write(b); err != nil {
		return fmt.Errorf("mabo-ctl: write %s to stdout: %w", path, err)
	}
	return nil
}

// stateDir returns the absolute path of `.dev`, or "" when it has not been
// created — which is the case before anything has resolved.
func (a *app) stateDir() string {
	if a.st == nil {
		return ""
	}
	return a.st.Path()
}

// locateConfig finds the mabo-ctl.yaml that WOULD be loaded, without parsing or
// validating it.
//
// It exists so `mabo-ctl config --raw` can print a file that does not parse. The
// normal path goes through config.Load, which validates — correct everywhere
// else, and exactly wrong for the one command whose job is to help you look at
// a broken file.
func (a *app) locateConfig() (string, error) {
	if a.configPath != "" {
		abs, err := filepath.Abs(a.configPath)
		if err != nil {
			return "", fmt.Errorf("resolve --config %q: %w", a.configPath, err)
		}
		if _, err := os.Stat(abs); err != nil {
			return "", fmt.Errorf("read %s: %w", abs, err)
		}
		return abs, nil
	}
	// Walk from the SAME root a.config() discovers from, not from os.Getwd():
	// env.Wd is injectable, which is what lets the CLI be tested without
	// chdir-ing the test process.
	dir := a.env.Wd
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return "", fmt.Errorf("working directory: %w", err)
		}
	}
	// Bounded exactly like config.Discover, and it must STAY that way: this
	// function exists to name the file that command would load, so a walk that
	// went further than the real one would report a config mabo-ctl will not use.
	path, _, err := config.DiscoverPath(dir)
	if err != nil {
		return "", err
	}
	return path, nil
}
