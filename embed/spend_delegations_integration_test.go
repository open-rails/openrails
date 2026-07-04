//go:build integration

package embed

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/embedded"
)

// Regression: the transcribed SetCustomerSpendDelegations bypasses the
// in-process transport, so it must pin the bound merchant onto ctx itself
// (localClient.merchantCtx). Before the fix every embedded call failed
// merchant.Require and surfaced as a 500.
func TestEmbeddedClientSetCustomerSpendDelegations(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, "b6b6b6b6-0000-4000-8000-000000000042")

	cfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureLive, DB: &config.DBConfig{URL: dsn}}
	rt, err := New(ctx, Options{Options: embedded.Options{Config: cfg}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	rt.emb.App().Runtime.SetConfiguredMerchant(dbtest.TestMerchantID)

	client := rt.Client()
	err = client.SetCustomerSpendDelegations(ctx, customerID.String(), []openrails.SpendDelegationInput{{
		Scope:    "invoker",
		ScopeKey: "test-invoker",
		Windows:  []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 5_000_000, Currency: "USD"}},
	}})
	require.NoError(t, err, "embedded SetCustomerSpendDelegations must pin the bound merchant itself")

	// Replace-with-empty exercises the delete lane through the same ctx path.
	require.NoError(t, client.SetCustomerSpendDelegations(ctx, customerID.String(), nil))
}
