package webhooks

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/stretchr/testify/require"
)

func TestProcessWebhook_RetryableErrorThenSuccess(t *testing.T) {
	ctx := context.Background()
	idem := idempotency.NewIdempotencyService(nil)
	svc := NewDeduplicationService(idem)

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
	require.Equal(t, idempotency.IdempotencyStatusSuccess, rec.Status)
}

func TestProcessWebhook_NonRetryableErrorCompletesAndSkipsFutureRetries(t *testing.T) {
	ctx := context.Background()
	idem := idempotency.NewIdempotencyService(nil)
	svc := NewDeduplicationService(idem)

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
	require.Equal(t, idempotency.IdempotencyStatusSuccess, rec.Status)
}

func TestProcessWebhook_PendingDuplicateDoesNotProcessConcurrently(t *testing.T) {
	ctx := context.Background()
	idem := idempotency.NewIdempotencyService(nil)
	svc := NewDeduplicationService(idem)

	started := make(chan struct{})
	release := make(chan struct{})
	firstErr := make(chan error, 1)
	var attempts atomic.Int32

	go func() {
		firstErr <- svc.ProcessWebhook(
			ctx,
			"tx-concurrent",
			"RenewalSuccess",
			models.ProcessorCCBill,
			map[string]any{"sample": "payload"},
			func(context.Context) error {
				attempts.Add(1)
				close(started)
				<-release
				return nil
			},
		)
	}()

	<-started

	err := svc.ProcessWebhook(
		ctx,
		"tx-concurrent",
		"RenewalSuccess",
		models.ProcessorCCBill,
		map[string]any{"sample": "payload"},
		func(context.Context) error {
			attempts.Add(1)
			return nil
		},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "webhook already in progress")
	require.Equal(t, int32(1), attempts.Load(), "pending duplicate should not run processing function")

	close(release)
	require.NoError(t, <-firstErr)
	require.Equal(t, int32(1), attempts.Load())

	rec, err := idem.Get(ctx, "webhook.ccbill.RenewalSuccess", "tx-concurrent")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, idempotency.IdempotencyStatusSuccess, rec.Status)
}

func TestIsDuplicate_DoesNotAutoCompletePendingClaim(t *testing.T) {
	ctx := context.Background()
	idem := idempotency.NewIdempotencyService(nil)
	svc := NewDeduplicationService(idem)

	isDupe, err := svc.IsDuplicate(ctx, "ccbill", "evt-1")
	require.NoError(t, err)
	require.False(t, isDupe)

	rec, err := idem.Get(ctx, "webhook.ccbill.event", "evt-1")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, idempotency.IdempotencyStatusPending, rec.Status)

	isDupe, err = svc.IsDuplicate(ctx, "ccbill", "evt-1")
	require.NoError(t, err)
	require.False(t, isDupe)

	require.NoError(t, idem.Complete(ctx, "webhook.ccbill.event", "evt-1", nil))
	isDupe, err = svc.IsDuplicate(ctx, "ccbill", "evt-1")
	require.NoError(t, err)
	require.True(t, isDupe)
}
