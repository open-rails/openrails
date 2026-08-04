//go:build integration

package main

import (
	"context"
	"io"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
)

// or#888: `openrails intents` / `intents-log` / `ledger-audit` opened a pool
// straight from cfg.DB and checked nothing, so an operator running them against
// the owner/admin role got every merchant_isolation policy skipped on commands
// that read and write merchant-scoped state. They now share openCLIDB, the same
// posture-checked door or#885 gave the embedded entry points. These tests drive
// the real cobra commands, not openCLIDB directly, so a future command that
// re-opens its own pool is caught here.

func cliCmdContext(dsn, env string) context.Context {
	cfg := &config.Config{
		Env:               env,
		TestMode:          config.CredentialPostureSandbox,
		ProviderWriteMode: config.ProviderWriteModeReadOnly,
		DB:                &config.DBConfig{URL: dsn},
	}
	return context.WithValue(context.Background(), config.ConfigContextKey, cfg)
}

func runCLI(t *testing.T, cmd *cobra.Command, ctx context.Context, args ...string) error {
	t.Helper()
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors = true
	return cmd.ExecuteContext(ctx)
}

// TestCLICommandsRefuseBypassRLSRoleOutsideDev: every merchant-scoped operator
// command must refuse a superuser/BYPASSRLS connection outside development.
func TestCLICommandsRefuseBypassRLSRoleOutsideDev(t *testing.T) {
	superDSN, _ := dbtest.SharedRLSPostgres(t)
	ctx := cliCmdContext(superDSN, "staging")

	for name, build := range map[string]func() *cobra.Command{
		"intents":      newIntentsCmd,
		"intents-log":  newIntentsLogCmd,
		"ledger-audit": newLedgerAuditCmd,
	} {
		t.Run(name, func(t *testing.T) {
			err := runCLI(t, build(), ctx, "--merchant="+dbtest.TestMerchantSlug)
			require.Error(t, err, "%s must refuse a BYPASSRLS role outside development", name)
			require.ErrorContains(t, err, "bypasses RLS")
			require.ErrorContains(t, err, "openrails_app")
		})
	}
}

// TestCLICommandsAcceptAppRole: the same commands get past the gate on the
// unprivileged openrails_app role. They may still fail for ordinary reasons
// (RLS hides rows the CLI has no merchant GUC for) — what must never appear is
// the posture rejection.
func TestCLICommandsAcceptAppRole(t *testing.T) {
	_, appDSN := dbtest.SharedRLSPostgres(t)
	ctx := cliCmdContext(appDSN, "staging")

	for name, build := range map[string]func() *cobra.Command{
		"intents":      newIntentsCmd,
		"intents-log":  newIntentsLogCmd,
		"ledger-audit": newLedgerAuditCmd,
	} {
		t.Run(name, func(t *testing.T) {
			err := runCLI(t, build(), ctx, "--merchant="+dbtest.TestMerchantSlug)
			if err != nil {
				require.NotContains(t, err.Error(), "bypasses RLS", "%s must pass the posture gate on openrails_app", name)
			}
		})
	}
}

// TestCLIDevelopmentWarnsOnBypassRLSRole mirrors the server/embedded gate:
// development only warns, so a local privileged DSN stays usable.
func TestCLIDevelopmentWarnsOnBypassRLSRole(t *testing.T) {
	superDSN, _ := dbtest.SharedRLSPostgres(t)
	cfg := &config.Config{
		Env:      "development",
		TestMode: config.CredentialPostureSandbox,
		DB:       &config.DBConfig{URL: superDSN},
	}
	database, err := openCLIDB(context.Background(), cfg)
	require.NoError(t, err, "development must only warn, never fail, on a bypass-RLS role")
	require.NoError(t, database.Close())
}

// TestOpenCLIDBRequiresConfig: no config, no door.
func TestOpenCLIDBRequiresConfig(t *testing.T) {
	_, err := openCLIDB(context.Background(), nil)
	require.ErrorContains(t, err, "config not loaded")
	_, err = openCLIDB(context.Background(), &config.Config{Env: "staging"})
	require.ErrorContains(t, err, "config not loaded")
}
