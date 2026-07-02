package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"time"

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
}

// DefaultCleanupConfig returns sensible default retention periods
func DefaultCleanupConfig() CleanupConfig {
	return CleanupConfig{
		NotificationSeenRetention:   90 * 24 * time.Hour,
		NotificationUnseenRetention: 180 * 24 * time.Hour,
		WebhookEventRetention:       webhooks.WebhookIdempotencyTTL,
	}
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
}

func (w CleanupExpiredDataWorker) Work(ctx context.Context, job *river.Job[CleanupExpiredDataArgs]) error {
	clock := w.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}

	config := w.Config
	if config == (CleanupConfig{}) {
		config = DefaultCleanupConfig()
	}

	now := clock.Now()
	result := CleanupResult{}
	var cleanupErr error

	logger := log.WithContext(ctx).WithField("worker", KindCleanupExpiredData)
	logger.Info("Starting cleanup of expired data")

	// 1. Expire checkout sessions that have passed their TTL
	var err error
	result.CheckoutSessionsExpired, err = w.expireCheckoutSessions(ctx, now)
	if err != nil {
		logger.WithError(err).Error("Failed to expire checkout sessions")
		cleanupErr = errors.Join(cleanupErr, err)
	}

	// 2. Clean up old notifications (seen ones first with shorter retention)
	result.NotificationsSeen, err = w.cleanupSeenNotifications(ctx, now, config.NotificationSeenRetention)
	if err != nil {
		logger.WithError(err).Error("Failed to cleanup seen notifications")
		cleanupErr = errors.Join(cleanupErr, err)
	}

	result.NotificationsAll, err = w.cleanupOldNotifications(ctx, now, config.NotificationUnseenRetention)
	if err != nil {
		logger.WithError(err).Error("Failed to cleanup old notifications")
		cleanupErr = errors.Join(cleanupErr, err)
	}

	// 3. Retention for webhook dedup marks (#678)
	result.WebhookEvents, err = w.cleanupWebhookEvents(ctx, now, config.WebhookEventRetention)
	if err != nil {
		logger.WithError(err).Error("Failed to cleanup webhook dedup marks")
		cleanupErr = errors.Join(cleanupErr, err)
	}

	logger.WithFields(log.Fields{
		"checkout_sessions_expired": result.CheckoutSessionsExpired,
		"notifications_seen":        result.NotificationsSeen,
		"notifications_unseen":      result.NotificationsAll,
		"webhook_events":            result.WebhookEvents,
	}).Info("Cleanup completed")

	if cleanupErr != nil {
		return fmt.Errorf("cleanup expired data: %w", cleanupErr)
	}
	return nil
}

func (w CleanupExpiredDataWorker) expireCheckoutSessions(ctx context.Context, now time.Time) (int64, error) {
	rows, err := w.DB.Gen(ctx).ExpireCheckoutSessions(ctx, now)
	if err != nil {
		return 0, fmt.Errorf("expire checkout sessions: %w", err)
	}
	return rows, nil
}

// cleanupSeenNotifications deletes seen notifications older than the retention period
func (w CleanupExpiredDataWorker) cleanupSeenNotifications(ctx context.Context, now time.Time, retention time.Duration) (int64, error) {
	cutoff := now.Add(-retention)

	rows, err := w.DB.Gen(ctx).DeleteSeenNotificationsBefore(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete seen notifications: %w", err)
	}
	return rows, nil
}

// cleanupOldNotifications deletes all notifications (including unseen) older than the retention period
func (w CleanupExpiredDataWorker) cleanupOldNotifications(ctx context.Context, now time.Time, retention time.Duration) (int64, error) {
	cutoff := now.Add(-retention)

	rows, err := w.DB.Gen(ctx).DeleteNotificationsBefore(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete old notifications: %w", err)
	}
	return rows, nil
}

// cleanupWebhookEvents deletes completed webhook dedup marks older than the
// retention window (#678). webhook_events is RLS-scoped, so this walks the
// control-plane merchant directory and deletes per merchant under MerchantTx
// (same pattern as the converge sweep). One merchant's failure doesn't abort
// the rest.
func (w CleanupExpiredDataWorker) cleanupWebhookEvents(ctx context.Context, now time.Time, retention time.Duration) (int64, error) {
	if retention <= 0 {
		retention = webhooks.WebhookIdempotencyTTL
	}
	cutoff := now.Add(-retention)

	merchantIDs, err := w.DB.Gen(ctx).ListActiveMerchantIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("cleanup webhook events: list merchants: %w", err)
	}
	var total int64
	var sweepErr error
	for _, mid := range merchantIDs {
		mctx := merchant.WithID(ctx, merchant.ID(mid))
		if err := w.DB.MerchantTx(mctx, func(ctx context.Context, tx pgx.Tx) error {
			n, err := gen.New(tx).DeleteCompletedWebhookEventsBefore(ctx, cutoff)
			total += n
			return err
		}); err != nil {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("delete webhook events for merchant %s: %w", mid, err))
		}
	}
	return total, sweepErr
}
