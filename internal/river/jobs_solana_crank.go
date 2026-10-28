package riverjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/alerting"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/pull"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	"github.com/open-rails/openrails/internal/writeposture"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const (
	// KindSolanaCrank is the recurring Solana pull ("cranking") job, the
	// Solana analog of the NMI DunningWorker: each run charges every due
	// subscription.
	KindSolanaCrank = "openrails.solana_crank"
	// SolanaCrankInterval is the crank's cadence; next_pull_at decides what
	// is due.
	SolanaCrankInterval = time.Hour

	solanaCrankBatchSize = 200
)

// SolanaCrankArgs triggers a cranking run over all due Solana subscriptions.
type SolanaCrankArgs struct{}

func (SolanaCrankArgs) Kind() string { return KindSolanaCrank }

// SolanaCrankWorker cranks each due Solana subscription: pull the plan amount
// on-chain, RenewMembership (idempotent on the tx signature) and advance
// next_pull_at. A failed pull routes to FailMembership and dunning.
//
// Missed periods are never back-billed: after downtime each subscription gets
// one pull, and the new period anchors at the pull moment. The program
// enforces the same bound (one plan amount per period, Custom:400 "period
// already paid", no banked balance), so even a buggy cranker cannot collect
// more than the current period.
type SolanaCrankWorker struct {
	WorkerDefaults[SolanaCrankArgs]
	DB        *db.DB
	Config    *config.Config
	Clock     clockwork.Clock
	BatchSize int
	// Intents is the write-through provider-intent runner, required by Work:
	// each due row posts a durable solana_pull intent (keyed on next_pull_at)
	// and executes it inline through SolanaPullIntentHandler, so a crash
	// between signing and finalize resolves via the recorded pre-submit
	// signature, never a blind re-pull. The intent handler's own core is
	// runner-less and never registered as a worker.
	Intents *intents.Runner
	// Balances reads the cranker wallets of the ctx merchant. After a pass
	// that signed a merchant's pulls, a low wallet raises its low-gas finding
	// and a funded one resolves it.
	Balances func(context.Context) ([]solanamodule.CrankerBalance, error)
}

func (SolanaCrankWorker) Kind() string { return KindSolanaCrank }

func (w *SolanaCrankWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *SolanaCrankWorker) Work(ctx context.Context, _ *river.Job[SolanaCrankArgs]) error {
	if w.Config != nil && config.IsLimitedMode(w.Config) {
		log.WithContext(ctx).Warn("limited mode: skipping Solana recurring pulls (#345)")
		return nil
	}
	// A pull without a durable intent is a charge nothing can recover: a
	// wiring defect, so fail the job loudly.
	if w.Intents == nil {
		return fmt.Errorf("solana crank: no intent runner wired; a recurring pull must post a durable intent first (#674)")
	}
	batch := w.BatchSize
	if batch <= 0 {
		batch = solanaCrankBatchSize
	}
	due, err := w.listDue(ctx, batch)
	if err != nil {
		return fmt.Errorf("solana crank: list due: %w", err)
	}
	if len(due) == 0 {
		return nil
	}
	log.WithContext(ctx).WithField("count", len(due)).Info("Solana cranker: processing due subscriptions")

	failures := 0
	signed := map[uuid.UUID]bool{}
	for _, row := range due {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		// Per-row isolation: one subscriber's failure never aborts the batch.
		// Durable intent first, inline execution, pre-submit signature
		// write-ahead: a crash resolves off the recorded signature, never a
		// paid-but-unrenewed subscriber. The handler freezes what it pulls when
		// it executes.
		err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(row.MerchantID), "solana crank pull intent", func(mctx context.Context) error {
			mctx = db.WithPSPID(mctx, row.PspID)
			pull, err := w.Intents.EnqueueAndExecute(mctx, intents.EnqueueParams{
				MerchantID:     row.MerchantID,
				Provider:       string(models.RailSolana),
				PspID:          row.PspID,
				IntentType:     pull.Type,
				SubscriptionID: &row.SubscriptionID,
				Payload: pull.Payload{
					SubscriptionPDA: row.SubscriptionPDA,
					RowID:           row.ID,
					NextPullAt:      row.NextPullAt.UTC(),
				},
				IdempotencyKey: pull.IdempotencyKey(row.ID, row.NextPullAt),
				NextAttemptAt:  w.now(),
				Origin:         intents.OriginSystem,
				OriginReason:   "solana recurring pull (cranking)",
			})
			if err == nil && pullSigned(pull) {
				signed[row.MerchantID] = true
			}
			return err
		})
		if err != nil {
			log.WithContext(ctx).WithError(err).WithField("subscription_pda", row.SubscriptionPDA).
				Error("Solana cranker: post pull intent failed; continuing")
			failures++
		}
	}
	for mid := range signed {
		w.checkGas(ctx, billing.MerchantID(mid))
	}
	if failures > 0 {
		return fmt.Errorf("solana crank: %d of %d pull intents failed", failures, len(due))
	}
	return nil
}

// pullSigned reports whether a pull's transaction was signed: its submission
// recorded the signature.
func pullSigned(pull gen.BillingProviderIntent) bool {
	var evidence struct {
		TransactionID string `json:"transaction_id"`
	}
	return json.Unmarshal(pull.ResultEvidence, &evidence) == nil && evidence.TransactionID != ""
}

// checkGas raises or resolves the low-gas finding of each cranker wallet that
// signs mid's pulls. A wallet whose balance cannot be read keeps its finding.
func (w *SolanaCrankWorker) checkGas(ctx context.Context, mid billing.MerchantID) {
	if w.Balances == nil {
		return
	}
	// The chain reads hold no connection; only the findings do.
	balances, err := w.Balances(merchant.WithID(ctx, mid))
	if len(balances) > 0 {
		err = errors.Join(err, w.DB.RunInMerchantScope(ctx, mid, "solana cranker gas", func(mctx context.Context) error {
			var errs error
			for _, b := range balances {
				errs = errors.Join(errs, alerting.RecordCrankerGas(mctx, w.DB, w.now(), alerting.CrankerGas{
					PSPID: b.PSPID, Wallet: b.Wallet.String(), Network: b.Network, Lamports: b.Lamports, Threshold: solanamodule.CrankerWarningLamports}))
			}
			return errs
		}))
	}
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).Warn("Solana cranker: gas check failed")
	}
}

// listDue is the pass's due episodes, merchants taking turns. A merchant whose
// write posture holds recurring pulls is left out, so its rows take no slot.
func (w *SolanaCrankWorker) listDue(ctx context.Context, batch int) ([]*models.SolanaSubscription, error) {
	repo := solanasubs.NewSolanaSubscriptionRepo(w.DB)
	postures := writeposture.View{Config: w.Config, DB: w.DB}
	var held []uuid.UUID
	checked := map[uuid.UUID]bool{}
	for {
		due, err := repo.ListDuePulls(ctx, w.now(), held, batch)
		if err != nil {
			return nil, err
		}
		limited := false
		for _, row := range due {
			if checked[row.MerchantID] {
				continue
			}
			checked[row.MerchantID] = true
			if posture := postures.Posture(ctx, row.MerchantID); posture.Limited() {
				log.WithContext(ctx).WithFields(log.Fields{"merchant_id": row.MerchantID, "reason": posture.Reason}).Warn("Solana cranker: recurring pulls wait for this merchant's write posture")
				held, limited = append(held, row.MerchantID), true
			}
		}
		if !limited {
			return due, nil
		}
	}
}
