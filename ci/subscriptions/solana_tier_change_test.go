//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/solanafake"
)

// A Solana tier change takes effect at once: a downgrade grants the new plan
// for the rest of the paid period with no payment. So it stays inside the
// subscription's tier group, and its direction comes from what the plans
// cost per hour, never from rank alone. A product with no tier group, priced
// ten times the current plan, is refused on change-tier and as a checkout; a
// pricier plan of lower rank is an upgrade whose transaction pulls the
// difference, and a cheaper plan of higher rank is a downgrade that pulls
// nothing. The wallet's transaction is change-tier's next action.
func TestSolanaTierChangeStaysInGroupAndPaysForMore(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	w.selfService = true
	fake, merchantKey := withSolana(t, w)
	w.start()
	mint := solanago.MustPublicKeyFromBase58(solanafake.DevnetDUSDMint)
	fake.Fund(merchantKey.PublicKey(), mint, 0)

	sfx := uuid.NewString()[:8]
	type plan struct {
		key, group string
		rank       int
		amount     int64
	}
	plans := []plan{
		{"basic", "membership", 2, 5_000_000},
		{"vip", "", 0, 50_000_000},
		{"plus", "membership", 1, 60_000_000},
		{"lite", "membership", 3, 3_000_000},
	}
	var doc strings.Builder
	doc.WriteString("schema_version: 1\nproducts:\n")
	for i, p := range plans {
		id := 5001 + i
		pda, err := fake.Plan(merchantKey.PublicKey(), uint64(id), solanafake.DevnetDUSDMint, uint64(p.amount), monthHours)
		require.NoError(t, err)
		fmt.Fprintf(&doc, "- key: %s-%s\n  display_name: %s\n", p.key, sfx, p.key)
		if p.group != "" {
			fmt.Fprintf(&doc, "  tier_group: %s\n  tier_rank: %d\n", p.group, p.rank)
		}
		fmt.Fprintf(&doc, `  prices:
  - key: %[1]s-%[2]s-monthly
    currency: usd
    unit_amount: %[3]d
    billing_interval_hours: 720
    access_duration_hours: 720
    psps: [solana]
    psp_links:
      solana:
        plan_pda: %[4]s
        plan_id: "%[5]d"
  entitlements_spec:
    %[1]s-%[2]s: null
`, p.key, sfx, p.amount, pda, id)
	}
	params, err := catalog.ParseApplicationYAML([]byte(doc.String()))
	require.NoError(t, err)
	_, err = w.client[embedded].ApplyCatalog(t.Context(), params)
	require.NoError(t, err)
	price := func(key string) string { return priceID(t, w, key+"-"+sfx, key+"-"+sfx+"-monthly") }

	b := &solanaBuyer{customer: w.newCustomer(), wallet: solanago.NewWallet().PrivateKey}
	fake.Fund(b.wallet.PublicKey(), mint, 500_000_000)
	shop := &solanaShop{w: w, fake: fake, merchant: merchantKey, mint: mint, option: w.options(billing.GetCheckoutConfigParams{ProductKey: "basic-" + sfx, PriceKey: "basic-" + sfx + "-monthly"})["solana"], price: price("basic"), key: "basic-" + sfx}
	c := shop.checkout(t, b, b.wallet.PublicKey())
	status, out := b.confirm(c, shop.land(t, signAs(t, c.bundle, b.wallet), w.clock.Now()))
	require.Equal(t, http.StatusOK, status, "%v", out)
	sub := unwrap(out)["subscription_id"].(string)
	w.advance(time.Minute)
	require.True(t, b.entitled("basic-"+sfx))

	// The ungrouped product is refused on every route, and grants nothing.
	status, out = b.call(http.MethodPost, "/subscriptions/"+sub+"/change-tier", "tc-"+uuid.NewString(), map[string]any{"price_id": price("vip")})
	require.Equal(t, http.StatusBadRequest, status, "%v", out)
	require.Contains(t, fmt.Sprint(out), "tier group")
	status, out = b.call(http.MethodPost, "/subscriptions/"+sub+"/change-tier", "tc-"+uuid.NewString(), map[string]any{"price_id": price("vip"), "signature": solanago.Signature{}.String()})
	require.Equal(t, http.StatusBadRequest, status, "%v", out)
	require.False(t, b.entitled("vip-"+sfx))
	require.True(t, b.entitled("basic-"+sfx))

	for _, tc := range []struct {
		target, kind string
		pulls        int
	}{
		{"plus", "upgrade", 1},
		{"lite", "downgrade", 0},
	} {
		prep := unwrap(b.must(http.MethodPost, "/subscriptions/"+sub+"/change-tier", "tc-"+uuid.NewString(), map[string]any{"price_id": price(tc.target)}))
		require.Equal(t, tc.kind, prep["action"], "%s: direction comes from the price per hour", tc.target)
		require.Equal(t, "requires_action", prep["status"])
		next := prep["next_action"].(map[string]any)
		require.Equal(t, "solana_sign_transactions", next["type"])
		tx, err := solanago.TransactionFromBase64(next["transactions"].([]any)[0].(string))
		require.NoError(t, err)
		pulls := 0
		for _, ix := range instructions(t, tx) {
			if isPull(ix) {
				pulls++
			}
		}
		require.Equal(t, tc.pulls, pulls, "%s: pulls in the switch", tc.target)
	}
}
