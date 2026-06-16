package riverjobs

import (
	"context"
	"fmt"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const KindHoldExpiry = "openrails.hold_expiry"

type HoldExpiryArgs struct{}

func (HoldExpiryArgs) Kind() string { return KindHoldExpiry }

// HoldExpiryWorker now expires only durable money windows. Request holds moved
// to Redis in #505 and expire through Redis TTL.
// Controlled by config.FeatureFlags.DisableEntitlementExpiration - when true, skips expiration.
type HoldExpiryWorker struct {
	river.WorkerDefaults[HoldExpiryArgs]
	DB        *db.DB
	Config    *config.Config
	Clock     clockwork.Clock
	BatchSize int
}

func (HoldExpiryWorker) Kind() string { return KindHoldExpiry }

func (w HoldExpiryWorker) Work(ctx context.Context, job *river.Job[HoldExpiryArgs]) error {
	if w.DB == nil {
		return fmt.Errorf("hold expiry worker: db is required")
	}

	// Check if entitlement expiration is disabled via feature flags
	if w.Config != nil && w.Config.IsEntitlementExpirationDisabled() {
		log.WithContext(ctx).WithField("worker", KindHoldExpiry).
			Warn("Entitlement expiration disabled via feature flag; skipping hold expiry")
		return nil
	}

	clock := w.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	batchSize := w.BatchSize
	if batchSize <= 0 {
		batchSize = 100
	}

	logger := log.WithContext(ctx).WithField("worker", KindHoldExpiry)

	// Money windows (#335) expire on the same sweep: release each open,
	// past-expiry window's unsettled remainder and mark it expired.
	moneySvc := money.NewMoneyService(w.DB, clock)
	expiredWindows, err := moneySvc.ExpireWindows(ctx, batchSize)
	if err != nil {
		return fmt.Errorf("expire money windows: %w", err)
	}
	if expiredWindows > 0 {
		logger.WithField("expired_windows", expiredWindows).Info("expired credit windows")
	}

	return nil
}
