package riverjobs

import (
	"context"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"
)

const KindHoldExpiry = "billing.hold_expiry"

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
		tx, err := w.DB.GetDB().(*bun.DB).BeginTx(ctx, nil)
		if err != nil {
			return err
		}

		// Find expired active holds (stored as credit_transactions rows with transaction_type='hold')
		var holds []models.CreditTransaction
		if err := tx.NewSelect().
			Model(&holds).
			Where("transaction_type = ? AND status = ? AND expires_at IS NOT NULL AND expires_at <= ?", "hold", "active", now).
			OrderExpr("expires_at ASC").
			Limit(batchSize).
			For("UPDATE SKIP LOCKED").
			Scan(ctx); err != nil {
			_ = tx.Rollback()
			return err
		}

		if len(holds) == 0 {
			if err := tx.Commit(); err != nil {
				return err
			}
			break
		}

		// Group holds by user+credit_type to batch balance updates
		type key struct {
			UserID       string
			CreditTypeID uuid.UUID
		}
		releasedTotals := make(map[key]int64)

		for i := range holds {
			hold := &holds[i]
			if hold.Authorized == nil || *hold.Authorized <= 0 {
				continue
			}
			k := key{UserID: hold.UserID, CreditTypeID: hold.CreditTypeID}
			releasedTotals[k] += *hold.Authorized

			// Mark hold as expired
			hold.Status = "expired"
			hold.UpdatedAt = now
			if _, err := tx.NewUpdate().Model(hold).
				Column("status", "updated_at").
				WherePK().
				Exec(ctx); err != nil {
				_ = tx.Rollback()
				return err
			}
		}

		// Update balances - subtract from held_balance to make credits available again
		for k, amount := range releasedTotals {
			if amount <= 0 {
				continue
			}

			bal := new(models.UserCreditBalance)
			err := tx.NewSelect().
				Model(bal).
				Where("user_id = ? AND credit_type_id = ?", k.UserID, k.CreditTypeID).
				For("UPDATE").
				Scan(ctx)
			if err != nil {
				_ = tx.Rollback()
				return err
			}

			// Reduce held_balance - credits become available again
			newHeldBalance := bal.HeldBalance - amount
			if newHeldBalance < 0 {
				// Shouldn't happen, but be safe
				newHeldBalance = 0
			}

			if _, err := tx.NewUpdate().Model((*models.UserCreditBalance)(nil)).
				Set("held_balance = ?", newHeldBalance).
				Set("updated_at = ?", now).
				Where("user_id = ? AND credit_type_id = ?", k.UserID, k.CreditTypeID).
				Exec(ctx); err != nil {
				_ = tx.Rollback()
				return err
			}

			logger.WithFields(log.Fields{
				"user_id":        k.UserID,
				"credit_type_id": k.CreditTypeID,
				"amount":         amount,
			}).Debug("released expired hold credits")
		}

		if err := tx.Commit(); err != nil {
			return err
		}

		totalExpired += len(holds)
		logger.WithField("expired_holds", len(holds)).Info("expired credit holds in batch")

		if len(holds) < batchSize {
			break
		}
	}

	if totalExpired > 0 {
		logger.WithField("total_expired", totalExpired).Info("completed hold expiry job")
	}

	return nil
}
