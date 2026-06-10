package riverjobs

import (
	"context"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
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

		// HARDCUT (#221/#223): credit rows are payer+tenant-scoped. Expiry rolls up
		// per (tenant, payer, credit_type); actor is carried for actor attribution
		// only and is not part of the balance key.
		type key struct {
			TenantID        uuid.UUID
			TenantSubjectID uuid.UUID
			CreditTypeID    uuid.UUID
		}
		expiredTotals := make(map[key]int64)
		for i := range blocks {
			if blocks[i].RemainingAmount <= 0 {
				continue
			}
			k := key{
				TenantID:        blocks[i].TenantID,
				TenantSubjectID: blocks[i].TenantSubjectID,
				CreditTypeID:    blocks[i].CreditTypeID,
			}
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
			bal := new(models.CreditBalance)
			err := tx.NewSelect().
				Model(bal).
				Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", k.TenantID, k.TenantSubjectID, k.CreditTypeID).
				For("UPDATE").
				Scan(ctx)
			if err != nil && !errorsIsNoRows(err) {
				_ = tx.Rollback()
				return err
			}
			if errorsIsNoRows(err) {
				bal = &models.CreditBalance{
					ID:              uuidutil.NewV7(),
					TenantID:        k.TenantID,
					TenantSubjectID: k.TenantSubjectID,
					CreditTypeID:    k.CreditTypeID,
					Balance:         0,
					HeldBalance:     0,
					CreatedAt:       now,
					UpdatedAt:       now,
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

			if _, err := tx.NewUpdate().Model((*models.CreditBalance)(nil)).
				Set("balance = ?", newBalance).
				Set("updated_at = ?", now).
				Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", k.TenantID, k.TenantSubjectID, k.CreditTypeID).
				Exec(ctx); err != nil {
				_ = tx.Rollback()
				return err
			}

			trx := &models.CreditTransaction{
				ID:              uuidutil.NewV7(),
				TenantID:        k.TenantID,
				TenantSubjectID: k.TenantSubjectID,
				// System event: no caller actor; payer-derived per money_in convention.
				Actor:           k.TenantSubjectID.String(),
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
	return err != nil && repo.IsNotFound(err)
}
