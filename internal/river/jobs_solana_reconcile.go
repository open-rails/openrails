package riverjobs

import (
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/alerting"

	"context"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	"github.com/open-rails/openrails/internal/shared/progress"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const (
	// KindSolanaReconcile cross-checks confirmed on-chain pulls against the
	// ledger. The cranker records a payment (RenewMembership), then stamps
	// last_signature (AdvanceAfterPull); a crash between them, or an
	// idempotency skip that masked a real gap, leaves a confirmed pull with no
	// payment row, which this sweep puts in the merchant inbox as a ledger
	// repair.
	KindSolanaReconcile = "openrails.solana_reconcile"

	solanaReconcileBatchSize = 500
)

// SolanaReconcileArgs triggers a ledger-reconciliation sweep over active subs.
type SolanaReconcileArgs struct{}

func (SolanaReconcileArgs) Kind() string { return KindSolanaReconcile }

// SolanaReconcileWorker verifies that every recorded on-chain pull
// (solana_subscriptions.last_signature) has a matching billing.payments row.
// Missing rows put a ledger repair in the merchant inbox; it never mutates the
// ledger itself (operator-led repair, like the other reconcilers).
type SolanaReconcileWorker struct {
	river.WorkerDefaults[SolanaReconcileArgs]
	DB        *db.DB
	Clock     clockwork.Clock
	BatchSize int
}

func (SolanaReconcileWorker) Kind() string { return KindSolanaReconcile }

func (w *SolanaReconcileWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *SolanaReconcileWorker) Work(ctx context.Context, _ *river.Job[SolanaReconcileArgs]) error {
	if w.DB == nil {
		log.WithContext(ctx).Warn("Solana reconcile worker not wired (no DB); skipping")
		return nil
	}
	batch := w.BatchSize
	if batch <= 0 {
		batch = solanaReconcileBatchSize
	}
	subRepo := solanasubs.NewSolanaSubscriptionRepo(w.DB)
	rows, err := subRepo.ListActiveWithSignature(ctx, batch)
	if err != nil {
		return fmt.Errorf("solana reconcile: list active subscriptions: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}

	paymentRepo := payments.NewPaymentRepo(w.DB)
	var drift int
	var failures int
	for _, row := range rows {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if row.LastSignature == nil || *row.LastSignature == "" {
			continue
		}
		progress.Mark(ctx, "solana reconcile subscription "+row.ID.String())
		sig := *row.LastSignature
		err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(row.MerchantID), "solana reconcile pull", func(mctx context.Context) error {
			scopeMerchantID, scopeErr := merchant.Require(mctx)
			if scopeErr != nil {
				return scopeErr
			}
			parent, err := w.DB.Gen(mctx).GetSubscriptionByID(mctx, gen.GetSubscriptionByIDParams{MerchantID: scopeMerchantID.UUID(), ID: row.SubscriptionID})
			if err != nil {
				return err
			}
			mctx = db.WithPSPID(mctx, parent.PspID)
			_, perr := paymentRepo.GetByPSPTransactionID(mctx, models.RailSolana, sig)
			if perr == nil {
				return nil // ledger is consistent for this pull
			}
			if !db.IsNotFound(perr) {
				// Transient lookup failure: log and move on, don't false-alarm.
				log.WithContext(mctx).WithError(perr).WithField("signature", sig).
					Warn("Solana reconcile: payment lookup failed; skipping row")
				return nil
			}

			// Confirmed pull with no payment row -> operator repair.
			drift++
			subID := row.SubscriptionID
			if alertErr := alerting.RecordLedgerRepair(mctx, w.DB, w.now(), alerting.LedgerRepair{
				Provider:       string(models.RailSolana),
				Operation:      "solana_crank_unrecorded_pull",
				TransactionID:  sig,
				SubscriptionID: &subID,
				Err:            fmt.Errorf("confirmed on-chain pull %s has no billing.payments record", sig),
				Metadata: map[string]any{
					"subscription_pda": row.SubscriptionPDA,
					"merchant_address": row.MerchantAddress,
					"tenant_id":        row.MerchantID.String(),
				},
			}); alertErr != nil {
				log.WithContext(mctx).WithError(alertErr).WithField("signature", sig).
					Warn("Solana reconcile: failed to record repair alert")
			}
			return nil
		})
		if err != nil {
			log.WithContext(ctx).WithError(err).WithField("subscription_pda", row.SubscriptionPDA).
				Error("Solana reconcile: merchant-scoped row failed; continuing")
			failures++
		}
	}
	if drift > 0 {
		log.WithContext(ctx).WithField("drift", drift).
			Warn("Solana reconcile: confirmed pulls missing ledger payments (#258)")
	}
	if failures > 0 {
		return fmt.Errorf("solana reconcile: %d of %d rows failed", failures, len(rows))
	}
	return nil
}
