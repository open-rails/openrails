package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const KindCleanupExpiredData = "openrails.cleanup_expired_data"

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
}

// DefaultCleanupConfig is the ONE defaults path (#711): worker registration
// passes it (or a test passes an explicit config); Work never re-defaults —
// a zero retention is a wiring bug and fails loudly instead of mass-deleting.
func DefaultCleanupConfig() CleanupConfig {
	return CleanupConfig{
		NotificationSeenRetention:   90 * 24 * time.Hour,
		NotificationUnseenRetention: 180 * 24 * time.Hour,
		WebhookEventRetention:       webhooks.WebhookIdempotencyTTL,

		PaymentSettlementAckedRetention: 30 * 24 * time.Hour,
	}
}

func (c CleanupConfig) validate() error {
	if c.NotificationSeenRetention <= 0 || c.NotificationUnseenRetention <= 0 || c.WebhookEventRetention <= 0 || c.PaymentSettlementAckedRetention <= 0 {
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
}

func (CleanupExpiredDataWorker) Kind() string { return KindCleanupExpiredData }

// CleanupResult holds the count of deleted records per table
type CleanupResult struct {
	CheckoutSessionsExpired int64
	NotificationsSeen       int64
	NotificationsAll        int64
	WebhookEvents           int64
	PaymentSettlements      int64
}

// Work runs every retention sweep once per merchant, inside that merchant's own
// scope (or#877 B4).
//
// It used to run all five on the job's bare context. Three of them
// (checkout-session expiry and both notification sweeps) carried UNQUALIFIED
// predicates, which under the production openrails_app role match
// `merchant_id = NULL`: zero rows, no error, an hourly log line claiming
// success. Retention had never run — notifications and expired checkout
// sessions accumulated without bound. The two that were already converted
// (webhook dedup marks, acked settlements) are the shape all five now share:
// enumerate ids from the policy-free merchant directory, then delete inside
// each merchant's scope, with the merchant predicate ALSO written into the SQL
// so the walk stays honest on a BYPASSRLS connection.
func (w CleanupExpiredDataWorker) Work(ctx context.Context, job *river.Job[CleanupExpiredDataArgs]) error {
	clock := w.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}

	config := w.Config
	if err := config.validate(); err != nil {
		return err
	}

	now := clock.Now()
	result := CleanupResult{}
	var cleanupErr error

	logger := log.WithContext(ctx).WithField("worker", KindCleanupExpiredData)
	logger.Info("Starting cleanup of expired data")

	merchantIDs, err := w.DB.GenDirectory().ListActiveMerchantIDs(ctx)
	if err != nil {
		return fmt.Errorf("cleanup expired data: list merchants: %w", err)
	}

	for _, mid := range merchantIDs {
		if err := w.DB.RunInMerchantScope(ctx, merchant.ID(mid), "cleanup expired data", func(mctx context.Context) error {
			w.sweepMerchant(mctx, mid, now, config, &result, &cleanupErr)
			return nil
		}); err != nil {
			// One merchant's failure must not abort the rest of the sweep.
			logger.WithError(err).WithField("merchant_id", mid).Error("Cleanup: merchant pass failed; continuing")
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}

	logger.WithFields(log.Fields{
		"merchants":                 len(merchantIDs),
		"checkout_sessions_expired": result.CheckoutSessionsExpired,
		"notifications_seen":        result.NotificationsSeen,
		"notifications_unseen":      result.NotificationsAll,
		"webhook_events":            result.WebhookEvents,
		"payment_settlements":       result.PaymentSettlements,
	}).Info("Cleanup completed")

	if cleanupErr != nil {
		return fmt.Errorf("cleanup expired data: %w", cleanupErr)
	}
	return nil
}

// sweepMerchant runs the five retention sweeps for ONE merchant, already inside
// its scope. Each sweep gets its own transaction so a failing one cannot roll
// back the others' deletes.
func (w CleanupExpiredDataWorker) sweepMerchant(
	ctx context.Context, mid uuid.UUID, now time.Time, config CleanupConfig,
	result *CleanupResult, cleanupErr *error,
) {
	logger := log.WithContext(ctx).WithFields(log.Fields{
		"worker": KindCleanupExpiredData, "merchant_id": mid,
	})
	sweep := func(name string, total *int64, fn func(ctx context.Context, q *gen.Queries) (int64, error)) {
		if err := w.DB.MerchantTx(ctx, func(tctx context.Context, tx pgx.Tx) error {
			n, err := fn(tctx, gen.New(tx))
			if err != nil {
				return err
			}
			*total += n
			return nil
		}); err != nil {
			logger.WithError(err).Error("Cleanup: " + name + " failed")
			*cleanupErr = errors.Join(*cleanupErr, fmt.Errorf("%s for merchant %s: %w", name, mid, err))
		}
	}

	// 1. Expire checkout sessions that have passed their TTL
	sweep("expire checkout sessions", &result.CheckoutSessionsExpired, func(ctx context.Context, q *gen.Queries) (int64, error) {
		return q.ExpireCheckoutSessions(ctx, gen.ExpireCheckoutSessionsParams{MerchantID: mid, Now: now})
	})

	// 2. Old notifications — seen ones first, with the shorter retention
	sweep("delete seen notifications", &result.NotificationsSeen, func(ctx context.Context, q *gen.Queries) (int64, error) {
		return q.DeleteSeenNotificationsBefore(ctx, gen.DeleteSeenNotificationsBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-config.NotificationSeenRetention),
		})
	})
	sweep("delete old notifications", &result.NotificationsAll, func(ctx context.Context, q *gen.Queries) (int64, error) {
		return q.DeleteNotificationsBefore(ctx, gen.DeleteNotificationsBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-config.NotificationUnseenRetention),
		})
	})

	// 3. Webhook dedup marks (#678)
	sweep("delete webhook events", &result.WebhookEvents, func(ctx context.Context, q *gen.Queries) (int64, error) {
		return q.DeleteCompletedWebhookEventsBefore(ctx, gen.DeleteCompletedWebhookEventsBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-config.WebhookEventRetention),
		})
	})

	// 4. Acked payment settlement events (#827). Pending events are never pruned.
	sweep("delete payment settlements", &result.PaymentSettlements, func(ctx context.Context, q *gen.Queries) (int64, error) {
		return q.DeleteDeliveredPaymentSettlementsBefore(ctx, gen.DeleteDeliveredPaymentSettlementsBeforeParams{
			MerchantID: mid, Cutoff: now.Add(-config.PaymentSettlementAckedRetention),
		})
	})
}
