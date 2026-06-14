package riverjobs

import (
	"context"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const KindCreditExpiry = "openrails.credit_expiry"

type CreditExpiryArgs struct{}

func (CreditExpiryArgs) Kind() string { return KindCreditExpiry }

// CreditExpiryWorker COMPACTS the credit-block table (#491): it deletes
// fully-spent (remaining_amount=0) and expired (past expires_at) money_blocks,
// keeping the spendable-lot table bounded so the derived available SUM stays
// cheap. Balance is derived from the unexpired, unspent lots — expired lots stop
// counting the moment they pass expires_at, so deletion is pure compaction (no
// balance movement, no ledger row). It touches money_blocks ONLY; the deposit
// receipt (money_transactions + payments) is never touched.
// Controlled by config.FeatureFlags.DisableEntitlementExpiration - when true, skips.
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
		batch := int64(0)
		// Privileged (no-GUC) cross-tenant compaction: delete spent + expired lots.
		err := w.DB.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			batchSize32, _ := safecast.Convert[int32](batchSize)
			deleted, err := gen.New(tx).DeleteCompactableMoneyBlocks(ctx, gen.DeleteCompactableMoneyBlocksParams{
				Now: now, BatchSize: batchSize32,
			})
			if err != nil {
				return err
			}
			batch = deleted
			return nil
		})
		if err != nil {
			return err
		}
		if batch == 0 {
			break
		}
		logger.WithField("compacted_blocks", batch).Info("compacted spent/expired credit blocks")
		if batch < int64(batchSize) {
			break
		}
	}

	return nil
}
