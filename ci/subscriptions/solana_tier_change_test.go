//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/solanafake"
)

// Tier changes settle the signed quote. Later credit comes from actual paid
// periods, not list prices or a no-transfer downgrade.
func TestSolanaTierChangeStaysInGroupAndPaysForMore(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	// This journey deliberately sends multiple confirmation retries in one burst.
	w.cfg = func(c *config.Config) {
		c.RateLimits = config.DefaultRateLimits()
		(*c.RateLimits)["subscribe"].RequestsPerMinute = 50
	}
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
		{"higher", "membership", 4, 90_000_000},
		{"premium", "membership", 5, 120_000_000},
	}
	var doc strings.Builder
	doc.WriteString("schema_version: 1\nproducts:\n")
	for i, p := range plans {
		id := 5001 + i
		pda, err := fake.Plan(merchantKey.PublicKey(), uint64(id), solanafake.DevnetDUSDMint, uint64(p.amount), monthHours)
		require.NoError(t, err)
		fmt.Fprintf(&doc, "  %s-%s:\n    display_name: %s\n", p.key, sfx, p.key)
		if p.group != "" {
			fmt.Fprintf(&doc, "    tier_group: %s\n    tier_rank: %d\n", p.group, p.rank)
		}
		fmt.Fprintf(&doc, `    prices:
      %[1]s-%[2]s-monthly:
        currency: usd
        unit_amount: %[3]d
        billing_interval_hours: 720
        access_duration_hours: 720
        psps: [solana]
        psp_links:
          solana:
            plan_pda: %[4]s
            plan_id: "%[5]d"
    entitlements: ["%[1]s-%[2]s"]
`, p.key, sfx, p.amount, pda, id)
	}
	params, err := catalog.ParseApplicationYAML([]byte(doc.String()))
	require.NoError(t, err)
	_, err = w.client[embedded].ApplyCatalog(t.Context(), params, billing.ApplyCatalogParams{})
	require.NoError(t, err)
	price := func(key string) string { return priceID(t, w, key+"-"+sfx, key+"-"+sfx+"-monthly") }

	b := &solanaBuyer{customer: w.newCustomer(), wallet: solanago.NewWallet().PrivateKey}
	fake.Fund(b.wallet.PublicKey(), mint, 60_000_000)
	shop := &solanaShop{w: w, fake: fake, merchant: merchantKey, mint: mint, option: w.options(billing.CheckoutOptionListParams{ProductKey: "basic-" + sfx, PriceKey: "basic-" + sfx + "-monthly"})["solana"], price: price("basic"), key: "basic-" + sfx}
	c := shop.checkout(t, b, b.wallet.PublicKey())
	done, err := b.confirm(c, shop.land(t, signAs(t, c.bundle, b.wallet), w.clock.Now()))
	require.NoError(t, err)
	require.NotNil(t, done.SubscriptionID)
	sub := done.SubscriptionID.String()
	w.advance(time.Minute)
	require.True(t, b.entitled("basic-"+sfx))
	var firstPath string
	var firstRequest, firstResult map[string]any

	// The ungrouped product is refused on every route, and grants nothing.
	status, out := b.call(http.MethodPost, "/subscriptions/"+sub+"/change", "tc-"+uuid.NewString(), map[string]any{"price_id": price("vip")})
	require.Equal(t, http.StatusBadRequest, status, "%v", out)
	require.Contains(t, fmt.Sprint(out), "tier group")
	status, out = b.call(http.MethodPost, "/subscriptions/"+sub+"/change", "tc-"+uuid.NewString(), map[string]any{"price_id": price("vip"), "signature": solanago.Signature{}.String()})
	require.Equal(t, http.StatusBadRequest, status, "%v", out)
	require.False(t, b.entitled("vip-"+sfx))
	require.True(t, b.entitled("basic-"+sfx))

	for _, tc := range []struct {
		target, effective string
		pulls             int
		amount            int64
		payments          int
	}{
		{"plus", "now", 1, 55_000_000, 2},
		{"higher", "now", 1, 35_070_000, 3},
		{"lite", "period_end", 0, 0, 3},
		{"premium", "now", 1, 120_000_000, 4},
	} {
		preview := unwrap(b.must(http.MethodPost, "/subscriptions/"+sub+"/change/preview", "", map[string]any{"price_id": price(tc.target)}))
		prep := unwrap(b.must(http.MethodPost, "/subscriptions/"+sub+"/change", "tc-"+uuid.NewString(), map[string]any{"price_id": price(tc.target)}))
		require.Equal(t, prep["amount_due_now"], preview["amount_due_now"], "preview and preparation use the same paid-period credit")
		require.Equal(t, prep["effective"], preview["effective"])
		require.Equal(t, fmt.Sprint(tc.amount), prep["amount_due_now"])
		require.Equal(t, tc.effective, prep["effective"], "%s: direction comes from the price per hour", tc.target)
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
		landedAt := w.clock.Now()
		signature := shop.land(t, signAs(t, tx, b.wallet), landedAt)
		if tc.target == "plus" {
			require.Zero(t, fake.Balance(b.wallet.PublicKey(), mint), "confirmation must not prepare another charge against the now-empty wallet")
			w.advance(time.Hour)
		}
		path := "/subscriptions/" + sub + "/change"
		body := map[string]any{"price_id": price(tc.target), "signature": signature}
		// Race two confirmations, including the no-payment downgrade.
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		var confirmations [2]map[string]any
		var group errgroup.Group
		for i := range confirmations {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, w.server.URL+mountPrefix+"/v1/me"+path, bytes.NewReader(raw))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+b.token)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "tc-"+uuid.NewString())
			group.Go(func() error {
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					return err
				}
				defer res.Body.Close()
				if err := json.NewDecoder(res.Body).Decode(&confirmations[i]); err != nil {
					return err
				}
				if res.StatusCode != http.StatusOK {
					return fmt.Errorf("confirmation: %d %v", res.StatusCode, confirmations[i])
				}
				return nil
			})
		}
		require.NoError(t, group.Wait())
		confirmed := unwrap(confirmations[0])
		require.Equal(t, confirmed, unwrap(confirmations[1]))
		require.Equal(t, confirmed, unwrap(b.must(http.MethodPost, path, "tc-"+uuid.NewString(), body)))
		if firstPath == "" {
			firstPath, firstRequest, firstResult = path, body, confirmed
		}
		require.Equal(t, "succeeded", confirmed["status"])
		require.Equal(t, prep["amount_due_now"], confirmed["amount_due_now"], "confirmation preserves the agreed quote")
		sub = confirmed["subscription_id"].(string)
		payments := w.payments(embedded, b.id)
		require.Len(t, payments, tc.payments, "only signup and upgrades transfer money")
		if tc.pulls == 1 {
			require.Equal(t, prep["amount_due_now"], fmt.Sprint(payments[0].Amount), "record the quoted charge, not the plan's full price")
		}
		id, err := billing.ParseSubscriptionID(sub)
		require.NoError(t, err)
		membership := w.subscription(embedded, id)
		require.Equal(t, landedAt, *membership.CurrentPeriodStartsAt)
		require.Equal(t, membership.CurrentPeriodEndsAt.Format(time.RFC3339), confirmed["next_charge_date"])
		require.True(t, b.entitled(tc.target+"-"+sfx))
		if tc.target == "plus" {
			fake.Fund(b.wallet.PublicKey(), mint, 500_000_000)
		}
	}
	// The first successor has since been canceled; replay still returns its original terms.
	require.Equal(t, firstResult, unwrap(b.must(http.MethodPost, firstPath, "tc-"+uuid.NewString(), firstRequest)))
	for _, body := range []map[string]any{
		{"price_id": price("premium"), "signature": firstRequest["signature"]},
		{"price_id": firstRequest["price_id"], "signature": solanago.Signature{}.String()},
	} {
		status, out := b.call(http.MethodPost, firstPath, "tc-"+uuid.NewString(), body)
		require.Equal(t, http.StatusBadRequest, status, "%v", out)
	}
	other := w.newCustomer()
	status, out = other.call(http.MethodPost, firstPath, "tc-"+uuid.NewString(), firstRequest)
	require.Equal(t, http.StatusNotFound, status, "%v", out)
	require.Len(t, w.payments(embedded, b.id), 4)
}
