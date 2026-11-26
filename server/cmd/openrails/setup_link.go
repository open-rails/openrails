package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/server/internal/hostconfig"
)

// newSetupLinkCmd issues the break-glass setup link: like the one run-server
// logs while no account holds a root role, but it works while operators exist.
func newSetupLinkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup-link",
		Short: "Issue a single-use link, valid 24 hours, that creates an owner account (break-glass: works while operators exist, replaces any live link)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if hostconfig.FromContext(ctx).Server.AdminConsole.Disabled {
				return errors.New("setup-link: the link opens the console's /admin/setup, and admin_console.disabled omits the console")
			}
			c, err := openCLIControlPlane(ctx)
			if err != nil {
				return err
			}
			defer c.close()
			link, err := c.cp.Core().IssueSetupLink(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Open it to create an owner account; single use, until %s:\n", link.ExpiresAt.UTC().Format(time.RFC3339))
			_, err = fmt.Fprintln(cmd.OutOrStdout(), link.URL)
			return err
		},
	}
}
