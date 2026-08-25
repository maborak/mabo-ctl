package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/maborak/mabo-ctl/internal/selfupdate"
	"github.com/spf13/cobra"
)

// upgradeCmd builds `mabo-ctl upgrade`.
//
// It deliberately never touches the config or the state directory: an upgrade
// must work in a directory that has no mabo-ctl.yaml at all, and it must not
// print a port-override notice in the middle of replacing a binary.
func (a *app) upgradeCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Replace this binary with the latest published release",
		Long: `Upgrade replaces the running mabo-ctl binary with the latest release
published on GitHub.

The download is verified against the release's SHA256SUMS before anything is
replaced, and the replacement is a rename over the running image, so an
interrupted upgrade can never leave a half-written binary behind. The process
that performed the upgrade keeps running the old image until it exits.

A binary built from source (a commit sha or "dev" rather than a release tag)
cannot be version-compared; upgrade says so and installs the latest release
anyway, unless it already is that exact release.`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				// A usage error, not a runtime one: exit 2 with the usage text,
				// like every other mistyped invocation.
				return usageErrorf("upgrade takes no arguments; got %q — did you mean `mabo-ctl upgrade` alone?", args[0])
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runUpgrade(cmd.Context(), force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"reinstall the latest release even when this binary is not older than it")
	return cmd
}

// runUpgrade resolves the latest release, decides whether it is newer than the
// running binary, and swaps it in. Every network and disk decision belongs to
// internal/selfupdate; this function is the conversation around it.
func (a *app) runUpgrade(ctx context.Context, force bool) error {
	if a.env.RunUpgrader != nil {
		return a.env.RunUpgrader(ctx, force)
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("mabo-ctl: locate this binary: %w", err)
	}
	// An install through a symlink (a user's ~/bin/mabo-ctl -> the real one)
	// must upgrade the real file, or the rename would retarget the symlink and
	// leave the old binary in place.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	fmt.Fprintln(a.env.Stderr, "checking the latest mabo-ctl release")
	rel, err := selfupdate.Latest(ctx, selfupdate.Options{Token: githubToken()})
	if err != nil {
		return err
	}

	if !force {
		switch cmp, cerr := selfupdate.Compare(version, rel.Tag); {
		case cerr != nil:
			// Not built from a tag: a sha cannot be ordered against a tag, so
			// say so and let an exact tag match be the only "already there".
			fmt.Fprintf(a.env.Stderr,
				"mabo-ctl: current version %q is not a release tag; cannot version-compare against %s\n",
				version, rel.Tag)
			if version == rel.Tag {
				fmt.Fprintf(a.env.Stdout, "mabo-ctl is already %s\n", rel.Tag)
				return nil
			}
		case cmp >= 0:
			fmt.Fprintf(a.env.Stdout, "mabo-ctl %s is up to date (latest release %s)\n", version, rel.Tag)
			return nil
		}
	}

	fmt.Fprintf(a.env.Stderr, "downloading %s (%s)\n", rel.Tag, rel.AssetName)
	if err := selfupdate.Apply(ctx, selfupdate.Options{Token: githubToken()}, exe, rel); err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stdout, "upgraded %s → %s; the running process keeps the old image until it exits\n",
		version, rel.Tag)
	return nil
}

// githubToken reads the token that authenticates a private repository's
// releases. GITHUB_TOKEN first, GH_TOKEN second — the same precedence the gh
// CLI documents. Empty means anonymous, which is right for a public repo and
// gets an honest 404 for a private one.
func githubToken() string {
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("GH_TOKEN")
}
