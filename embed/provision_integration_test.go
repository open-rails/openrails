//go:build integration

package embed_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/embedded"
)

// TestUpsertMerchantConfig_SeedsProviderAccounts verifies the #593 public
// provisioning API: an embedded host declares its merchant's rail provider
// accounts via rt.UpsertMerchantConfig (no raw SQL, no control plane), the rows
// land bound to the merchant, and re-running it is idempotent. This is the call
// doujins #426 makes from `migrate legacy`.
func TestUpsertMerchantConfig_SeedsProviderAccounts(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	appDB := dbtest.OpenAppDB(t, dsn)
	pool := appDB.Pool()

	slug := fmt.Sprintf("embed-provision-%d", time.Now().UnixNano())
	cfg := &config.Config{Env: "dev", DB: &config.DBConfig{URL: dsn}}
	rt, err := embed.New(ctx, embed.Options{
		Options: embedded.Options{Config: cfg},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	m := embed.MerchantConfig{
		DisplayName: slug,
		ProviderAccounts: map[string]embed.ProviderAccountConfig{
			"mobius": {
				"nmi": {Environment: "live", AccountID: "579145"},
			},
			"ccbill": {
				"ccbill": {Environment: "live", AccountID: "945280/0000"},
			},
		},
	}
	id, err := rt.UpsertMerchantConfig(ctx, slug, m)
	require.NoError(t, err)
	require.False(t, id.IsZero())

	// payment_provider_accounts is RLS-scoped: read it on a connection pinned to the
	// merchant so the policy admits its rows.
	countProviderAccounts := func() int {
		t.Helper()
		conn, err := pool.Acquire(ctx)
		require.NoError(t, err)
		defer conn.Release()
		_, err = conn.Exec(ctx, "SELECT set_config('app.merchant_id', $1, false)", id.UUID().String())
		require.NoError(t, err)
		var n int
		require.NoError(t, conn.QueryRow(ctx,
			`SELECT count(*) FROM openrails.payment_provider_accounts WHERE rail = ANY($1)`,
			[]string{"nmi", "ccbill"}).Scan(&n))
		return n
	}
	require.Equal(t, 2, countProviderAccounts(), "both declared provider accounts are seeded")

	// Re-running is idempotent — no error, no duplicate rows.
	_, err = rt.UpsertMerchantConfig(ctx, slug, m)
	require.NoError(t, err)
	require.Equal(t, 2, countProviderAccounts(), "re-run does not duplicate provider accounts")
}
