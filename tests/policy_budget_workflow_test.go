//go:build integration

package tests

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/merchant"
)

func checkPolicyBudgetEffects(t *testing.T, f treasuryWorkflow) {
	ctx := merchant.WithID(t.Context(), f.merchant.MerchantID)
	ledger := money.NewMoneyService(dbtest.OpenMerchantDB(t, f.merchant.MerchantID.UUID()))
	payer := func() openrails.CustomerID {
		t.Helper()
		id, _ := f.actor(t, nil)
		require.NoError(t, f.client.SetCreditLimit(ctx, id, "USD", 100_000_000_000))
		account, err := f.client.Balance(ctx, id)
		require.NoError(t, err)
		require.Equal(t, "arrears", account.BillingMode)
		return id
	}
	check := func(who openrails.CustomerID, amount, rate int64, tier string) *openrails.AdmitResponse {
		t.Helper()
		deadline, key := time.Now().Add(time.Hour), uuid.NewString()
		result, err := f.client.Admit(ctx, openrails.AdmitRequest{CustomerID: who, Invoker: who.String(), InvokerType: openrails.InvokerTypePayer,
			Currency: "USD", EstimatedAmount: amount, AccrualRateDeltaPerHour: rate, TrustLevel: tier, RequestID: key, Source: "budget", ExpiresAt: &deadline})
		require.NoError(t, err)
		if result.Allowed {
			require.NoError(t, f.client.Release(ctx, key))
		}
		return result
	}
	report := func(who openrails.CustomerID, amount int64) {
		t.Helper()
		require.NoError(t, f.client.RecordUsage(ctx, openrails.UsageReport{CustomerID: who, Invoker: who.String(), Currency: "USD", EventType: "compute", Amount: amount, Source: "budget", SourceID: uuid.NewString()}))
	}
	previous, err := f.client.GetMerchantSettings(ctx)
	require.NoError(t, err)
	quota := openrails.MerchantSettings{BillingPolicies: []openrails.BillingPolicyInput{
		{Name: "budget_rate", Kind: "accrual_rate_cap", AccrualRateCapPerHour: 10_000_000, AccrualRateWindowSeconds: 3600},
	}, BillingPolicyBindings: []openrails.BillingPolicyBindingInput{{PolicyName: "budget_rate"}}}
	quota.BillingPolicies = append(previous.BillingPolicies, quota.BillingPolicies...)
	require.NoError(t, f.client.SetMerchantSettings(ctx, quota))
	busy, quiet, fresh := payer(), payer(), payer()
	report(busy, 9_000_000)
	report(quiet, 1_000_000)
	denied := check(busy, 1000, 2_000_000, "")
	require.False(t, denied.Allowed)
	require.Equal(t, admission.DenyAccrualRateCap, denied.DenyCode)
	require.True(t, check(quiet, 1000, 2_000_000, "").Allowed)
	require.True(t, check(busy, 1000, 0, "").Allowed, "zero prospective rate does not consume more capacity")
	require.Equal(t, admission.DenyAccrualRateCap, check(fresh, 1000, 11_000_000, "").DenyCode)
	quota.BillingPolicies = append(quota.BillingPolicies, openrails.BillingPolicyInput{Name: "budget_rate_large", Kind: "accrual_rate_cap", AccrualRateCapPerHour: 100_000_000})
	quota.BillingPolicyBindings = append(quota.BillingPolicyBindings, openrails.BillingPolicyBindingInput{PolicyName: "budget_rate_large", Tier: "large"})
	require.NoError(t, f.client.SetMerchantSettings(ctx, quota))
	require.True(t, check(busy, 1000, 2_000_000, "large").Allowed)
	require.False(t, check(busy, 1000, 2_000_000, "").Allowed, "tier override must not lift unrelated requests")

	debtPayer := payer()
	require.NoError(t, f.client.SetCreditLimit(ctx, debtPayer, "USD", 100_000_000))
	_, err = ledger.AccrueOwed(ctx, identity.CustomerID(debtPayer), "USD", "usage", uuid.NewString(), 500_000_000)
	require.NoError(t, err)
	require.True(t, check(debtPayer, 1000, 1_000_000, "large").Allowed, "prior debt does not gate a rate cap")
	window := openrails.MerchantSettings{BillingPolicies: []openrails.BillingPolicyInput{{Name: "budget_window", Kind: "window_spend_cap",
		SpendWindows: []openrails.BudgetWindowInput{{Key: "month", WindowSeconds: 30 * 24 * 3600, Limit: 2_000_000_000}}}},
		BillingPolicyBindings: []openrails.BillingPolicyBindingInput{{PolicyName: "budget_window"}}}
	window.BillingPolicies = append(previous.BillingPolicies, window.BillingPolicies...)
	require.NoError(t, f.client.SetMerchantSettings(ctx, window))
	cloud := payer()
	require.NoError(t, f.client.SetCreditLimit(ctx, cloud, "USD", 2_500_000_000))
	_, err = ledger.AccrueOwed(ctx, identity.CustomerID(cloud), "USD", "usage", uuid.NewString(), 3_000_000_000)
	require.NoError(t, err)
	require.True(t, check(cloud, 10_000_000, 0, "").Allowed, "prior debt is a separate delinquency signal, not new window spend")
	key := uuid.NewString()
	deadline := time.Now().Add(time.Hour)
	first, err := f.client.Admit(ctx, openrails.AdmitRequest{CustomerID: cloud, Invoker: cloud.String(), InvokerType: openrails.InvokerTypePayer,
		Currency: "USD", EstimatedAmount: 10_000_000, RequestID: key, Source: "budget", ExpiresAt: &deadline})
	require.NoError(t, err)
	require.True(t, first.Allowed)
	denied = check(cloud, 2_000_000_000, 0, "")
	require.False(t, denied.Allowed)
	require.Equal(t, admission.DenyBudgetExceeded, denied.DenyCode)
	require.NoError(t, f.client.Release(ctx, key))
}
