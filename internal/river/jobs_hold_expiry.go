package riverjobs

import (
	"context"
	"fmt"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const KindHoldExpiry = "openrails.hold_expiry"

type HoldExpiryArgs struct{}

func (HoldExpiryArgs) Kind() string { return KindHoldExpiry }

// HoldExpiryWorker expires credit holds that have passed their expires_at time.
// When a hold expires, the held credits become available again (no transaction created).
// This handles cases where a job crashes without calling capture/release.
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

	now := clock.Now().UTC()
	logger := log.WithContext(ctx).WithField("worker", KindHoldExpiry)

	totalExpired := 0

	for {
		batch := 0
		// Privileged (no-GUC) cross-tenant sweep with explicit tenant predicates.
		err := w.DB.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			q := gen.New(tx)

			// Find expired active holds (stored as money_transactions rows with transaction_type='hold')
			batchSize32, _ := safecast.Convert[int32](batchSize)
			holds, err := q.ListExpiredActiveMoneyHoldsForUpdate(ctx, gen.ListExpiredActiveMoneyHoldsForUpdateParams{
				Now: now, BatchSize: batchSize32,
			})
			if err != nil {
				return err
			}
			batch = len(holds)
			if batch == 0 {
				return nil
			}

			// Held is DERIVED from active holds (#491): flipping a hold to 'expired'
			// IS the release — no balance cache to update. Funds become available
			// again automatically once the hold no longer counts as active.
			for i := range holds {
				if err := q.ExpireMoneyHold(ctx, gen.ExpireMoneyHoldParams{ID: holds[i].ID, UpdatedAt: now}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if batch == 0 {
			break
		}

		totalExpired += batch
		logger.WithField("expired_holds", batch).Info("expired credit holds in batch")

		if batch < batchSize {
			break
		}
	}

	if totalExpired > 0 {
		logger.WithField("total_expired", totalExpired).Info("completed hold expiry job")
	}

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
