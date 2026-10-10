//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/vaulttest"
)

// A customer's billing policy is its own assignment while the merchant's
// settings declare it, else its tier's, else the default. Invoice thresholds
// and delinquency grace follow that rung. An assignment the settings stop
// declaring (an edit outside OpenRails) falls back to the tier, then the
// default, and opens a finding; an edit through OpenRails refuses to remove a
// policy customers are assigned.
func TestBillingPolicyRungs(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	w.vault = vaulttest.New(t)
	floor := int64(1)
	policy := func(name string, threshold int64, grace int) billing.BillingPolicy {
		return billing.BillingPolicy{Name: name, Kind: "outstanding_cap", OutstandingCapAmount: 500_000_000,
			CollectionThresholdAmount: &threshold, DelinquencyGraceDays: &grace, DelinquencyAmountFloor: &floor}
	}
	w.settings = billing.MerchantSettings{
		BillingPolicies: []billing.BillingPolicy{
			policy("own", 10_000_000, 1), policy("tier", 20_000_000, 2), policy("fallback", 30_000_000, 3), policy("doomed", 5_000_000, 1),
		},
		BillingPolicyBindings: []billing.BillingPolicyBinding{{PolicyName: "tier", Tier: "gold"}, {PolicyName: "fallback"}},
	}
	w.start()
	ctx, client := t.Context(), w.client[remote]

	type payer struct {
		name string
		c    *customer
	}
	setup := func(name string, params billing.UpdateCustomerParams) payer {
		c := w.newCustomer()
		params.CreditLimits = []billing.CreditLimit{{Currency: "USD", Amount: 200_000_000}}
		params.InvoiceProfile = catalog.Value(billing.InvoiceProfile{CollectionMethod: billing.CollectSendInvoice, NetTermsDays: 1})
		_, err := client.UpdateCustomer(ctx, c.cid(), params)
		require.NoError(t, err, name)
		return payer{name, c}
	}
	own := setup("own", billing.UpdateCustomerParams{BillingPolicy: catalog.Value("own")})
	gold := setup("tier", billing.UpdateCustomerParams{TrustLevels: []billing.TrustLevel{{Currency: "USD", TrustLevel: "gold"}}})
	plain := setup("default", billing.UpdateCustomerParams{})
	dangling := setup("dangling", billing.UpdateCustomerParams{BillingPolicy: catalog.Value("doomed")})

	// An edit through OpenRails cannot drop a policy a customer holds.
	got, err := client.GetMerchantConfiguration(ctx)
	require.NoError(t, err)
	_, err = client.UpdateMerchantConfiguration(ctx, billing.UpdateMerchantConfigurationParams{ExpectedRevision: &got.Revision,
		Settings: &billing.MerchantSettings{BillingPolicies: []billing.BillingPolicy{policy("tier", 20_000_000, 2), policy("fallback", 30_000_000, 3), policy("doomed", 5_000_000, 1)}}})
	require.ErrorIs(t, err, billing.ErrInvalid, "the own policy is assigned")
	// An edit outside OpenRails can: its customers fall back and a finding opens.
	w.editDoc("merchant", func(doc map[string]any) {
		settings := doc["settings"].(map[string]any)
		var kept []any
		for _, p := range settings["billing_policies"].([]any) {
			if p.(map[string]any)["name"] != "doomed" {
				kept = append(kept, p)
			}
		}
		settings["billing_policies"] = kept
	})
	var findings int
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT count(*) FROM billing.reconciliation_findings WHERE finding_type = 'consistency.dangling_billing_policy' AND resolved_at IS NULL`)).Scan(&findings))
	require.Equal(t, 1, findings)

	invoiced := func(p payer) int {
		page, err := client.ListInvoices(ctx, billing.InvoiceListParams{CustomerID: p.c.cid()})
		require.NoError(t, err)
		return len(page.Items)
	}
	spend := func(amount int64, payers ...payer) {
		for _, p := range payers {
			_, err := recordUsage(ctx, client, billing.RecordUsageParams{CustomerID: p.c.cid(), Invoker: p.c.id, Currency: "USD", EventType: "rungs", Amount: amount, Source: "test", SourceID: uuid.NewString()})
			require.NoError(t, err, p.name)
		}
		w.advance(time.Minute)
		w.runPass(invoicePass{})
	}
	everyone := []payer{own, gold, plain, dangling}
	expect := func(want map[payer]int) {
		t.Helper()
		for _, p := range everyone {
			require.Equal(t, want[p], invoiced(p), "%s", p.name)
		}
	}
	spend(15_000_000, everyone...)
	expect(map[payer]int{own: 1})
	spend(10_000_000, gold, plain, dangling)
	expect(map[payer]int{own: 1, gold: 1})
	spend(10_000_000, plain, dangling)
	expect(map[payer]int{own: 1, gold: 1, plain: 1, dangling: 1})

	// Grace follows the same rung: a day for its own policy, two for the
	// tier's, three for the default's (the dangling assignment's too).
	due := func(p payer) time.Time {
		page, err := client.ListInvoices(ctx, billing.InvoiceListParams{CustomerID: p.c.cid()})
		require.NoError(t, err)
		require.NotNil(t, page.Items[0].DueAt)
		return *page.Items[0].DueAt
	}
	latest := due(own)
	for _, p := range everyone {
		if d := due(p); d.After(latest) {
			latest = d
		}
	}
	delinquent := func(p payer) bool {
		page, err := client.ListInvoices(ctx, billing.InvoiceListParams{CustomerID: p.c.cid()})
		require.NoError(t, err)
		return page.Items[0].Delinquent
	}
	w.advanceTo(latest.Add(36 * time.Hour))
	w.runPass(delinquencyPass{})
	require.True(t, delinquent(own), "past its own policy's day of grace")
	require.False(t, delinquent(gold), "inside the tier's two days")
	require.False(t, delinquent(plain), "inside the default's three days")
	require.False(t, delinquent(dangling), "the dangling assignment follows the default")
	w.advanceTo(latest.Add(60 * time.Hour))
	w.runPass(delinquencyPass{})
	require.True(t, delinquent(gold))
	require.False(t, delinquent(plain))
	require.False(t, delinquent(dangling))
	w.advanceTo(latest.Add(84 * time.Hour))
	w.runPass(delinquencyPass{})
	require.True(t, delinquent(plain))
	require.True(t, delinquent(dangling))

	status, body := w.staffJSON(http.MethodGet, "/v1/admin/findings", nil)
	require.Equal(t, http.StatusOK, status, "%v", body)
}
