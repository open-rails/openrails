package money

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestUsageBillingDatesKeepOriginalAnchor(t *testing.T) {
	anchor := time.Date(2026, 1, 31, 8, 15, 0, 0, time.UTC)
	feb := NextUsageBillingDate(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), anchor)
	require.Equal(t, time.Date(2026, 2, 28, 8, 15, 0, 0, time.UTC), feb)
	require.Equal(t, time.Date(2026, 3, 31, 8, 15, 0, 0, time.UTC), NextUsageBillingDate(feb, anchor))
	leap := time.Date(2028, 2, 29, 8, 15, 0, 0, time.UTC)
	require.Equal(t, leap, NextUsageBillingDate(time.Date(2028, 2, 1, 0, 0, 0, 0, time.UTC), anchor))
	thirteenth := time.Date(2026, 1, 13, 0, 0, 0, 0, time.UTC)
	require.Equal(t, time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC), NextUsageBillingDate(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), thirteenth))
}

func TestUsageBillingZeroOverrideNeverInheritsCredit(t *testing.T) {
	defaults := &billing.UsageBillingCurrency{CreditLimit: 100, MinimumInvoiceAmount: 5}
	require.EqualValues(t, 100, usageCreditLimit(gen.BillingMoneySetting{}, defaults))
	require.Zero(t, usageCreditLimit(gen.BillingMoneySetting{CreditLimitOverride: true}, defaults))
	require.EqualValues(t, 40, usageCreditLimit(gen.BillingMoneySetting{CreditLimitOverride: true, CreditLimitAmount: 40}, defaults))
	zero := int64(0)
	require.Zero(t, usageMinimumInvoice(gen.BillingMoneySetting{UsageMinimumInvoiceAmount: &zero}, defaults))
	require.EqualValues(t, 5, usageMinimumInvoice(gen.BillingMoneySetting{}, defaults))
}

func TestUsageBillingDerivesTransactionConfiguration(t *testing.T) {
	service := &MoneyService{UsageBillingEnabled: true, EngineAdmissionHold: true}
	derived := service.WithDatabase(nil)
	require.True(t, derived.UsageBillingEnabled)
	require.True(t, derived.EngineAdmissionHold)
	derived.UsageBillingEnabled = false
	require.True(t, service.UsageBillingEnabled, "derived handles do not mutate another host")
}
