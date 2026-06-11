package riverjobs

import (
	"context"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
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
		batch := 0
		// Privileged (no-GUC) cross-tenant sweep with explicit tenant predicates.
		err := w.DB.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			q := gen.New(tx)
			batchSize32, _ := safecast.Convert[int32](batchSize)
			blocks, err := q.ListExpiredCreditBlocksForUpdate(ctx, gen.ListExpiredCreditBlocksForUpdateParams{
				Now: now, BatchSize: batchSize32,
			})
			if err != nil {
				return err
			}
			batch = len(blocks)
			if batch == 0 {
				return nil
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
				if err := q.SetCreditBlockRemaining(ctx, gen.SetCreditBlockRemainingParams{
					ID: blocks[i].ID, RemainingAmount: 0,
				}); err != nil {
					return err
				}
			}

			for k, amount := range expiredTotals {
				if amount <= 0 {
					continue
				}
				balKey := gen.LockCreditBalanceParams{
					TenantID: k.TenantID, TenantSubjectID: k.TenantSubjectID, CreditTypeID: k.CreditTypeID,
				}
				bal, err := q.LockCreditBalance(ctx, balKey)
				if err != nil && !errorsIsNoRows(err) {
					return err
				}
				if errorsIsNoRows(err) {
					if err := q.InsertCreditBalanceIfAbsent(ctx, gen.InsertCreditBalanceIfAbsentParams{
						ID: uuidutil.NewV7(), TenantID: k.TenantID, TenantSubjectID: k.TenantSubjectID,
						CreditTypeID: k.CreditTypeID, Now: now,
					}); err != nil {
						return err
					}
					bal, err = q.LockCreditBalance(ctx, balKey)
					if err != nil {
						return err
					}
				}

				newBalance := bal.Balance - amount
				if newBalance < 0 {
					newBalance = 0
				}

				if err := q.SetCreditBalance(ctx, gen.SetCreditBalanceParams{
					TenantID: k.TenantID, TenantSubjectID: k.TenantSubjectID, CreditTypeID: k.CreditTypeID,
					Balance: newBalance, UpdatedAt: now,
				}); err != nil {
					return err
				}

				if err := q.InsertCreditTransaction(ctx, gen.InsertCreditTransactionParams{
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
				}); err != nil {
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
		logger.WithField("expired_blocks", batch).Info("expired credit blocks")
		if batch < batchSize {
			break
		}
	}

	return nil
}

func errorsIsNoRows(err error) bool {
	return err != nil && repo.IsNotFound(err)
}
