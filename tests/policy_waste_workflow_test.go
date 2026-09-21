//go:build integration

package tests

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/fx"
)

func checkPolicyWasteAndProfiles(t *testing.T, f treasuryWorkflow) {
	ctx := t.Context()
	f.surface.App().Runtime.FXProvider = fx.NewMockProvider(map[string]float64{"eur": 2})
	fund := func(c *openrails.Client) openrails.CustomerID {
		t.Helper()
		payer := openrails.CustomerID(uuid.New())
		_, err := c.DepositCredits(ctx, openrails.DepositCreditsRequest{CustomerID: &payer, Invoker: payer.String(), Currency: "USD", Amount: 100_000_000, Source: "waste", SourceID: uuid.NewString()})
		require.NoError(t, err)
		return payer
	}
	grant := func(c *openrails.Client, payer openrails.CustomerID, invoker string) {
		t.Helper()
		require.NoError(t, c.SetCustomerSpendDelegation(ctx, payer, openrails.SpendDelegationInput{Scope: "invoker", ScopeKey: invoker,
			Windows: []openrails.SpendLimitWindow{{Key: "delegated", WindowSeconds: 3600, Limit: 100_000_000, Currency: "USD"}}}))
	}
	admit := func(c *openrails.Client, payer openrails.CustomerID, invoker string) *openrails.AdmitResponse {
		t.Helper()
		deadline, key := time.Now().Add(time.Hour), uuid.NewString()
		result, err := c.Admit(ctx, openrails.AdmitRequest{CustomerID: payer, Invoker: invoker, InvokerType: openrails.InvokerTypeDelegated, TrustLevel: "free",
			Currency: "USD", EstimatedAmount: 100, RequestID: key, Source: "waste", ExpiresAt: &deadline})
		require.NoError(t, err)
		if result.Allowed {
			require.NoError(t, c.Release(ctx, key))
		}
		return result
	}
	peer := f.surface.ProvisionOwnedMerchant("policy-peer-" + uuid.NewString()[:8])
	other := f.surface.Client(openrails.WithAPIKey(peer.APIKey), openrails.WithMerchantID(peer.MerchantID))
	for _, emptyDocument := range []bool{false, true} {
		if emptyDocument {
			require.NoError(t, other.SetMerchantSettings(ctx, openrails.MerchantSettings{}))
		}
		payer, invoker := fund(other), "default-"+uuid.NewString()
		grant(other, payer, invoker)
		_, err := other.ReportWastedSpend(ctx, openrails.WastedSpendReport{CustomerID: payer, Invoker: invoker, Currency: "USD", Amount: 2_000_000, Source: "waste", SourceID: uuid.NewString()})
		require.NoError(t, err)
		require.True(t, admit(other, payer, invoker).Allowed, "both absent and empty settings keep the $5 default cutoff")
	}
	alpha := openrails.MerchantProfileInput{DisplayName: "Merchant Alpha", LogoURL: "https://alpha.example/logo.png"}
	beta := openrails.MerchantProfileInput{DisplayName: "Merchant Beta", LogoURL: "https://beta.example/logo.png"}
	profileDocument, err := f.client.GetMerchantSettings(ctx)
	require.NoError(t, err)
	profileDocument.Profile = &alpha
	require.NoError(t, f.client.SetMerchantSettings(ctx, *profileDocument))
	require.NoError(t, other.SetMerchantSettings(ctx, openrails.MerchantSettings{Profile: &beta}))
	own, err := f.client.GetMerchantSettings(ctx)
	require.NoError(t, err)
	foreign, err := other.GetMerchantSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, &alpha, own.Profile)
	require.Equal(t, &beta, foreign.Profile)
	require.Empty(t, foreign.BillingPolicies)
	require.Empty(t, foreign.BillingPolicyBindings)
	windows := []openrails.BudgetWindowInput{{Key: "burst", WindowSeconds: 900, Limit: 1_000_000, Currency: "USD"}}
	windowDocument := *own
	windowDocument.DelegatedInvokerWastedSpendLimits = windows
	require.NoError(t, f.client.SetMerchantSettings(ctx, windowDocument))
	own, err = f.client.GetMerchantSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, &alpha, own.Profile, "the full settings document retains its configured profile")
	require.Equal(t, windows, own.DelegatedInvokerWastedSpendLimits)
	foreign, err = other.GetMerchantSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, &beta, foreign.Profile)
	require.Empty(t, foreign.DelegatedInvokerWastedSpendLimits, "another merchant must not inherit these limits")

	payerA, payerB := fund(f.client), fund(f.client)
	invoker := "shared-invoker-" + uuid.NewString()
	grant(f.client, payerA, invoker)
	grant(f.client, payerB, invoker)
	converted, err := f.client.ReportWastedSpend(ctx, openrails.WastedSpendReport{CustomerID: payerA, Invoker: invoker, Currency: "EUR", Amount: 600_000, Source: "waste", SourceID: uuid.NewString()})
	require.NoError(t, err)
	require.Equal(t, "USD", converted.PolicyCurrency)
	require.EqualValues(t, 1_200_000, converted.PolicyRecordedAmount)
	blocked := admit(f.client, payerA, invoker)
	require.False(t, blocked.Allowed)
	require.Equal(t, "abuse", blocked.BlockedBy)
	require.True(t, admit(f.client, payerB, invoker).Allowed, "same invoker label under a different payer has independent usage")
	_, err = f.client.ReportWastedSpend(ctx, openrails.WastedSpendReport{CustomerID: payerB, Invoker: invoker, Currency: "USD", Amount: 2_000_000, Source: "waste", SourceID: uuid.NewString()})
	require.NoError(t, err)
	require.False(t, admit(f.client, payerB, invoker).Allowed)

	own, err = f.client.GetMerchantSettings(ctx)
	require.NoError(t, err)
	own.BillingPolicies = append(own.BillingPolicies, openrails.BillingPolicyInput{Name: "direct_grace", Kind: "outstanding_cap",
		BadSpendWindows: []openrails.BudgetWindowInput{{Key: "burst", WindowSeconds: 900, Limit: 1_000_000}}})
	own.BillingPolicyBindings = append(own.BillingPolicyBindings, openrails.BillingPolicyBindingInput{PolicyName: "direct_grace", Tier: "free"})
	require.NoError(t, f.client.SetMerchantSettings(ctx, *own))
	direct := fund(f.client)
	under := openrails.WastedSpendReport{CustomerID: direct, Invoker: direct.String(), InvokerType: openrails.InvokerTypePayer, Currency: "USD", Amount: 500_000, Source: "waste", SourceID: uuid.NewString()}
	result, err := f.client.ReportWastedSpend(ctx, under)
	require.NoError(t, err)
	require.Equal(t, "USD", result.Currency)
	require.EqualValues(t, 500_000, result.ForgivenAmount)
	require.Zero(t, result.ChargedAmount)
	over := under
	over.Amount = 1_500_000
	over.SourceID = uuid.NewString()
	result, err = f.client.ReportWastedSpend(ctx, over)
	require.NoError(t, err)
	require.EqualValues(t, 500_000, result.ForgivenAmount)
	require.EqualValues(t, 1_000_000, result.ChargedAmount)
	_, err = f.client.ReportWastedSpend(ctx, over)
	require.NoError(t, err)
	balance, err := f.client.Balance(ctx, direct)
	require.NoError(t, err)
	require.EqualValues(t, 99_000_000, balance.BalanceAmount, "only overage is debited, exactly once across retry")
	pool := dbtest.OpenMerchantDB(t, f.merchant.MerchantID.UUID()).Pool()
	for _, row := range []struct {
		sourceID string
		amount   int64
	}{{under.SourceID, 0}, {over.SourceID, 1_000_000}} {
		var amount int64
		require.NoError(t, pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount),0)::bigint FROM billing.usage_events
WHERE customer_id=$1 AND event_type='wasted_spend' AND source='waste' AND source_id=$2`, direct.UUID(), row.sourceID).Scan(&amount))
		require.Equal(t, row.amount, amount, "usage records the chargeable portion of each waste report")
	}
	otherCurrency := over
	otherCurrency.Currency, otherCurrency.Amount = "EUR", 1
	result, err = f.client.ReportWastedSpend(ctx, otherCurrency)
	require.NoError(t, err)
	require.False(t, result.Duplicate, "the same source ID in another currency is a distinct report")
	require.Equal(t, "EUR", result.Currency)
	require.EqualValues(t, 1, result.ForgivenAmount)
	grant(f.client, direct, direct.String())
	require.True(t, admit(f.client, direct, direct.String()).Allowed, "direct-payer waste must not contaminate delegated cutoff counters")
}
