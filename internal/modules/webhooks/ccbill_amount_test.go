package webhooks

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

func TestValidateCCBillBilledAmount(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	require.NoError(t, validateCCBillBilledAmount(ctx, nil, 1000, 1000, nil, nil))
	require.NoError(t, validateCCBillBilledAmount(ctx, nil, 980, 1000, nil, nil))
	require.NoError(t, validateCCBillBilledAmount(ctx, nil, 1020, 1000, nil, nil))

	err := validateCCBillBilledAmount(ctx, nil, 979, 1000, map[string]interface{}{"subscription_id": "sub_123"}, nil)
	require.Error(t, err)

	var billingErr *BillingError
	require.True(t, errors.As(err, &billingErr))
	require.Equal(t, ErrorTypeAmount, billingErr.Type)
	require.Equal(t, int64(1000), billingErr.Context["expected_amount_cents"])
	require.Equal(t, int64(979), billingErr.Context["billed_amount_cents"])
	require.Equal(t, int64(20), billingErr.Context["tolerance_cents"])
	require.Equal(t, "sub_123", billingErr.Context["subscription_id"])
}

func TestValidateCCBillCurrencyMatches(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateCCBillCurrencyMatches("USD", "usd", nil))
	require.NoError(t, validateCCBillCurrencyMatches("eur", "", nil))

	err := validateCCBillCurrencyMatches("eur", "usd", map[string]interface{}{"subscription_id": "sub_123"})
	require.Error(t, err)
	var billingErr *BillingError
	require.True(t, errors.As(err, &billingErr))
	require.Equal(t, ErrorTypeValidation, billingErr.Type)
	require.Equal(t, "eur", billingErr.Context["billed_currency"])
	require.Equal(t, "usd", billingErr.Context["expected_currency"])
	require.Equal(t, "sub_123", billingErr.Context["subscription_id"])
}

func TestCCBillSuccessRequiresTransactionID(t *testing.T) {
	t.Parallel()

	svc := &CCBillWebhookService{}
	err := svc.handleNewSaleSuccessInternal(context.Background(), &CCBillNewSaleSuccessEvent{})
	require.Error(t, err)
	require.True(t, shouldTreatCCBillErrorAsNonRetryable(err))

	err = svc.handleRenewalSuccessInternal(context.Background(), &CCBillRenewalSuccessEvent{})
	require.Error(t, err)
	require.True(t, shouldTreatCCBillErrorAsNonRetryable(err))
}

func TestCapCCBillRetryAt(t *testing.T) {
	t.Parallel()

	paidTermEnd := time.Date(2026, 5, 1, 23, 59, 59, 0, time.UTC)
	withinCap := paidTermEnd.Add(subscriptions.DunningInterval - time.Second)
	capped := capCCBillRetryAt(&withinCap, &paidTermEnd)
	require.NotNil(t, capped)
	require.Equal(t, withinCap, *capped)

	distantRetry := paidTermEnd.Add(30 * 24 * time.Hour)
	capped = capCCBillRetryAt(&distantRetry, &paidTermEnd)
	require.NotNil(t, capped)
	require.Equal(t, paidTermEnd.Add(subscriptions.DunningInterval), *capped)
}
