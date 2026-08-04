//go:build integration

package integrationharness

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#890: SeedRailMerchantAccounts used to carry only the natural key
// (rail/environment/account_id), so psps.key stayed NULL on every seeded
// account and the #848 wire selector — "declared PSP key wins, unambiguous
// rail-kind fallback" — could never name one. A merchant's SECOND armed PSP
// then made every checkout ambiguous with no way to disambiguate, and tests
// papered over it by re-stamping the key themselves.
//
// This drives the real seeder into a real standalone runtime and asserts both
// halves of the selector against two armed NMI PSPs: each declared key picks
// ITS account, and the bare rail kind refuses with both keys named.
func TestSeedRailMerchantAccountsStampsPSPKeyForWireSelection(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	rt := surface.App().Runtime
	require.NotNil(t, rt.CheckoutService)

	// A dedicated merchant: two armed NMI PSPs on the shared test merchant
	// would make bare-"nmi" resolution ambiguous for every other test here.
	nano := time.Now().UnixNano()
	mid := merchant.ID(uuid.New())
	pool := h.Pool()
	_, err := pool.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		mid.UUID(), fmt.Sprintf("psp890-%d", nano))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.psps WHERE merchant_id = $1`, mid.UUID())
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.merchant_secrets WHERE merchant_id = $1`, mid.UUID())
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.merchants WHERE id = $1`, mid.UUID())
	})

	keyA := fmt.Sprintf("mobius-a-%d", nano)
	keyB := fmt.Sprintf("mobius-b-%d", nano)
	accountA := fmt.Sprintf("gw-a-%d", nano)
	accountB := fmt.Sprintf("gw-b-%d", nano)

	SeedRailMerchantAccounts(ctx, t, rt, mid, config.PSPSet{
		keyA: {Rail: models.RailNMI, AccountID: accountA, NMI: &config.NMIRailConfig{SecurityKey: "sk-" + accountA}},
		keyB: {Rail: models.RailNMI, AccountID: accountB, NMI: &config.NMIRailConfig{SecurityKey: "sk-" + accountB}},
	})

	// 1. The seeder stamps the declared manifest key on the row.
	rows, err := pool.Query(ctx,
		`SELECT key, account_id FROM openrails.psps WHERE merchant_id = $1`, mid.UUID())
	require.NoError(t, err)
	defer rows.Close()
	seeded := map[string]string{}
	for rows.Next() {
		var key *string
		var account string
		require.NoError(t, rows.Scan(&key, &account))
		require.NotNil(t, key, "psps.key must be stamped for account %s", account)
		seeded[*key] = account
	}
	require.NoError(t, rows.Err())
	require.Equal(t, map[string]string{keyA: accountA, keyB: accountB}, seeded)

	// 2. Each declared key selects ITS account on the checkout wire.
	env := config.ExpectedProviderEnvironment(rt.Config.IsTestMode())
	mctx := merchant.WithID(ctx, mid)
	for key, account := range map[string]string{keyA: accountA, keyB: accountB} {
		require.NoError(t, rt.CheckoutService.CheckoutRailUsable(mctx, key), "PSP key %q must be usable", key)
		got := rt.CheckoutService.ResolvePSPID(mctx, key)
		require.NotNil(t, got, "PSP key %q must resolve to an armed account", key)
		require.Equal(t, merchants.PspID(string(models.RailNMI), env, account), *got)
	}

	// 3. The bare rail kind stays ambiguous — and names both keys, which is only
	// possible because the keys are stamped.
	err = rt.CheckoutService.CheckoutRailUsable(mctx, string(models.RailNMI))
	var ambiguous *checkout.AmbiguousRailError
	require.ErrorAs(t, err, &ambiguous)
	require.ElementsMatch(t, []string{keyA, keyB}, ambiguous.Keys)
}
