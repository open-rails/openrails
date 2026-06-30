package webhooks

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestValidateCCBillBilledAmount(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	require.NoError(t, validateCCBillBilledAmount(ctx, nil, 1000, 10_000_000, nil, nil))
	require.NoError(t, validateCCBillBilledAmount(ctx, nil, 980, 10_000_000, nil, nil))
	require.NoError(t, validateCCBillBilledAmount(ctx, nil, 1020, 10_000_000, nil, nil))

	err := validateCCBillBilledAmount(ctx, nil, 979, 10_000_000, map[string]interface{}{"subscription_id": "sub_123"}, nil)
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

func TestCCBillInitialChargeAmountUsesIntro(t *testing.T) {
	t.Parallel()

	initial := int64(19_950_000)
	trialHours := 30 * 24
	price := &models.Price{Amount: 14_950_000, AutoRenew: true, TrialUnitAmount: &initial, TrialDurationHours: &trialHours}
	require.Equal(t, int64(19_950_000), ccbillInitialChargeAmount(price))
}

func TestCCBillPriceLookupIDPrefersRBO(t *testing.T) {
	t.Parallel()

	require.Equal(t, "0000007498", ccbillPriceLookupID(" 0000007498 ", "flex-123"))
	require.Equal(t, "flex-123", ccbillPriceLookupID("", " flex-123 "))
}

func TestParseCCBillAmountCentsAllowsZeroForTrial(t *testing.T) {
	t.Parallel()

	got, err := parseCCBillAmountCents("0.00", "billedInitialPrice", "billedAmount", true)
	require.NoError(t, err)
	require.Zero(t, got)

	_, err = parseCCBillAmountCents("0.00", "billedInitialPrice", "billedAmount", false)
	require.Error(t, err)
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
	withinCap := paidTermEnd.Add(ccbillGraceCap - time.Second)
	capped := capCCBillRetryAt(&withinCap, &paidTermEnd)
	require.NotNil(t, capped)
	require.Equal(t, withinCap, *capped)

	distantRetry := paidTermEnd.Add(30 * 24 * time.Hour)
	capped = capCCBillRetryAt(&distantRetry, &paidTermEnd)
	require.NotNil(t, capped)
	require.Equal(t, paidTermEnd.Add(ccbillGraceCap), *capped)
}

func TestShouldIgnoreCCBillRenewalFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 1, 23, 59, 59, 0, time.UTC)
	cancelled := &models.Subscription{Status: models.StatusCancelled}
	ignored, reason := shouldIgnoreCCBillRenewalFailure(cancelled, nil)
	require.True(t, ignored)
	require.Equal(t, "cancelled_subscription", reason)

	active := &models.Subscription{Status: models.StatusActive, CurrentPeriodStartsAt: &now}
	failureAt := now
	ignored, reason = shouldIgnoreCCBillRenewalFailure(active, &failureAt)
	require.True(t, ignored)
	require.Equal(t, "stale_renewal_failure", reason)

	futureFailure := now.Add(time.Hour)
	ignored, reason = shouldIgnoreCCBillRenewalFailure(active, &futureFailure)
	require.False(t, ignored)
	require.Empty(t, reason)
}

func TestParseCCBillDateUsingTimestamp_EndOfDayUTC(t *testing.T) {
	t.Parallel()

	got, err := parseCCBillDateUsingTimestamp("2026-03-15")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, time.Date(2026, 3, 15, 23, 59, 59, 0, time.UTC), *got)
}

func TestParseCCBillDateUsingTimestamp_EmptyDateReturnsNil(t *testing.T) {
	t.Parallel()

	got, err := parseCCBillDateUsingTimestamp("")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestParseCCBillDateUsingTimestamp_InvalidDateFails(t *testing.T) {
	t.Parallel()

	got, err := parseCCBillDateUsingTimestamp("2026-31-99")
	require.Error(t, err)
	require.Nil(t, got)
}
