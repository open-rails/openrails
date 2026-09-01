package riverjobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/shared/progress"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const (
	// KindSolanaReconcile cross-checks confirmed on-chain pulls against the
	// billing ledger (#258). The cranker records a payment (RenewMembership) and
	// then stamps last_signature (AdvanceAfterPull) in two steps; a crash between
	// them — or a RenewMembership idempotency skip that masked a real gap — leaves
	// a confirmed pull with no payment row. This sweep surfaces that drift as an
	// operator repair alert (same surface as the Stripe/NMI ledger reconcilers).
	KindSolanaReconcile = "openrails.solana_reconcile"

	solanaReconcileBatchSize = 500
)

// SolanaReconcileArgs triggers a ledger-reconciliation sweep over active subs.
type SolanaReconcileArgs struct{}

func (SolanaReconcileArgs) Kind() string { return KindSolanaReconcile }

// SolanaReconcileWorker verifies that every recorded on-chain pull
// (solana_subscriptions.last_signature) has a matching openrails.payments row.
// Missing rows emit a billing_ledger_repair_required alert; it never mutates the
// ledger itself (operator-led repair, like the other reconcilers).
type SolanaReconcileWorker struct {
	river.WorkerDefaults[SolanaReconcileArgs]
	DB                  *db.DB
	NotificationService *subscriptions.NotificationService
	Clock               clockwork.Clock
	BatchSize           int
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
		_, perr := paymentRepo.GetByTransactionID(ctx, models.RailSolana, sig)
		if perr == nil {
			continue // ledger is consistent for this pull
		}
		if !db.IsNotFound(perr) {
			// Transient lookup failure: log and move on, don't false-alarm.
			log.WithContext(ctx).WithError(perr).WithField("signature", sig).
				Warn("Solana reconcile: payment lookup failed; skipping row")
			continue
		}

		// Confirmed pull with no payment row -> operator repair.
		drift++
		subID := row.SubscriptionID
		if alertErr := webhooks.RecordLedgerRepairAlert(ctx, w.NotificationService, w.DB, w.now(), webhooks.LedgerRepairAlert{
			Provider:       string(models.RailSolana),
			Operation:      "solana_crank_unrecorded_pull",
			TransactionID:  sig,
			SubscriptionID: &subID,
			Err:            fmt.Errorf("confirmed on-chain pull %s has no openrails.payments record", sig),
			Metadata: map[string]any{
				"subscription_pda": row.SubscriptionPDA,
				"merchant_address": row.MerchantAddress,
				"tenant_id":        row.MerchantID.String(),
			},
		}); alertErr != nil {
			log.WithContext(ctx).WithError(alertErr).WithField("signature", sig).
				Warn("Solana reconcile: failed to record repair alert")
		}
	}
	if drift > 0 {
		log.WithContext(ctx).WithField("drift", drift).
			Warn("Solana reconcile: confirmed pulls missing ledger payments (#258)")
	}
	return nil
}
