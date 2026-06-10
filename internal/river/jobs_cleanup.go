package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const KindCleanupExpiredData = "billing.cleanup_expired_data"

// CleanupConfig defines retention periods for various data types
type CleanupConfig struct {
	// NotificationSeenRetention is how long to keep seen notifications
	// Default: 90 days (matches model's IsExpiredForCleanup)
	NotificationSeenRetention time.Duration

	// NotificationUnseenRetention is how long to keep unseen notifications
	// Default: 180 days (matches model's IsExpiredForCleanup)
	NotificationUnseenRetention time.Duration
}

// DefaultCleanupConfig returns sensible default retention periods
func DefaultCleanupConfig() CleanupConfig {
	return CleanupConfig{
		NotificationSeenRetention:   90 * 24 * time.Hour,
		NotificationUnseenRetention: 180 * 24 * time.Hour,
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

	logger.WithFields(log.Fields{
		"checkout_sessions_expired": result.CheckoutSessionsExpired,
		"notifications_seen":        result.NotificationsSeen,
		"notifications_unseen":      result.NotificationsAll,
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
