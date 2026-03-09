package riverjobs

import (
	"context"
	"database/sql"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"
)

const KindCreditExpiry = "billing.credit_expiry"

type CreditExpiryArgs struct{}

func (CreditExpiryArgs) Kind() string { return KindCreditExpiry }

// CreditExpiryWorker expires credit blocks that have passed their expiration date.
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
		var blocks []models.CreditBlock
		if err := tx.NewSelect().
			Model(&blocks).
			Where("remaining_amount > 0 AND expires_at IS NOT NULL AND expires_at <= ?", now).
			OrderExpr("expires_at ASC").
			Limit(batchSize).
			For("UPDATE SKIP LOCKED").
			Scan(ctx); err != nil {
			_ = tx.Rollback()
			return err
		}
		if len(blocks) == 0 {
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
		for i := range blocks {
			if blocks[i].RemainingAmount <= 0 {
				continue
			}
			k := key{UserID: blocks[i].UserID, CreditTypeID: blocks[i].CreditTypeID}
			expiredTotals[k] += blocks[i].RemainingAmount
			blocks[i].RemainingAmount = 0
			if _, err := tx.NewUpdate().Model(&blocks[i]).
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
					ID:           uuid.New(),
					UserID:       k.UserID,
					CreditTypeID: k.CreditTypeID,
					Balance:      0,
					HeldBalance:  0,
					CreatedAt:    now,
					UpdatedAt:    now,
				}
				if _, err := tx.NewInsert().Model(bal).Exec(ctx); err != nil {
					_ = tx.Rollback()
					return err
				}
			}

			newBalance := bal.Balance - amount
			if newBalance < 0 {
				newBalance = 0
			}

			if _, err := tx.NewUpdate().Model((*models.UserCreditBalance)(nil)).
				Set("balance = ?", newBalance).
				Set("updated_at = ?", now).
				Where("user_id = ? AND credit_type_id = ?", k.UserID, k.CreditTypeID).
				Exec(ctx); err != nil {
				_ = tx.Rollback()
				return err
			}

			trx := &models.CreditTransaction{
				ID:              uuid.New(),
				UserID:          k.UserID,
				CreditTypeID:    k.CreditTypeID,
				Amount:          -amount,
				BalanceAfter:    &newBalance,
				TransactionType: "expiry",
				Status:          "posted",
				Source:          "expiry_job",
				ExpiresAt:       &now,
				CreatedAt:       now,
				UpdatedAt:       now,
			}
			if _, err := tx.NewInsert().Model(trx).Exec(ctx); err != nil {
				_ = tx.Rollback()
				return err
			}
		}

		if err := tx.Commit(); err != nil {
			return err
		}
		logger.WithField("expired_blocks", len(blocks)).Info("expired credit blocks")
		if len(blocks) < batchSize {
			break
		}
	}

	return nil
}

func errorsIsNoRows(err error) bool {
	return err != nil && err == sql.ErrNoRows
}
