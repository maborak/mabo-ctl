package main

import (
	"reflect"
	"testing"

	"github.com/spf13/cobra"
)

// TestTTYBrokerDispatchPassesFlagsRaw pins the fix for a bug that made
// `tty: true` dead on every platform: the hidden broker subcommand was
// registered with Args: ArbitraryArgs but WITHOUT DisableFlagParsing, so
// cobra rejected the broker's own --log/--sock flags before RunE ran and the
// parent JSON-unmarshalled cobra's error line as a handshake. ArbitraryArgs
// bounds positional count; only DisableFlagParsing hands the argv through
// untouched, which is what the broker's own parseBrokerArgs requires.
//
// The test dispatches through the REAL cobra machinery — the path the old
// test seam (a substituted broker executable) never exercised — with RunE
// swapped out so a passing dispatch does not spawn a broker.
func TestTTYBrokerDispatchPassesFlagsRaw(t *testing.T) {
	var got []string
	cmd := (&app{}).ttyBrokerCmd()
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		got = args
		return nil
	}
	cmd.SetArgs([]string{"--log", "/tmp/x.log", "--sock", "/tmp/x.sock", "--svc", "web", "--", "/bin/sh", "-c", "true"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cobra rejected the broker's own argv: %v", err)
	}
	want := []string{"--log", "/tmp/x.log", "--sock", "/tmp/x.sock", "--svc", "web", "--", "/bin/sh", "-c", "true"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("broker received %q, want the raw argv %q", got, want)
	}
}
