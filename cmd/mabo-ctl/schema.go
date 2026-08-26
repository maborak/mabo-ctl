package main

import (
	"fmt"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/spf13/cobra"
)

// schemaCmd builds `mabo-ctl schema`.
//
// Like completion, it works in a directory with no mabo-ctl.yaml: both outputs
// describe mabo-ctl itself, neither reads a config.
func (a *app) schemaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Print the JSON Schema for mabo-ctl.yaml, or --commands to catalogue this binary",
		Long: `Schema prints one of two machine-readable documents.

DEFAULT — the JSON Schema (draft-07) describing mabo-ctl.yaml, for editor
validation:

  mabo-ctl schema > mabo-ctl.schema.json

and then, in mabo-ctl.yaml:

  # yaml-language-server: $schema=./mabo-ctl.schema.json

The repository also ships the generated schema at schema/mabo-ctl.schema.json.
It is hand-mapped from the same structs the parser decodes and is drift-guarded
against the shipped example, so a field the parser accepts but the schema does
not describe is a test failure, not a surprise.

--commands — a catalogue OF THE BINARY ITSELF for programs that drive it:
every command's flags and argument semantics, the exit-code table, which
outputs are stable machine contracts, and every web-console HTTP route.
It is generated from the same command tree --help renders, so the two can
never disagree:

  mabo-ctl schema --commands > /tmp/mabo-catalogue.json`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if boolFlag(cmd, "commands") {
				b, err := buildCatalog(cmd.Root())
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(a.env.Stdout, string(b))
				return err
			}
			b, err := config.Schema()
			if err != nil {
				return fmt.Errorf("mabo-ctl: build schema: %w", err)
			}
			_, err = fmt.Fprintln(a.env.Stdout, string(b))
			return err
		},
	}
	cmd.Flags().Bool("commands", false,
		"catalogue this binary instead of the YAML schema: commands, flags, exit codes, HTTP routes")
	return cmd
}
