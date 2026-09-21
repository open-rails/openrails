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
)

// TestUpsertMerchantConfig_SeedsPSPs verifies the #593 public
// provisioning API: an embedded host declares its merchant's PSPs
// via rt.UpsertMerchantConfig (no raw SQL, no control plane), the rows
// land bound to the merchant, and re-running it is idempotent. This is the call
// host-one #426 makes from `migrate legacy`.
func TestUpsertMerchantConfig_SeedsPSPs(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	appDB := dbtest.OpenAppDB(t, dsn)
	pool := appDB.Pool()

	slug := fmt.Sprintf("embed-provision-%d", time.Now().UnixNano())
	cfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureLive, DB: &config.DBConfig{URL: dsn}}
	rt, err := embed.New(ctx, embed.Options{
		Config: cfg, River: embed.RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	m := embed.MerchantConfig{
		DisplayName: slug,
		PSPs: map[string]embed.PSPConfig{
			"mobius": {
				"nmi": {AccountID: "gateway-" + slug},
			},
			"ccbill": {
				"ccbill": {AccountID: fmt.Sprintf("999983-%d", time.Now().UnixNano())},
			},
		},
	}
	id, err := rt.UpsertMerchantConfig(ctx, slug, m)
	require.NoError(t, err)
	require.False(t, id.IsZero())
	cleanupCCBillWebhookMerchant(t, id)

	// Count only this fixture's accounts, independently of database role/RLS.
	countPSPs := func() int {
		t.Helper()
		var n int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM billing.psps WHERE merchant_id = $1 AND rail = ANY($2)`,
			id.UUID(), []string{"nmi", "ccbill"}).Scan(&n))
		return n
	}
	require.Equal(t, 2, countPSPs(), "both declared PSPs are seeded")

	// Re-running is idempotent — no error, no duplicate rows.
	_, err = rt.UpsertMerchantConfig(ctx, slug, m)
	require.NoError(t, err)
	require.Equal(t, 2, countPSPs(), "re-run does not duplicate PSPs")

	// #770: the engine is bound to `slug` now — upserting a DIFFERENT merchant
	// into the same engine must fail loudly (one embedded engine, one merchant),
	// and must fail BEFORE writing anything: no second merchant row appears.
	otherSlug := slug + "-second"
	_, err = rt.UpsertMerchantConfig(ctx, otherSlug, embed.MerchantConfig{DisplayName: otherSlug})
	require.ErrorContains(t, err, "already bound to merchant")
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM billing.merchants WHERE slug = $1`, otherSlug).Scan(&n))
	require.Zero(t, n, "refused second merchant must not be provisioned")
}
