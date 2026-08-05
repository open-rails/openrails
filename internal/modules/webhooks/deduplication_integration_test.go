//go:build integration

package webhooks

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/replaycache"
	"github.com/stretchr/testify/require"
)

// or#893: the dedup service REQUIRES a database. Postgres webhook_events has
// been the dedup truth since #678, so a nil-DB construction was never a lighter
// harness — it was a shape the production graph cannot produce, and the
// `mark == nil` branches downstream existed to serve it. These lease/retry
// properties are about the Redis layer, but they run over the real pair now.
type dedupHarness struct {
	ctx  context.Context
	db   *db.DB
	svc  *DeduplicationService
	idem *replaycache.Store
}

func newDedupHarness(t *testing.T) *dedupHarness {
	t.Helper()
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	idem := replaycache.NewStore(nil)
	svc, err := NewDeduplicationService(idem, dbi)
	require.NoError(t, err)
	return &dedupHarness{ctx: dbtest.WithTestMerchant(context.Background()), db: dbi, svc: svc, idem: idem}
}

// eventID mints a run-unique id and removes its truth row afterwards, so one
// run's marks never dedup the next run's deliveries.
func (h *dedupHarness) eventID(t *testing.T, prefix string) string {
	t.Helper()
	id := prefix + "-" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = h.db.Pool().Exec(context.Background(),
			"DELETE FROM openrails.webhook_events WHERE event_id = $1", id)
	})
	return id
}

// A nil database is refused at construction, not tolerated at use.
func TestDeduplicationServiceRequiresPostgresTruth(t *testing.T) {
	svc, err := NewDeduplicationService(replaycache.NewStore(nil), nil)
	require.Error(t, err)
	require.Nil(t, svc)
	require.Contains(t, err.Error(), "database is required")
}

func TestProcessWebhook_RetryableErrorThenSuccess(t *testing.T) {
	h := newDedupHarness(t)
	evt := h.eventID(t, "tx-retryable")

	attempts := 0
	err := h.svc.ProcessWebhook(
		h.ctx,
		evt,
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
	require.Equal(t, 0, dedupMarkRows(t, h.ctx, h.db, "webhook.ccbill.RenewalSuccess", evt),
		"a retryable failure must NOT record the truth mark — the redelivery has to run")

	err = h.svc.ProcessWebhook(
		h.ctx,
		evt,
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
	require.Equal(t, 1, dedupMarkRows(t, h.ctx, h.db, "webhook.ccbill.RenewalSuccess", evt))

	rec, err := h.idem.Get(h.ctx, "webhook.ccbill.RenewalSuccess", evt)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, replaycache.StatusSuccess, rec.Status)
}

func TestProcessWebhook_NonRetryableErrorCompletesAndSkipsFutureRetries(t *testing.T) {
	h := newDedupHarness(t)
	evt := h.eventID(t, "tx-terminal")

	attempts := 0
	err := h.svc.ProcessWebhook(
		h.ctx,
		evt,
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

	err = h.svc.ProcessWebhook(
		h.ctx,
		evt,
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

	rec, err := h.idem.Get(h.ctx, "webhook.ccbill.RenewalSuccess", evt)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, replaycache.StatusSuccess, rec.Status)
}

func TestProcessWebhook_PendingDuplicateDoesNotProcessConcurrently(t *testing.T) {
	h := newDedupHarness(t)
	evt := h.eventID(t, "tx-concurrent")

	started := make(chan struct{})
	release := make(chan struct{})
	firstErr := make(chan error, 1)
	var attempts atomic.Int32

	go func() {
		firstErr <- h.svc.ProcessWebhook(
			h.ctx,
			evt,
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

	err := h.svc.ProcessWebhook(
		h.ctx,
		evt,
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

	rec, err := h.idem.Get(h.ctx, "webhook.ccbill.RenewalSuccess", evt)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, replaycache.StatusSuccess, rec.Status)
}

// A handler slower than the pending lease must stay exclusive: the heartbeat
// renews the lease, so a redelivery is rejected instead of taking over (#678).
func TestProcessWebhook_SlowHandlerKeepsLeaseViaHeartbeat(t *testing.T) {
	h := newDedupHarness(t)
	h.svc.pendingLease = 100 * time.Millisecond
	evt := h.eventID(t, "tx-slow")

	started := make(chan struct{})
	release := make(chan struct{})
	firstErr := make(chan error, 1)
	var attempts atomic.Int32

	go func() {
		firstErr <- h.svc.ProcessWebhook(
			h.ctx, evt, "RenewalSuccess", models.RailCCBill.EventSource(), nil,
			func(context.Context) error {
				attempts.Add(1)
				close(started)
				<-release
				return nil
			},
		)
	}()

	<-started
	time.Sleep(3 * h.svc.pendingLease) // well past the lease; heartbeat must have renewed it

	err := h.svc.ProcessWebhook(
		h.ctx, evt, "RenewalSuccess", models.RailCCBill.EventSource(), nil,
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
	h := newDedupHarness(t)
	h.svc.pendingLease = 50 * time.Millisecond
	evt := h.eventID(t, "tx-dead")

	// Simulate a crashed holder: claim pending, never heartbeat or complete.
	_, existed, err := h.idem.Begin(h.ctx, "webhook.ccbill.RenewalSuccess", evt)
	require.NoError(t, err)
	require.False(t, existed)

	time.Sleep(2 * h.svc.pendingLease)

	attempts := 0
	err = h.svc.ProcessWebhook(
		h.ctx, evt, "RenewalSuccess", models.RailCCBill.EventSource(), nil,
		func(context.Context) error {
			attempts++
			return nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, attempts, "stale pending claim should be taken over")

	rec, err := h.idem.Get(h.ctx, "webhook.ccbill.RenewalSuccess", evt)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, replaycache.StatusSuccess, rec.Status)
}

// mustDedupService is the wiring helper for tests that build a dispatcher.
func mustDedupService(t *testing.T, idem *replaycache.Store, dbi *db.DB) *DeduplicationService {
	t.Helper()
	svc, err := NewDeduplicationService(idem, dbi)
	require.NoError(t, err)
	return svc
}
