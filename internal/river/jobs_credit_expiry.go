package riverjobs

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/doujins-org/doujins-billing/config"
	"github.com/doujins-org/doujins-billing/internal/db"
	"github.com/doujins-org/doujins-billing/internal/db/models"
	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"
)

const KindCreditExpiry = "billing.credit_expiry"

type CreditExpiryArgs struct{}

func (CreditExpiryArgs) Kind() string { return KindCreditExpiry }

// CreditExpiryWorker expires credit batches that have passed their expiration date.
// Controlled by config.FeatureFlags.DisableEntitlementExpiration - when true, skips expiration.
type CreditExpiryWorker struct {
	river.WorkerDefaults[CreditExpiryArgs]
	DB        *db.DB
	Config    *config.Config
	Clock     clockwork.Clock
	BatchSize int
}

func (CreditExpiryWorker) Kind() string { return KindCreditExpiry }

func (w CreditExpiryWorker) Work(ctx context.Context, job *river.Job[CreditExpiryArgs]) error {
	if w.DB == nil {
		return fmt.Errorf("db is required")
	}

	// Check if entitlement expiration is disabled via feature flags
	if w.Config != nil && w.Config.IsEntitlementExpirationDisabled() {
		log.WithContext(ctx).WithField("worker", KindCreditExpiry).
			Warn("Entitlement expiration disabled via feature flag; skipping credit expiry")
		return nil
	}

	clock := w.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	batchSize := w.BatchSize
	if batchSize <= 0 {
		batchSize = 200
	}

	now := clock.Now().UTC()
	logger := log.WithContext(ctx).WithField("worker", KindCreditExpiry)

	for {
		tx, err := w.DB.GetDB().(*bun.DB).BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		var batches []models.CreditExpiryBatch
		if err := tx.NewSelect().
			Model(&batches).
			Where("remaining_amount > 0 AND expires_at <= ?", now).
			OrderExpr("expires_at ASC").
			Limit(batchSize).
			For("UPDATE SKIP LOCKED").
			Scan(ctx); err != nil {
			_ = tx.Rollback()
			return err
		}
		if len(batches) == 0 {
			if err := tx.Commit(); err != nil {
				return err
			}
			break
		}

		type key struct {
			UserID       string
			CreditTypeID uuid.UUID
		}
		expiredTotals := make(map[key]int64)
		for i := range batches {
			if batches[i].RemainingAmount <= 0 {
				continue
			}
			k := key{UserID: batches[i].UserID, CreditTypeID: batches[i].CreditTypeID}
			expiredTotals[k] += batches[i].RemainingAmount
			batches[i].RemainingAmount = 0
			if _, err := tx.NewUpdate().Model(&batches[i]).
				Column("remaining_amount").
				WherePK().
				Exec(ctx); err != nil {
				_ = tx.Rollback()
				return err
			}
		}

		for k, amount := range expiredTotals {
			if amount <= 0 {
				continue
			}
			bal := new(models.UserCreditBalance)
			err := tx.NewSelect().
				Model(bal).
				Where("user_id = ? AND credit_type_id = ?", k.UserID, k.CreditTypeID).
				For("UPDATE").
				Scan(ctx)
			if err != nil && !errorsIsNoRows(err) {
				_ = tx.Rollback()
				return err
			}
			if errorsIsNoRows(err) {
				bal = &models.UserCreditBalance{
					UserID:       k.UserID,
					CreditTypeID: k.CreditTypeID,
					Balance:      0,
					HeldBalance:  0,
					Permanent:    0,
					Expiring:     0,
					CreatedAt:    now,
					UpdatedAt:    now,
				}
				if _, err := tx.NewInsert().Model(bal).Exec(ctx); err != nil {
					_ = tx.Rollback()
					return err
				}
			}

			newBalance := bal.Balance - amount
			if newBalance < bal.HeldBalance {
				newBalance = bal.HeldBalance
			}
			if newBalance < 0 {
				newBalance = 0
			}
			newExpiring := bal.Expiring - amount
			if newExpiring < 0 {
				newExpiring = 0
			}

			var earliest *time.Time
			var next time.Time
			if err := tx.NewSelect().
				Model((*models.CreditExpiryBatch)(nil)).
				ColumnExpr("min(expires_at)").
				Where("user_id = ? AND credit_type_id = ? AND remaining_amount > 0 AND expires_at > ?", k.UserID, k.CreditTypeID, now).
				Scan(ctx, &next); err == nil && !next.IsZero() {
				earliest = &next
			}

			if _, err := tx.NewUpdate().Model((*models.UserCreditBalance)(nil)).
				Set("balance = ?", newBalance).
				Set("expiring_balance = ?", newExpiring).
				Set("earliest_expiry = ?", earliest).
				Set("updated_at = ?", now).
				Where("user_id = ? AND credit_type_id = ?", k.UserID, k.CreditTypeID).
				Exec(ctx); err != nil {
				_ = tx.Rollback()
				return err
			}

			trx := &models.CreditTransaction{
				UserID:          k.UserID,
				CreditTypeID:    k.CreditTypeID,
				Amount:          -amount,
				BalanceAfter:    newBalance,
				TransactionType: "expiry",
				Source:          "expiry_job",
				ExpiresAt:       &now,
				CreatedAt:       now,
			}
			if _, err := tx.NewInsert().Model(trx).Exec(ctx); err != nil {
				_ = tx.Rollback()
				return err
			}
		}

		if err := tx.Commit(); err != nil {
			return err
		}
		logger.WithField("expired_batches", len(batches)).Info("expired credit batches")
		if len(batches) < batchSize {
			break
		}
	}

	return nil
}

func errorsIsNoRows(err error) bool {
	return err != nil && err == sql.ErrNoRows
}
