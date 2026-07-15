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
	"github.com/open-rails/openrails/pkg/identity"
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
	err = client.SetCustomerSpendDelegations(ctx, customerID.String(), []openrails.SpendDelegationInput{
		{
			Scope:    "invoker",
			ScopeKey: "test-invoker",
			Windows:  []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 5_000_000, Currency: "USD"}},
		},
		{
			Scope:  "role",
			RoleID: "test-role",
			Windows: []openrails.SpendLimitWindow{{
				Key: "month", WindowSeconds: 2592000, Limit: 9_000_000, Currency: "USD",
			}},
		},
	})
	require.NoError(t, err, "embedded SetCustomerSpendDelegations must pin the bound merchant itself")

	require.NoError(t, client.SetCustomerSpendDelegation(ctx, customerID.String(), openrails.SpendDelegationInput{
		Scope:    "invoker",
		ScopeKey: "test-invoker",
		// Currency is intentionally omitted: spend limits are also valid for
		// non-monetary units, and the singular upsert must preserve that contract.
		Windows: []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 123}},
	}))
	stored, err := rt.Service().InvokerSpendLimits(dbtest.WithTestMerchant(ctx), identity.CustomerID(customerID))
	require.NoError(t, err)
	require.Len(t, stored, 2, "single embedded upsert must preserve unrelated rows")
	limits := map[string]int64{}
	for _, row := range stored {
		limits[row.Scope+"\x00"+row.ScopeKey] = row.Windows[0].Limit
	}
	require.EqualValues(t, 123, limits["invoker\x00test-invoker"])
	require.EqualValues(t, 9_000_000, limits["role\x00test-role"])

	err = client.SetCustomerSpendDelegations(ctx, customerID.String(), []openrails.SpendDelegationInput{
		{
			Scope: " role ", RoleID: " test-role ",
			Windows: []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 1}},
		},
		{
			Scope: "role", ScopeKey: "test-role",
			Windows: []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 2}},
		},
	})
	require.ErrorIs(t, err, openrails.ErrInvalid)
	var embeddedStatus *openrails.StatusError
	require.ErrorAs(t, err, &embeddedStatus)
	require.Equal(t, 400, embeddedStatus.Status)
	require.Contains(t, err.Error(), "duplicate delegation for role")
	stored, err = rt.Service().InvokerSpendLimits(dbtest.WithTestMerchant(ctx), identity.CustomerID(customerID))
	require.NoError(t, err)
	require.Len(t, stored, 2, "rejected embedded duplicate document must not mutate policy")

	// Replace-with-empty exercises the delete lane through the same ctx path.
	require.NoError(t, client.SetCustomerSpendDelegations(ctx, customerID.String(), nil))
}
