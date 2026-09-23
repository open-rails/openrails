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

// Explicit merchant scope makes these operator commands independent of DB role flags.

func cliCmdContext(dsn, env string) context.Context {
	cfg := &config.Config{

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

func TestCLICommandsAcceptOwnerConnection(t *testing.T) {
	superDSN, _ := dbtest.SharedRLSPostgres(t)
	ctx := cliCmdContext(superDSN, "staging")

	for name, build := range map[string]func() *cobra.Command{
		"intents":      newIntentsCmd,
		"intents-log":  newIntentsLogCmd,
		"ledger-audit": newLedgerAuditCmd,
	} {
		t.Run(name, func(t *testing.T) {
			err := runCLI(t, build(), ctx, "--merchant=id:"+dbtest.TestMerchantID.String())
			require.NoError(t, err)
		})
	}
}

func TestCLICommandsAcceptAppRole(t *testing.T) {
	_, appDSN := dbtest.SharedRLSPostgres(t)
	ctx := cliCmdContext(appDSN, "staging")

	for name, build := range map[string]func() *cobra.Command{
		"intents":      newIntentsCmd,
		"intents-log":  newIntentsLogCmd,
		"ledger-audit": newLedgerAuditCmd,
	} {
		t.Run(name, func(t *testing.T) {
			err := runCLI(t, build(), ctx, "--merchant=id:"+dbtest.TestMerchantID.String())
			require.NoError(t, err)
		})
	}
}

func TestCLIOpensOwnerConnection(t *testing.T) {
	superDSN, _ := dbtest.SharedRLSPostgres(t)
	cfg := &config.Config{
		TestMode: config.CredentialPostureSandbox,
		DB:       &config.DBConfig{URL: superDSN},
	}
	database, err := openCLIDB(context.Background(), cfg)
	require.NoError(t, err)
	require.NoError(t, database.Close())
}

// TestOpenCLIDBRequiresConfig: no config, no door.
func TestOpenCLIDBRequiresConfig(t *testing.T) {
	_, err := openCLIDB(context.Background(), nil)
	require.ErrorContains(t, err, "config not loaded")
	_, err = openCLIDB(context.Background(), &config.Config{})
	require.ErrorContains(t, err, "config not loaded")
}
