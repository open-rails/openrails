package riverjobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/shared/opsmetric"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const KindCleanupExpiredData = "openrails.cleanup_expired_data"

const (
	// cleanupMerchantBatch caps one pass's fan-out. The work queue is indexed
	// on the work itself, so this bounds a pass by ACTIVITY — and the durable
	// cursor means the merchants beyond the cap are the NEXT pass's head, not
	// the tail of a queue that never gets there.
	cleanupMerchantBatch = 200

	// cleanupDeleteBatch bounds ONE delete statement, and so one transaction.
	cleanupDeleteBatch = 1000

	// cleanupMerchantRowBudget bounds one merchant's share of one pass across
	// all its sweeps. A merchant with years of unswept rows drains over
	// several hourly passes instead of monopolising this one.
	cleanupMerchantRowBudget = 50_000
)

// CleanupConfig defines retention periods for various data types
type CleanupConfig struct {
	// NotificationSeenRetention is how long to keep seen notifications
	// Default: 90 days (matches model's IsExpiredForCleanup)
	NotificationSeenRetention time.Duration

	// NotificationUnseenRetention is how long to keep unseen notifications
	// Default: 180 days (matches model's IsExpiredForCleanup)
	NotificationUnseenRetention time.Duration

	// WebhookEventRetention is how long completed webhook dedup marks
	// (openrails.webhook_events, #678) are kept. Default: 90 days — the same
	// window as the Redis completed-key cache TTL.
	WebhookEventRetention time.Duration

	// PaymentSettlementAckedRetention is how long acknowledged (delivered)
	// payment settlement events (#827) are kept. Default: 30 days. Pending
	// (unacked) events are never pruned.
	PaymentSettlementAckedRetention time.Duration

	// HostLifecycleEventAckedRetention is how long acknowledged host-lifecycle
	// events (or#878 delinquency transitions) are kept. Default: 30 days, the
	// same as the settlement feed it is modelled on. Pending (unacked) events
	// are NEVER pruned — an unread shutoff or restore instruction is not
	// garbage, it is undone work.
	HostLifecycleEventAckedRetention time.Duration
}

// DefaultCleanupConfig is the ONE defaults path (#711): worker registration
// passes it (or a test passes an explicit config); Work never re-defaults —
// a zero retention is a wiring bug and fails loudly instead of mass-deleting.
// clampInt32 narrows a row/batch budget for sqlc's int32 params; budgets are
// small positive constants, so the clamp exists for the checker and the
// impossible case, not for expected values.
func clampInt32(v int) int32 {
	if v < 0 {
		return 0
	}
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(v)
}

func DefaultCleanupConfig() CleanupConfig {
	return CleanupConfig{
		NotificationSeenRetention:   90 * 24 * time.Hour,
		NotificationUnseenRetention: 180 * 24 * time.Hour,
		WebhookEventRetention:       webhooks.WebhookIdempotencyTTL,

		PaymentSettlementAckedRetention:  30 * 24 * time.Hour,
		HostLifecycleEventAckedRetention: 30 * 24 * time.Hour,
	}
}

func (c CleanupConfig) validate() error {
	if c.NotificationSeenRetention <= 0 || c.NotificationUnseenRetention <= 0 || c.WebhookEventRetention <= 0 ||
		c.PaymentSettlementAckedRetention <= 0 || c.HostLifecycleEventAckedRetention <= 0 {
		return fmt.Errorf("cleanup worker config requires positive retentions (wire DefaultCleanupConfig()): %+v", c)
	}
	return nil
}

type CleanupExpiredDataArgs struct{}

func (CleanupExpiredDataArgs) Kind() string { return KindCleanupExpiredData }

type CleanupExpiredDataWorker struct {
	river.WorkerDefaults[CleanupExpiredDataArgs]
	DB     *db.DB
	Clock  clockwork.Clock
	Config CleanupConfig
	// MerchantBatch overrides cleanupMerchantBatch (0 = the default). It exists
	// so a test can force the capped-pass path without seeding hundreds of
	// merchants; registration never sets it.
	MerchantBatch int
}

func (CleanupExpiredDataWorker) Kind() string { return KindCleanupExpiredData }

// CleanupResult holds the count of deleted records per table
type CleanupResult struct {
	CheckoutSessionsExpired int64
	NotificationsSeen       int64
	NotificationsAll        int64
	WebhookEvents           int64
	PaymentSettlements      int64
	HostLifecycleEvents     int64
	// MerchantsBudgetCapped counts merchants whose backlog exceeded one pass's
	// row budget. Nonzero over many passes = the retention window is losing to
	// the write rate.
	MerchantsBudgetCapped int
}

// Work runs every retention sweep once per merchant WITH DUE WORK, inside that
// merchant's own scope.
//
// or#877 B4 fixed the scope bug: three of the five sweeps carried UNQUALIFIED
// predicates, which under the production openrails_app role match
// `merchant_id = NULL` — zero rows, no error, an hourly log line claiming
// success. All six now delete inside the merchant's scope with the merchant
// predicate ALSO written into the SQL, so the walk stays honest on a BYPASSRLS
// connection.
//
// or#837 fixes what that left: the walk itself. It enumerated EVERY active
// merchant every hour, `ORDER BY id`, no cursor and no LIMIT, then opened six
// transactions per merchant to discover — nearly always — nothing to delete. At
// thousands of merchants that is work scaling with the directory, not with
// activity. Now:
//
//   - merchants come from migration 0056's indexed work queue: only those
//     holding a row past one of THIS config's cutoffs, capped at
//     cleanupMerchantBatch;
//   - a durable cursor (openrails.worker_sweep_cursors) makes the next pass
//     resume after the last merchant handled, so a capped pass cannot re-serve
//     the same head forever and starve the tail. Draining the queue clears the
//     cursor and the ring starts over;
//   - each sweep deletes in bounded batches and loops, so a huge backlog is
//     many short transactions rather than one long one.
func (w CleanupExpiredDataWorker) Work(ctx context.Context, job *river.Job[CleanupExpiredDataArgs]) error {
	_, _, err := w.sweepPass(ctx)
	return err
}

// sweepPass is Work's body, returning what the pass actually did: the merchants
// it visited (the due-work list, in visit order) and the row counts. Work
// discards both; the scaling tests assert on them, because "only the merchants
// with due work were touched" is not observable from the deleted rows alone.
func (w CleanupExpiredDataWorker) sweepPass(ctx context.Context) ([]uuid.UUID, CleanupResult, error) {
	clock := w.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}

	config := w.Config
	if err := config.validate(); err != nil {
		return nil, CleanupResult{}, err
	}

	now := clock.Now()
	started := clock.Now()
	result := CleanupResult{}
	var cleanupErr error

	batch := w.MerchantBatch
	if batch <= 0 {
		batch = cleanupMerchantBatch
	}

	logger := log.WithContext(ctx).WithField("worker", KindCleanupExpiredData)

	directory := w.DB.GenDirectory()
	cursor, err := directory.GetSweepCursor(ctx, KindCleanupExpiredData)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, result, fmt.Errorf("cleanup expired data: load sweep cursor: %w", err)
	}

	dueWork := func(after *uuid.UUID, limit int32) ([]*uuid.UUID, error) {
		return directory.ListRetentionWorkMerchants(ctx, gen.ListRetentionWorkMerchantsParams{
			Now:                    now,
			NotificationCutoff:     now.Add(-config.NotificationUnseenRetention),
			NotificationSeenCutoff: now.Add(-config.NotificationSeenRetention),
			WebhookCutoff:          now.Add(-config.WebhookEventRetention),
			SettlementCutoff:       now.Add(-config.PaymentSettlementAckedRetention),
			LifecycleCutoff:        now.Add(-config.HostLifecycleEventAckedRetention),
			After:                  after,
			MerchantLimit:          limit,
		})
	}

	merchantIDs, err := dueWork(cursor, int32(batch))
	if err != nil {
		return nil, result, fmt.Errorf("cleanup expired data: list merchants with retention work: %w", err)
	}

	// The ring wraps INSIDE the pass. A short list means everything after the
	// cursor is drained, so the rest of this pass's capacity goes to the
	// merchants before it rather than being thrown away — otherwise the tail of
	// the ring costs a whole empty tick to notice. Ids at or below the cursor
	// are exactly the ones the first query could not have returned.
	if cursor != nil && len(merchantIDs) < batch {
		head, herr := dueWork(nil, clampInt32(batch-len(merchantIDs)))
		if herr != nil {
			return nil, result, fmt.Errorf("cleanup expired data: list merchants with retention work (ring wrap): %w", herr)
		}
		for _, mid := range head {
			if mid != nil && bytes.Compare(mid[:], cursor[:]) <= 0 {
				merchantIDs = append(merchantIDs, mid)
			}
		}
	}

	// A full batch means work was left behind: resume after the last merchant
	// handled. A short one means the whole ring is drained — clear the cursor so
	// the next pass starts fresh.
	var nextCursor *uuid.UUID
	if len(merchantIDs) == batch {
		nextCursor = merchantIDs[len(merchantIDs)-1]
	}

	visited := make([]uuid.UUID, 0, len(merchantIDs))
	for _, mid := range merchantIDs {
		if mid == nil {
			continue
		}
		merchantID := *mid
		visited = append(visited, merchantID)
		if err := w.DB.RunInMerchantScope(ctx, merchant.ID(merchantID), "cleanup expired data", func(mctx context.Context) error {
			w.sweepMerchant(mctx, merchantID, now, config, &result, &cleanupErr)
			return nil
		}); err != nil {
			// One merchant's failure must not abort the rest of the sweep.
			logger.WithError(err).WithField("merchant_id", merchantID).Error("Cleanup: merchant pass failed; continuing")
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}

	if serr := directory.SaveSweepCursor(ctx, gen.SaveSweepCursorParams{
		WorkerKind: KindCleanupExpiredData, CursorMerchantID: nextCursor,
	}); serr != nil {
		// A lost cursor costs fairness on the next pass, not correctness.
		logger.WithError(serr).Warn("Cleanup: could not persist sweep cursor")
	}

	opsmetric.Emit(ctx, opsmetric.MetricRetentionSweep, log.Fields{
		"worker":                    KindCleanupExpiredData,
		"merchants_with_work":       len(merchantIDs),
		"merchants_budget_capped":   result.MerchantsBudgetCapped,
		"resumed_from_cursor":       cursor != nil,
		"more_work_queued":          nextCursor != nil,
		"duration_ms":               clock.Now().Sub(started).Milliseconds(),
		"checkout_sessions_expired": result.CheckoutSessionsExpired,
		"notifications_seen":        result.NotificationsSeen,
		"notifications_unseen":      result.NotificationsAll,
		"webhook_events":            result.WebhookEvents,
		"payment_settlements":       result.PaymentSettlements,
		"host_lifecycle_events":     result.HostLifecycleEvents,
	})

	if cleanupErr != nil {
		return visited, result, fmt.Errorf("cleanup expired data: %w", cleanupErr)
	}
	return visited, result, nil
}

// sweepMerchant runs the six retention sweeps for ONE merchant, already inside
// its scope. Each BATCH gets its own transaction, so a failing sweep cannot roll
// back another's deletes and no single statement holds a transaction open across
// a large backlog (or#837, inherited from or#846).
func (w CleanupExpiredDataWorker) sweepMerchant(
	ctx context.Context, mid uuid.UUID, now time.Time, config CleanupConfig,
	result *CleanupResult, cleanupErr *error,
) {
	logger := log.WithContext(ctx).WithFields(log.Fields{
		"worker": KindCleanupExpiredData, "merchant_id": mid,
	})

	budget := cleanupMerchantRowBudget
	capped := false

	// sweep loops one retention statement in cleanupDeleteBatch-sized bites
	// until it returns a short batch (nothing left) or the merchant's row
	// budget for this pass runs out.
	sweep := func(name string, total *int64, fn func(ctx context.Context, q *gen.Queries, limit int32) (int64, error)) {
		for budget > 0 {
			limit := cleanupDeleteBatch
			if budget < limit {
				limit = budget
			}
			var n int64
			if err := w.DB.MerchantTx(ctx, func(tctx context.Context, tx pgx.Tx) error {
				var err error
				n, err = fn(tctx, gen.New(tx), clampInt32(limit))
				return err
			}); err != nil {
				logger.WithError(err).Error("Cleanup: " + name + " failed")
				*cleanupErr = errors.Join(*cleanupErr, fmt.Errorf("%s for merchant %s: %w", name, mid, err))
				return
			}
			*total += n
			budget -= int(n)
			if n < int64(limit) {
				return
			}
		}
		capped = true
	}

	// 1. Expire checkout sessions that have passed their TTL
	sweep("expire checkout sessions", &result.CheckoutSessionsExpired, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.ExpireCheckoutSessions(ctx, gen.ExpireCheckoutSessionsParams{MerchantID: mid, Now: now, RowLimit: limit})
	})

	// 2. Old notifications — seen ones first, with the shorter retention
	sweep("delete seen notifications", &result.NotificationsSeen, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteSeenNotificationsBefore(ctx, gen.DeleteSeenNotificationsBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-config.NotificationSeenRetention), RowLimit: limit,
		})
	})
	sweep("delete old notifications", &result.NotificationsAll, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteNotificationsBefore(ctx, gen.DeleteNotificationsBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-config.NotificationUnseenRetention), RowLimit: limit,
		})
	})

	// 3. Webhook dedup marks (#678)
	sweep("delete webhook events", &result.WebhookEvents, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteCompletedWebhookEventsBefore(ctx, gen.DeleteCompletedWebhookEventsBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-config.WebhookEventRetention), RowLimit: limit,
		})
	})

	// 4. Acked payment settlement events (#827). Pending events are never pruned.
	sweep("delete payment settlements", &result.PaymentSettlements, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteDeliveredPaymentSettlementsBefore(ctx, gen.DeleteDeliveredPaymentSettlementsBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-config.PaymentSettlementAckedRetention), RowLimit: limit,
		})
	})

	// 5. Acked host-lifecycle events (or#878). Same discipline as the feed above:
	// only DELIVERED rows are pruned.
	sweep("delete host lifecycle events", &result.HostLifecycleEvents, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteDeliveredHostLifecycleEventsBefore(ctx, gen.DeleteDeliveredHostLifecycleEventsBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-config.HostLifecycleEventAckedRetention), RowLimit: limit,
		})
	})

	if capped {
		result.MerchantsBudgetCapped++
		logger.WithField("row_budget", cleanupMerchantRowBudget).
			Warn("Cleanup: merchant hit this pass's row budget; the rest drains on the next pass")
	}
}
