package services

import (
	"context"
	"errors"
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestProcessWebhook_RetryableErrorThenSuccess(t *testing.T) {
	ctx := context.Background()
	idem := NewIdempotencyService(nil)
	svc := NewDeduplicationService(idem, nil)

	attempts := 0
	err := svc.ProcessWebhook(
		ctx,
		"tx-retryable",
		"RenewalSuccess",
		models.ProcessorCCBill,
		map[string]any{"sample": "payload"},
		func(context.Context) error {
			attempts++
			if attempts == 1 {
				return errors.New("transient failure")
			}
			return nil
		},
	)
	require.Error(t, err)
	require.Equal(t, 1, attempts)

	err = svc.ProcessWebhook(
		ctx,
		"tx-retryable",
		"RenewalSuccess",
		models.ProcessorCCBill,
		map[string]any{"sample": "payload"},
		func(context.Context) error {
			attempts++
			return nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, 2, attempts)

	rec, err := idem.Get(ctx, "webhook.ccbill.RenewalSuccess", "tx-retryable")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, IdempotencyStatusSuccess, rec.Status)
}

func TestProcessWebhook_NonRetryableErrorCompletesAndSkipsFutureRetries(t *testing.T) {
	ctx := context.Background()
	idem := NewIdempotencyService(nil)
	svc := NewDeduplicationService(idem, nil)

	attempts := 0
	err := svc.ProcessWebhook(
		ctx,
		"tx-terminal",
		"RenewalSuccess",
		models.ProcessorCCBill,
		map[string]any{"sample": "payload"},
		func(context.Context) error {
			attempts++
			return MarkWebhookErrorNonRetryable(errors.New("invalid immutable payload"))
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, attempts)

	err = svc.ProcessWebhook(
		ctx,
		"tx-terminal",
		"RenewalSuccess",
		models.ProcessorCCBill,
		map[string]any{"sample": "payload"},
		func(context.Context) error {
			attempts++
			return nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, attempts, "second call should be skipped as already processed")

	rec, err := idem.Get(ctx, "webhook.ccbill.RenewalSuccess", "tx-terminal")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, IdempotencyStatusSuccess, rec.Status)
}

func TestIsDuplicate_DoesNotAutoCompletePendingClaim(t *testing.T) {
	ctx := context.Background()
	idem := NewIdempotencyService(nil)
	svc := NewDeduplicationService(idem, nil)

	isDupe, err := svc.IsDuplicate(ctx, "ccbill", "evt-1")
	require.NoError(t, err)
	require.False(t, isDupe)

	rec, err := idem.Get(ctx, "webhook.ccbill.event", "evt-1")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, IdempotencyStatusPending, rec.Status)

	isDupe, err = svc.IsDuplicate(ctx, "ccbill", "evt-1")
	require.NoError(t, err)
	require.False(t, isDupe)

	require.NoError(t, idem.Complete(ctx, "webhook.ccbill.event", "evt-1", nil))
	isDupe, err = svc.IsDuplicate(ctx, "ccbill", "evt-1")
	require.NoError(t, err)
	require.True(t, isDupe)
}
