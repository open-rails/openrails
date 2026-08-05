package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// retiredCommands maps a command name this CLI no longer has to the name that
// replaced it. or#893 phase 8: ONE name per command. A cobra Alias made two
// spellings work forever and let scripts, Taskfiles and docs drift apart; a
// retired name now prints the rename and exits nonzero.
var retiredCommands = map[string]string{
	"server":       "run-server",
	"push-catalog": "push-merchant-catalog",
	"dump-catalog": "dump-merchant-catalog",
}

// newRetiredCommands builds the hidden stubs. They are hidden so `--help` shows
// exactly one name per command, and they run so the operator gets the rename
// instead of cobra's bare "unknown command".
func newRetiredCommands() []*cobra.Command {
	out := make([]*cobra.Command, 0, len(retiredCommands))
	for old, replacement := range retiredCommands {
		out = append(out, newRetiredCommand(old, replacement))
	}
	return out
}

func newRetiredCommand(old, replacement string) *cobra.Command {
	return &cobra.Command{
		Use:                old,
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		// Override the root's config-loading hook: learning that a command was
		// renamed must not require a loadable config. The rename is the only
		// thing this command has to say.
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error { return nil },
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("%q was renamed to %q (or#893): run `openrails %s`", old, replacement, replacement)
		},
	}
}
