package main

import (
	"fmt"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/spf13/cobra"
)

// schemaCmd builds `mabo-ctl schema`.
//
// Like completion, it works in a directory with no mabo-ctl.yaml: the schema
// describes the file, it does not read one.
func (a *app) schemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the JSON Schema for mabo-ctl.yaml",
		Long: `Schema writes the JSON Schema (draft-07) describing mabo-ctl.yaml to
stdout, for editor validation:

  mabo-ctl schema > mabo-ctl.schema.json

and then, in mabo-ctl.yaml:

  # yaml-language-server: $schema=./mabo-ctl.schema.json

The repository also ships the generated schema at schema/mabo-ctl.schema.json.
It is hand-mapped from the same structs the parser decodes and is drift-guarded
against the shipped example, so a field the parser accepts but the schema does
not describe is a test failure, not a surprise.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			b, err := config.Schema()
			if err != nil {
				return fmt.Errorf("mabo-ctl: build schema: %w", err)
			}
			_, err = fmt.Fprintln(a.env.Stdout, string(b))
			return err
		},
	}
}
