package webhooks

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/replaycache"
	"github.com/stretchr/testify/require"
)

func TestProcessWebhook_RetryableErrorThenSuccess(t *testing.T) {
	ctx := context.Background()
	idem := replaycache.NewStore(nil)
	svc := NewDeduplicationService(idem, nil)

	attempts := 0
	err := svc.ProcessWebhook(
		ctx,
		"tx-retryable",
		"RenewalSuccess",
		models.RailCCBill.EventSource(),
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
		models.RailCCBill.EventSource(),
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
	require.Equal(t, replaycache.StatusSuccess, rec.Status)
}

func TestProcessWebhook_NonRetryableErrorCompletesAndSkipsFutureRetries(t *testing.T) {
	ctx := context.Background()
	idem := replaycache.NewStore(nil)
	svc := NewDeduplicationService(idem, nil)

	attempts := 0
	err := svc.ProcessWebhook(
		ctx,
		"tx-terminal",
		"RenewalSuccess",
		models.RailCCBill.EventSource(),
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
		models.RailCCBill.EventSource(),
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
	require.Equal(t, replaycache.StatusSuccess, rec.Status)
}

func TestProcessWebhook_PendingDuplicateDoesNotProcessConcurrently(t *testing.T) {
	ctx := context.Background()
	idem := replaycache.NewStore(nil)
	svc := NewDeduplicationService(idem, nil)

	started := make(chan struct{})
	release := make(chan struct{})
	firstErr := make(chan error, 1)
	var attempts atomic.Int32

	go func() {
		firstErr <- svc.ProcessWebhook(
			ctx,
			"tx-concurrent",
			"RenewalSuccess",
			models.RailCCBill.EventSource(),
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
		models.RailCCBill.EventSource(),
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
	require.Equal(t, replaycache.StatusSuccess, rec.Status)
}

// A handler slower than the pending lease must stay exclusive: the heartbeat
// renews the lease, so a redelivery is rejected instead of taking over (#678).
func TestProcessWebhook_SlowHandlerKeepsLeaseViaHeartbeat(t *testing.T) {
	ctx := context.Background()
	idem := replaycache.NewStore(nil)
	svc := NewDeduplicationService(idem, nil)
	svc.pendingLease = 100 * time.Millisecond

	started := make(chan struct{})
	release := make(chan struct{})
	firstErr := make(chan error, 1)
	var attempts atomic.Int32

	go func() {
		firstErr <- svc.ProcessWebhook(
			ctx, "tx-slow", "RenewalSuccess", models.RailCCBill.EventSource(), nil,
			func(context.Context) error {
				attempts.Add(1)
				close(started)
				<-release
				return nil
			},
		)
	}()

	<-started
	time.Sleep(3 * svc.pendingLease) // well past the lease; heartbeat must have renewed it

	err := svc.ProcessWebhook(
		ctx, "tx-slow", "RenewalSuccess", models.RailCCBill.EventSource(), nil,
		func(context.Context) error {
			attempts.Add(1)
			return nil
		},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "webhook already in progress")
	require.Equal(t, int32(1), attempts.Load(), "slow handler must stay exclusive")

	close(release)
	require.NoError(t, <-firstErr)
}

// A genuinely dead holder (pending record, no heartbeat) is still taken over
// after the lease ages out.
func TestProcessWebhook_DeadHolderIsTakenOver(t *testing.T) {
	ctx := context.Background()
	idem := replaycache.NewStore(nil)
	svc := NewDeduplicationService(idem, nil)
	svc.pendingLease = 50 * time.Millisecond

	// Simulate a crashed holder: claim pending, never heartbeat or complete.
	_, existed, err := idem.Begin(ctx, "webhook.ccbill.RenewalSuccess", "tx-dead")
	require.NoError(t, err)
	require.False(t, existed)

	time.Sleep(2 * svc.pendingLease)

	attempts := 0
	err = svc.ProcessWebhook(
		ctx, "tx-dead", "RenewalSuccess", models.RailCCBill.EventSource(), nil,
		func(context.Context) error {
			attempts++
			return nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, attempts, "stale pending claim should be taken over")

	rec, err := idem.Get(ctx, "webhook.ccbill.RenewalSuccess", "tx-dead")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, replaycache.StatusSuccess, rec.Status)
}
