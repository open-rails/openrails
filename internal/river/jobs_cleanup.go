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
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/eventlog"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/orders"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/open-rails/openrails/internal/shared/opsmetric"
	"github.com/open-rails/openrails/internal/shared/progress"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const KindCleanupExpiredData = "openrails.cleanup_expired_data"

const (
	// cleanupMerchantBatch caps one pass's fan-out; the durable cursor makes
	// the merchants beyond the cap the next pass's head.
	cleanupMerchantBatch = 200

	// cleanupDeleteBatch bounds ONE delete statement, and so one transaction.
	cleanupDeleteBatch = 1000

	// cleanupIndexedMaxBatches bounds one pass's deletes of one indexed
	// table across merchants (event log, carts, idempotency keys, Solana Pay
	// references); a larger backlog drains over the following passes.
	cleanupIndexedMaxBatches = 50

	// cleanupMerchantRowBudget bounds one merchant's share of one pass across
	// all its sweeps. A merchant with years of unswept rows drains over
	// several hourly passes instead of monopolising this one.
	cleanupMerchantRowBudget = 50_000
)

// CleanupConfig defines retention periods for various data types
type CleanupConfig struct {
	// EventRetention bounds retained facts independently of reader progress.
	EventRetention time.Duration

	// NotificationSeenRetention is how long to keep seen notifications
	// Default: 90 days (matches model's IsExpiredForCleanup)
	NotificationSeenRetention time.Duration

	// NotificationUnseenRetention is how long to keep unseen notifications
	// Default: 180 days (matches model's IsExpiredForCleanup)
	NotificationUnseenRetention time.Duration

	// WebhookEventRetention is how long completed webhook dedup marks
	// (webhook_events) are kept. Default: 90 days.
	WebhookEventRetention time.Duration

	// PaymentAttemptRetention is how long payment attempts, rebill cycles and
	// NMI history months are kept. Default: 25 months.
	PaymentAttemptRetention time.Duration
}

// clampInt32 narrows a small positive row/batch budget to sqlc's int32
// params; the clamp exists for the checker, not for expected values.
func clampInt32(v int) int32 {
	if v < 0 {
		return 0
	}
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(v)
}

// DefaultCleanupConfig is the one defaults path: Work never re-defaults, so a
// zero retention fails loudly instead of mass-deleting.
func DefaultCleanupConfig() CleanupConfig {
	return CleanupConfig{
		EventRetention:              retention.EventLog,
		NotificationSeenRetention:   retention.NotificationsRead,
		NotificationUnseenRetention: retention.NotificationsUnread,
		WebhookEventRetention:       webhooks.WebhookEventRetention,

		PaymentAttemptRetention: retention.PaymentAttempts,
	}
}

func (c CleanupConfig) validate() error {
	if c.EventRetention <= 0 || c.NotificationSeenRetention <= 0 || c.NotificationUnseenRetention <= 0 || c.WebhookEventRetention <= 0 ||
		c.PaymentAttemptRetention <= 0 {
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
	// RowBudget overrides cleanupMerchantRowBudget (0 = the default), so a test
	// can show a backlog draining over several bounded passes without seeding
	// tens of thousands of rows; registration never sets it.
	RowBudget int
}

func (CleanupExpiredDataWorker) Kind() string { return KindCleanupExpiredData }

// CleanupResult holds the count of deleted records per table
type CleanupResult struct {
	EventLog int64

	UserActionReservations     int64
	NotificationEvents         int64
	Carts                      int64
	IdempotencyKeys            int64
	SolanaPayReferences        int64
	PeriodicLeases             int64
	WorkerStates               int64
	NotificationsSeen          int64
	NotificationsAll           int64
	WebhookEvents              int64
	HostOutbox                 int64
	PaymentAttempts            int64
	RebillCycles               int64
	NMIHistoryMonths           int64
	SubscriptionTransitions    int64
	SubscriptionBillingChanges int64
	OrderAttempts              int64
	UnpaidOrders               int64
	FailedUsageWindows         int64
	MeterPeriodQuantities      int64
	AdmissionDenials           int64
	WebhookHealthDays          int64
	ReconciliationFindings     int64
	ReconciliationRuns         int64
	ProviderIntents            int64
	ProviderMutationLogs       int64
	ReleasedParks              int64
	// PartitionsCreated and PartitionsDropped count whole monthly partitions.
	PartitionsCreated int
	PartitionsDropped int
	// MerchantsBudgetCapped counts merchants whose backlog exceeded one pass's
	// row budget. Nonzero over many passes = the retention window is losing to
	// the write rate.
	MerchantsBudgetCapped int
}

// Work is the one retention owner. It runs the deployment-wide sweeps (event
// log, notification journal, reservations, partitions, carts, idempotency
// keys, Solana Pay references, orphan leases and worker state), then every
// retention sweep once per merchant with due work, inside that merchant's
// scope and with the merchant predicate in the SQL:
//
//   - merchants come from an indexed work queue (only those holding a row
//     past one of this config's cutoffs), capped at cleanupMerchantBatch;
//   - a durable cursor (worker_state) resumes the next pass after the last
//     merchant handled, so a capped pass cannot starve the tail; draining the
//     queue clears it;
//   - each sweep deletes in bounded batches, so a huge backlog is many short
//     transactions rather than one long one.
func (w CleanupExpiredDataWorker) Work(ctx context.Context, job *river.Job[CleanupExpiredDataArgs]) error {
	_, _, err := w.sweepPass(ctx)
	return err
}

// Sweep runs one pass and reports what it deleted.
func (w CleanupExpiredDataWorker) Sweep(ctx context.Context) (CleanupResult, error) {
	_, result, err := w.sweepPass(ctx)
	return result, err
}

// sweepPass runs one pass and returns the merchants it visited, in order, and
// the row counts; tests assert that only merchants with due work were touched.
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

	for batch := 0; batch < cleanupIndexedMaxBatches; batch++ {
		n, err := eventlog.Prune(ctx, w.DB, now, config.EventRetention, cleanupDeleteBatch)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			break
		}
		result.EventLog += int64(n)
		if n < cleanupDeleteBatch {
			break
		}
	}

	for batch := 0; batch < cleanupIndexedMaxBatches; batch++ {
		n, err := w.DB.GenDirectory().PruneNotificationJournal(ctx, gen.PruneNotificationJournalParams{Cutoff: now.Add(-max(retention.EventLog, config.EventRetention)), RowLimit: cleanupDeleteBatch})
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			break
		}
		result.NotificationEvents += n
		if n < cleanupDeleteBatch {
			break
		}
		if batch == cleanupIndexedMaxBatches-1 {
			logger.Warn("Cleanup: notification event retention reached its pass budget")
		}
	}

	for range cleanupIndexedMaxBatches {
		n, err := destructive.PruneUserActionReservations(ctx, w.DB, cleanupDeleteBatch)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("prune user action reservations: %w", err))
			break
		}
		result.UserActionReservations += n
		if n < cleanupDeleteBatch {
			break
		}
	}

	directory := w.DB.GenDirectory()

	// Partitioned tables are pruned by the calendar: whole months are created
	// ahead and dropped behind, and no row is read.
	if n, err := retention.EnsurePartitions(ctx, directory, now); err != nil {
		logger.WithError(err).Error("Cleanup: ensure partitions failed")
		cleanupErr = errors.Join(cleanupErr, err)
	} else {
		result.PartitionsCreated = n
	}
	if n, err := retention.DropExpiredPartitions(ctx, directory, now); err != nil {
		logger.WithError(err).Error("Cleanup: drop expired partitions failed")
		cleanupErr = errors.Join(cleanupErr, err)
	} else {
		result.PartitionsDropped = n
	}

	// Indexed, bounded deletes across merchants.
	global := func(name string, total *int64, fn func(limit int32) (int64, error)) {
		for range cleanupIndexedMaxBatches {
			n, err := fn(cleanupDeleteBatch)
			if err != nil {
				logger.WithError(err).Error("Cleanup: " + name + " failed")
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("%s: %w", name, err))
				return
			}
			*total += n
			if n < cleanupDeleteBatch {
				return
			}
		}
	}
	global("delete carts", &result.Carts, func(limit int32) (int64, error) {
		return orders.DeleteExpiredCarts(ctx, w.DB, now, limit)
	})
	// Expiry is checked when a key is claimed; this only reclaims storage.
	global("delete expired idempotency keys", &result.IdempotencyKeys, func(limit int32) (int64, error) {
		return idempotency.DeleteExpired(ctx, w.DB, limit)
	})
	// Pending references and credited or review receipts are never selected.
	global("delete settled Solana Pay references", &result.SolanaPayReferences, func(limit int32) (int64, error) {
		return solanamodule.DeleteSettled(ctx, w.DB, now, limit)
	})
	global("delete orphan periodic leases", &result.PeriodicLeases, func(limit int32) (int64, error) {
		return directory.DeleteOrphanPeriodicLeases(ctx, gen.DeleteOrphanPeriodicLeasesParams{RetentionDays: retention.Days(retention.OrphanState), RowLimit: limit})
	})
	global("delete orphan worker state", &result.WorkerStates, func(limit int32) (int64, error) {
		return directory.DeleteOrphanWorkerState(ctx, gen.DeleteOrphanWorkerStateParams{RetentionDays: retention.Days(retention.OrphanState), RowLimit: limit})
	})

	cursorRow, err := loadSweepCursor(ctx, directory, KindCleanupExpiredData)
	if err != nil {
		return nil, result, fmt.Errorf("cleanup expired data: %w", err)
	}
	cursor := cursorRow.CursorMerchantID

	dueWork := func(after *uuid.UUID, limit int32) ([]uuid.UUID, error) {
		return directory.ListRetentionWorkMerchants(ctx, gen.ListRetentionWorkMerchantsParams{
			Now:                    now,
			NotificationCutoff:     now.Add(-config.NotificationUnseenRetention),
			NotificationSeenCutoff: now.Add(-config.NotificationSeenRetention),
			WebhookCutoff:          now.Add(-config.WebhookEventRetention),
			OutboxCutoff:           now.Add(-max(retention.EventLog, config.EventRetention)),
			AttemptCutoff:          now.Add(-config.PaymentAttemptRetention),
			TransitionCutoff:       now.Add(-retention.SubscriptionTransitions),
			OrderAttemptCutoff:     now.Add(-retention.ExpiredOrderAttempts),
			UnpaidOrderCutoff:      now.Add(-retention.UnpaidOrders),
			MeterCutoff:            now.Add(-retention.MeterPeriodQuantities),
			CounterCutoff:          now.Add(-retention.MetricCounters),
			FindingCutoff:          now.Add(-retention.ResolvedFindings),
			RunCutoff:              now.Add(-retention.ReconciliationRuns),
			ProviderWriteCutoff:    now.Add(-retention.ProviderWrites),
			ParkCutoff:             now.Add(-retention.ReleasedParks),
			After:                  after,
			MerchantLimit:          limit,
		})
	}

	merchantIDs, err := dueWork(cursor, int32(batch))
	if err != nil {
		return nil, result, fmt.Errorf("cleanup expired data: list merchants with retention work: %w", err)
	}

	// The ring wraps inside the pass: after a short list, the remaining
	// capacity goes to the merchants at or below the cursor (which the first
	// query could not return) instead of costing a whole empty tick.
	if cursor != nil && len(merchantIDs) < batch {
		head, herr := dueWork(nil, clampInt32(batch-len(merchantIDs)))
		if herr != nil {
			return nil, result, fmt.Errorf("cleanup expired data: list merchants with retention work (ring wrap): %w", herr)
		}
		for _, mid := range head {
			if bytes.Compare(mid[:], cursor[:]) <= 0 {
				merchantIDs = append(merchantIDs, mid)
			}
		}
	}

	// A full batch means work was left behind: resume after the last merchant
	// handled. A short one means the whole ring is drained — clear the cursor so
	// the next pass starts fresh.
	var nextCursor *uuid.UUID
	if len(merchantIDs) == batch {
		last := merchantIDs[len(merchantIDs)-1]
		nextCursor = &last
	}

	visited := make([]uuid.UUID, 0, len(merchantIDs))
	for _, merchantID := range merchantIDs {
		visited = append(visited, merchantID)
		progress.Mark(ctx, "cleanup merchant "+merchantID.String())
		if err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(merchantID), "cleanup expired data", func(mctx context.Context) error {
			w.sweepMerchant(mctx, merchantID, now, config, &result, &cleanupErr)
			return nil
		}); err != nil {
			// One merchant's failure must not abort the rest of the sweep.
			logger.WithError(err).WithField("merchant_id", merchantID).Error("Cleanup: merchant pass failed; continuing")
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}

	saveSweepCursor(ctx, directory, KindCleanupExpiredData, cursorRow, nextCursor, logger)

	opsmetric.Emit(ctx, opsmetric.MetricRetentionSweep, log.Fields{
		"worker":                       KindCleanupExpiredData,
		"merchants_with_work":          len(merchantIDs),
		"merchants_budget_capped":      result.MerchantsBudgetCapped,
		"resumed_from_cursor":          cursor != nil,
		"more_work_queued":             nextCursor != nil,
		"duration_ms":                  clock.Now().Sub(started).Milliseconds(),
		"carts":                        result.Carts,
		"idempotency_keys":             result.IdempotencyKeys,
		"solana_pay_references":        result.SolanaPayReferences,
		"periodic_leases":              result.PeriodicLeases,
		"worker_states":                result.WorkerStates,
		"notification_events":          result.NotificationEvents,
		"event_log":                    result.EventLog,
		"notifications_seen":           result.NotificationsSeen,
		"notifications_unseen":         result.NotificationsAll,
		"webhook_events":               result.WebhookEvents,
		"host_outbox":                  result.HostOutbox,
		"payment_attempts":             result.PaymentAttempts,
		"rebill_cycles":                result.RebillCycles,
		"nmi_history_months":           result.NMIHistoryMonths,
		"subscription_transitions":     result.SubscriptionTransitions,
		"subscription_billing_changes": result.SubscriptionBillingChanges,
		"order_attempts":               result.OrderAttempts,
		"unpaid_orders":                result.UnpaidOrders,
		"failed_usage_windows":         result.FailedUsageWindows,
		"meter_period_quantities":      result.MeterPeriodQuantities,
		"admission_denials":            result.AdmissionDenials,
		"webhook_health_days":          result.WebhookHealthDays,
		"reconciliation_findings":      result.ReconciliationFindings,
		"reconciliation_runs":          result.ReconciliationRuns,
		"provider_intents":             result.ProviderIntents,
		"provider_mutation_logs":       result.ProviderMutationLogs,
		"released_parks":               result.ReleasedParks,
		"partitions_created":           result.PartitionsCreated,
		"partitions_dropped":           result.PartitionsDropped,
	})

	if cleanupErr != nil {
		return visited, result, fmt.Errorf("cleanup expired data: %w", cleanupErr)
	}
	return visited, result, nil
}

// sweepMerchant runs the retention sweeps for one merchant, already inside
// its scope. Each batch gets its own transaction, so a failing sweep cannot
// roll back another's deletes and no transaction stays open across a large
// backlog.
func (w CleanupExpiredDataWorker) sweepMerchant(
	ctx context.Context, mid uuid.UUID, now time.Time, config CleanupConfig,
	result *CleanupResult, cleanupErr *error,
) {
	logger := log.WithContext(ctx).WithFields(log.Fields{
		"worker": KindCleanupExpiredData, "merchant_id": mid,
	})

	budget := cleanupMerchantRowBudget
	if w.RowBudget > 0 {
		budget = w.RowBudget
	}
	rowBudget := budget
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
			progress.Mark(ctx, name)
			if n < int64(limit) {
				return
			}
		}
		capped = true
	}

	// Old notifications: seen ones first, with the shorter retention.
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

	// Webhook dedup marks.
	sweep("delete webhook events", &result.WebhookEvents, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteCompletedWebhookEventsBefore(ctx, gen.DeleteCompletedWebhookEventsBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-config.WebhookEventRetention), RowLimit: limit,
		})
	})

	// The host outbox dedupe journal, retained from ingestion.
	sweep("delete host outbox", &result.HostOutbox, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteHostOutboxBefore(ctx, gen.DeleteHostOutboxBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-max(retention.EventLog, config.EventRetention)), RowLimit: limit,
		})
	})

	// Payment attempts and then the rebill cycles they leave empty.
	sweep("delete payment attempts", &result.PaymentAttempts, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeletePaymentAttemptsBefore(ctx, gen.DeletePaymentAttemptsBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-config.PaymentAttemptRetention), RowLimit: limit,
		})
	})
	sweep("delete rebill cycles", &result.RebillCycles, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteRebillCyclesBefore(ctx, gen.DeleteRebillCyclesBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-config.PaymentAttemptRetention), RowLimit: limit,
		})
	})
	// NMI history months, kept as long as attempts.
	sweep("delete nmi history months", &result.NMIHistoryMonths, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteNMIHistoryMonthsBefore(ctx, gen.DeleteNMIHistoryMonthsBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-config.PaymentAttemptRetention), RowLimit: limit,
		})
	})

	// guarded names the table for billing.guard_retention_delete: the one
	// path that deletes an append-only fact, and only past its period.
	guarded := func(table string, fn func(ctx context.Context, q *gen.Queries, limit int32) (int64, error)) func(context.Context, *gen.Queries, int32) (int64, error) {
		return func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
			if err := q.DeclareRetentionSweep(ctx, table); err != nil {
				return 0, err
			}
			return fn(ctx, q, limit)
		}
	}

	// Subscription status history.
	sweep("delete subscription transitions", &result.SubscriptionTransitions, guarded("subscription_status_transitions",
		func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
			return q.DeleteSubscriptionTransitionsPastRetention(ctx, gen.DeleteSubscriptionTransitionsPastRetentionParams{
				MerchantID: mid, RetentionDays: retention.Days(retention.SubscriptionTransitions), RowLimit: limit,
			})
		}))

	sweep("delete subscription billing changes", &result.SubscriptionBillingChanges, guarded("subscription_billing_changes",
		func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
			return q.DeleteSubscriptionBillingChangesPastRetention(ctx, gen.DeleteSubscriptionBillingChangesPastRetentionParams{
				MerchantID: mid, RetentionDays: retention.Days(retention.SubscriptionTransitions), RowLimit: limit,
			})
		}))

	// Order attempts that expired without reaching a provider, then the
	// unpaid closed orders that never started one.
	sweep("delete abandoned order attempts", &result.OrderAttempts, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteAbandonedOrderAttemptsBefore(ctx, gen.DeleteAbandonedOrderAttemptsBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-retention.ExpiredOrderAttempts), RowLimit: limit,
		})
	})
	sweep("delete unpaid orders", &result.UnpaidOrders, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteClosedUnpaidOrders(ctx, gen.DeleteClosedUnpaidOrdersParams{
			MerchantID: mid, Before: now.Add(-retention.UnpaidOrders), RowLimit: limit,
		})
	})

	// Usage counters past the windows and periods that read them.
	sweep("delete ended failed usage windows", &result.FailedUsageWindows, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteEndedFailedUsageWindows(ctx, gen.DeleteEndedFailedUsageWindowsParams{MerchantID: mid, Now: now, RowLimit: limit})
	})
	sweep("delete meter period quantities", &result.MeterPeriodQuantities, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteMeterPeriodQuantitiesBefore(ctx, gen.DeleteMeterPeriodQuantitiesBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-retention.MeterPeriodQuantities), RowLimit: limit,
		})
	})

	// Metric counters.
	sweep("delete admission denials", &result.AdmissionDenials, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteAdmissionDenialsBefore(ctx, gen.DeleteAdmissionDenialsBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-retention.MetricCounters), RowLimit: limit,
		})
	})
	sweep("delete webhook health days", &result.WebhookHealthDays, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteWebhookHealthDaysBefore(ctx, gen.DeleteWebhookHealthDaysBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-retention.MetricCounters), RowLimit: limit,
		})
	})

	// Resolved findings, then the reconciliation runs nothing names any more.
	sweep("delete resolved findings", &result.ReconciliationFindings, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteResolvedFindingsBefore(ctx, gen.DeleteResolvedFindingsBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-retention.ResolvedFindings), RowLimit: limit,
		})
	})
	sweep("delete reconciliation runs", &result.ReconciliationRuns, guarded("maintenance_runs",
		func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
			return q.DeleteReconciliationRunsPastRetention(ctx, gen.DeleteReconciliationRunsPastRetentionParams{
				MerchantID: mid, RetentionDays: retention.Days(retention.ReconciliationRuns), RowLimit: limit,
			})
		}))

	// Finished outbox intents and the mutation log. Intents that are the
	// record of money, a membership or an erasure are not outbox types.
	sweep("delete finished outbox intents", &result.ProviderIntents, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteFinishedOutboxIntentsBefore(ctx, gen.DeleteFinishedOutboxIntentsBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-retention.ProviderWrites), RowLimit: limit,
		})
	})
	sweep("delete provider mutation logs", &result.ProviderMutationLogs, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteProviderMutationLogsBefore(ctx, gen.DeleteProviderMutationLogsBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-retention.ProviderWrites), RowLimit: limit,
		})
	})

	// 13. Released parks.
	sweep("delete released parks", &result.ReleasedParks, func(ctx context.Context, q *gen.Queries, limit int32) (int64, error) {
		return q.DeleteReleasedParksBefore(ctx, gen.DeleteReleasedParksBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-retention.ReleasedParks), RowLimit: limit,
		})
	})

	if capped {
		result.MerchantsBudgetCapped++
		logger.WithField("row_budget", rowBudget).
			Warn("Cleanup: merchant hit this pass's row budget; the rest drains on the next pass")
	}
}
