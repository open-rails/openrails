//go:build integration

package embed_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
)

// TestMerchantConstructorSeedsPSPs verifies the #593 public
// provisioning API: an embedded host declares its merchant's PSPs
// at construction (no raw SQL, no control plane), the rows
// land bound to the merchant, and re-running it is idempotent. This is the call
// host-one #426 makes from `migrate legacy`.
func TestMerchantConstructorSeedsPSPs(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	appDB := dbtest.OpenAppDB(t, dsn)
	pool := appDB.Pool()

	slug := fmt.Sprintf("embed-provision-%d", time.Now().UnixNano())
	cfg := &config.Config{TestMode: config.CredentialPostureLive, DB: &config.DBConfig{URL: dsn}}
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

	rt, id, err := newDeclaredMerchant(ctx, embed.Options{
		Config: cfg, River: embed.RiverManagedByOpenRails(),
	}, slug, m)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
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

	// Reconstructing the runtime reconciles the same merchant and PSP identities.
	require.NoError(t, rt.Close(ctx))
	restarted, again, err := newDeclaredMerchant(ctx, embed.Options{Config: cfg, River: embed.RiverManagedByOpenRails()}, slug, m)
	require.NoError(t, err)
	t.Cleanup(func() { _ = restarted.Close(context.Background()) })
	require.Equal(t, id, again)
	require.Equal(t, 2, countPSPs(), "restart does not duplicate PSPs")
	// The one-merchant binding cannot be overridden by a client selector.
	_, err = restarted.Client(openrails.WithMerchantID(merchant.ID(uuid.New())))
	require.ErrorContains(t, err, id.String())
}
