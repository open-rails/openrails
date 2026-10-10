package riverjobs

import (
	"context"
	"fmt"
	"strconv"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/failpoint"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/progress"
	"github.com/open-rails/openrails/internal/writeposture"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const (
	// KindSolanaCrank is the recurring Solana pull ("cranking") job, the
	// Solana analog of the NMI DunningWorker: each run charges every due
	// subscription.
	KindSolanaCrank = "openrails.solana_crank"

	solanaCrankBatchSize = 200
)

// SolanaCrankArgs triggers a cranking run over all due Solana subscriptions.
type SolanaCrankArgs struct{}

func (SolanaCrankArgs) Kind() string { return KindSolanaCrank }

// solanaCranker is the on-chain pull surface (satisfied by *recurring.CrankService).
type solanaCranker interface {
	Crank(ctx context.Context, merchantID billing.MerchantID, sub *models.SolanaSubscription, amountBaseUnits uint64) (string, error)
}

// presubmitCranker is the optional signature write-ahead capability:
// presubmit(sig) persists the signed tx signature before submission, and
// memoLocalID (the durable pull-intent id; Nil = unstamped) is stamped on the
// pull tx as its self-recognition memo. Fakes without it skip the write-ahead.
type presubmitCranker interface {
	CrankWithPresubmit(ctx context.Context, merchantID billing.MerchantID, sub *models.SolanaSubscription, amountBaseUnits uint64, memoLocalID uuid.UUID, presubmit func(solanago.Signature, solanaint.ChainTerminal) error) (string, error)
}

// membershipManager is the lifecycle surface the cranker drives (satisfied by
// *subscriptions.SubscriptionLifecycleService): renew on a confirmed pull, fail
// (-> dunning) on a charge failure.
type membershipManager interface {
	RenewMembership(ctx context.Context, params *subscriptions.RenewMembershipParams) error
	FailMembership(ctx context.Context, params *subscriptions.FailMembershipParams) error
	// CancelMembership terminates the subscription (an out-of-band cancel
	// detected at rebill time: the subscriber revoked the delegation on-chain).
	CancelMembership(ctx context.Context, params *subscriptions.CancelMembershipParams) error
}

// solanaSubStore is the persistence surface crankOne mutates (satisfied by
// *solanasubs.SolanaSubscriptionRepo), so tests can drive the state machine
// with a fake.
type solanaSubStore interface {
	SetStatus(ctx context.Context, id uuid.UUID, status string) error
	SetNextPullAt(ctx context.Context, id uuid.UUID, nextPullAt time.Time) error
	AdvanceAfterPull(ctx context.Context, id uuid.UUID, periodStart time.Time, signature string, nextPullAt time.Time) error
}

// resolvedPlan is the per-subscription billing terms crankOne acts on: the
// on-chain pull amount + period + ghost-plan fingerprint, plus the fiat
// amount/currency recorded on renewal.
type resolvedPlan struct {
	amountBaseUnits uint64
	periodHours     uint64
	fingerprint     int64
	fiatAmount      int64
	currency        string
	// retryAttempts is the subscription's consecutive-failure count so far.
	retryAttempts int
	// The subscription a failed pull is an attempt of.
	customerID, pspID uuid.UUID
	policy            models.CollectionPolicy
	periodEnd         *time.Time
}

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
	river.WorkerDefaults[SolanaCrankArgs]
	DB        *db.DB
	Config    *config.Config
	Clock     clockwork.Clock
	Cranker   solanaCranker
	Lifecycle membershipManager
	BatchSize int
	// Intents is the write-through provider-intent runner, required by Work:
	// each due row posts a durable solana_pull intent (keyed on next_pull_at)
	// and executes it inline through SolanaPullIntentHandler, so a crash
	// between signing and finalize resolves via the recorded pre-submit
	// signature, never a blind re-pull. The intent handler's own core is
	// runner-less and never registered as a worker.
	Intents *intents.Runner

	// resolvePlanFn loads the billing terms for a row. nil in production (the
	// DB-backed w.resolvePlan); tests inject a fake.
	resolvePlanFn func(ctx context.Context, row *models.SolanaSubscription) (resolvedPlan, error)
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
	if w.Cranker == nil || w.Lifecycle == nil {
		log.WithContext(ctx).Warn("Solana cranker not fully wired (no cranker/lifecycle); skipping run")
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
	repo := solanasubs.NewSolanaSubscriptionRepo(w.DB)
	due, err := repo.ListDue(ctx, w.now(), batch)
	if err != nil {
		return fmt.Errorf("solana crank: list due: %w", err)
	}
	if len(due) == 0 {
		return nil
	}
	log.WithContext(ctx).WithField("count", len(due)).Info("Solana cranker: processing due subscriptions")

	failures := 0
	postures := writeposture.View{Config: w.Config, DB: w.DB}
	for _, row := range due {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		progress.Mark(ctx, "solana crank subscription "+row.ID.String())
		if posture := postures.Posture(ctx, row.MerchantID); posture.Limited() {
			log.WithContext(ctx).WithFields(log.Fields{"merchant_id": row.MerchantID, "reason": posture.Reason}).Warn("Solana cranker: recurring pulls wait for this merchant's write posture")
			continue
		}
		// Per-row isolation: one subscriber's failure never aborts the batch.
		// Durable intent first, inline execution, pre-submit signature
		// write-ahead: a crash resolves off the recorded signature, never a
		// paid-but-unrenewed subscriber.
		err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(row.MerchantID), "solana crank pull intent", func(mctx context.Context) error {
			_, err := w.Intents.EnqueueAndExecute(mctx, intents.EnqueueParams{
				MerchantID:     row.MerchantID,
				Provider:       string(models.RailSolana),
				PspID:          row.PspID,
				IntentType:     TypeSolanaPull,
				SubscriptionID: &row.SubscriptionID,
				Payload: SolanaPullPayload{
					SubscriptionPDA: row.SubscriptionPDA,
					RowID:           row.ID,
					NextPullAt:      row.NextPullAt.UTC(),
				},
				IdempotencyKey: SolanaPullIdempotencyKey(row.ID, row.NextPullAt),
				NextAttemptAt:  w.now(),
				Origin:         intents.OriginSystem,
				OriginReason:   "solana recurring pull (cranking)",
			})
			return err
		})
		if err != nil {
			log.WithContext(ctx).WithError(err).WithField("subscription_pda", row.SubscriptionPDA).
				Error("Solana cranker: post pull intent failed; continuing")
			failures++
		}
	}
	if failures > 0 {
		return fmt.Errorf("solana crank: %d of %d pull intents failed", failures, len(due))
	}
	return nil
}

// crankKind classifies a completed (error-free) crankOne for the intent ledger.
type crankKind int

const (
	crankSucceeded    crankKind = iota // pulled + renewed + advanced
	crankAlreadyPaid                   // period already paid on-chain; advanced without local renewal
	crankCanceled                      // delegate revoked → membership canceled (no dunning)
	crankDunned                        // recoverable decline → FailMembership + rescheduled
	crankGhostExpired                  // ghost plan → row expired
)

// crankOutcome is crankOne's classified result. signature is set on success
// (and, via the presubmit hook, durably recorded before submission).
type crankOutcome struct {
	kind      crankKind
	signature string
	reason    string
	evidence  map[string]any
}

// crankOne runs the pull state machine for one row. presubmit (optional)
// durably records the signed tx signature before submission. memoLocalID (the
// pull intent id; Nil = unstamped) is stamped on the pull tx as its SPL Memo,
// so a chain reader can resolve the tx to the intent with no local send
// record. An error means the pull is unresolved (operational failure, or a
// landed pull whose local finalize failed; the recorded signature tells them
// apart); nil means the period resolved (see crankKind).
func (w *SolanaCrankWorker) crankOne(ctx context.Context, repo solanaSubStore, row *models.SolanaSubscription, memoLocalID uuid.UUID, presubmit func(solanago.Signature, solanaint.ChainTerminal) error) (crankOutcome, error) {
	merchantID := billing.MerchantID(row.MerchantID)

	// Resolve the plan amount (token base units) + period + ghost-plan fingerprint
	// from the linked price's Solana rail config.
	resolve := w.resolvePlanFn
	if resolve == nil {
		resolve = w.resolvePlan
	}
	plan, err := resolve(ctx, row)
	if err != nil {
		return crankOutcome{}, err
	}
	amountBaseUnits := plan.amountBaseUnits
	periodHours := plan.periodHours
	fingerprint := plan.fingerprint

	// Ghost-plan guard: the plan was deleted + recreated at the same PDA. The
	// on-chain pull would fail PlanTermsMismatch; terminate this record.
	if fingerprint != 0 && row.PlanCreatedAtFingerprint != 0 && fingerprint != row.PlanCreatedAtFingerprint {
		log.WithContext(ctx).WithField("subscription_pda", row.SubscriptionPDA).
			Warn("Solana cranker: plan created_at fingerprint mismatch (ghost plan); expiring")
		if err := repo.SetStatus(ctx, row.ID, models.SolanaSubscriptionExpired); err != nil {
			return crankOutcome{}, err
		}
		return crankOutcome{kind: crankGhostExpired, reason: "plan fingerprint mismatch (ghost plan); subscription expired"}, nil
	}

	var sig string
	var crankErr error
	if pc, ok := w.Cranker.(presubmitCranker); ok && presubmit != nil {
		sig, crankErr = pc.CrankWithPresubmit(ctx, merchantID, row, amountBaseUnits, memoLocalID, presubmit)
	} else {
		sig, crankErr = w.Cranker.Crank(ctx, merchantID, row, amountBaseUnits)
	}
	if crankErr != nil {
		// ClassifyCrankError maps the on-chain error onto the billing
		// decline-code vocabulary and an action. Devnet codes: Custom:400 =
		// period already paid (idempotent); token OwnerMismatch (Custom:4) =
		// delegate revoked (terminal); token InsufficientFunds (Custom:1) =
		// recoverable; RPC/gas = operational.
		cf := recurring.ClassifyCrankError(crankErr)
		llog := log.WithContext(ctx).WithError(crankErr).WithFields(log.Fields{
			"subscription_pda": row.SubscriptionPDA,
			"decline_code":     string(cf.Code),
			"onchain_code":     cf.OnChainCode,
		})
		switch cf.Category {
		case recurring.Operational:
			// RPC/network or the cranker wallet out of SOL gas: retry next run, NEVER
			// dun — a shared outage would wrongly past-due a merchant's whole book.
			// Leave next_pull_at unchanged so it stays due.
			llog.Warn("Solana cranker: operational pull failure; retry next run (no dunning)")
			return crankOutcome{}, crankErr
		case recurring.AlreadyPaid:
			// Pulled on-chain but not recorded locally: advance past this
			// period without re-attempting or dunning; the reconcile worker
			// repairs the ledger. An intent-driven crank whose own recorded
			// signature landed repairs the renewal first, so never gets here.
			llog.Warn("Solana cranker: period already paid on-chain (idempotent); advancing, ledger repair via reconcile (#258)")
			periodHoursI64, err := safecast.Convert[int64](periodHours)
			if err != nil {
				return crankOutcome{}, fmt.Errorf("solana crank: period hours overflow: %w", err)
			}
			next := w.now().Add(time.Duration(periodHoursI64) * time.Hour)
			if err := repo.SetNextPullAt(ctx, row.ID, next); err != nil {
				return crankOutcome{}, err
			}
			return crankOutcome{
				kind:     crankAlreadyPaid,
				reason:   "period already paid on-chain; advanced without local renewal (reconcile repairs)",
				evidence: map[string]any{"decline_code": string(cf.Code)},
			}, nil
		case recurring.Terminal:
			// The subscriber revoked the SPL token delegate on-chain: cancel
			// and stop, never dun. A user's cancel_subscription stops only
			// future-period pulls, so it raises no crank error here; OpenRails
			// mirrors it at confirm. Solana cancels are immediate, never
			// scheduled.
			llog.Warn("Solana cranker: terminal pull failure (delegate revoked); cancelling subscription (no dunning)")
			if err := w.recordPullAttempt(ctx, row, plan, memoLocalID, string(cf.Code), crankErr.Error()); err != nil {
				return crankOutcome{}, err
			}
			if err := repo.SetStatus(ctx, row.ID, models.SolanaSubscriptionCanceled); err != nil {
				return crankOutcome{}, fmt.Errorf("solana crank: set canceled status: %w", err)
			}
			subID := row.SubscriptionID
			proc := models.RailSolana
			reason := fmt.Sprintf("%s (%s)", crankErr.Error(), cf.Code)
			if err := w.Lifecycle.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
				Rail:           &proc,
				SubscriptionID: &subID,
				CancelType:     models.CancelTypeUser,
				CancelFeedback: &reason,
				RevokeAccess:   true,
			}); err != nil {
				return crankOutcome{}, fmt.Errorf("solana crank: cancel membership: %w", err)
			}
			return crankOutcome{
				kind:     crankCanceled,
				reason:   reason,
				evidence: map[string]any{"decline_code": string(cf.Code)},
			}, nil
		default:
			// Recoverable subscriber decline (insufficient USDC, etc.) ->
			// dunning, on the membership's retry schedule.
			llog.Warn("Solana cranker: recoverable pull failure; routing to dunning")
			reason := crankErr.Error()
			code := string(cf.Code)
			subID := row.SubscriptionID
			if err := w.recordPullAttempt(ctx, row, plan, memoLocalID, code, reason); err != nil {
				return crankOutcome{}, err
			}
			if err := w.Lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
				Rail:            models.RailSolana,
				SubscriptionID:  &subID,
				FailureReason:   &reason,
				FailureCode:     &code,
				AttemptRecorded: true,
			}); err != nil {
				return crankOutcome{}, fmt.Errorf("solana crank: fail membership: %w", err)
			}
			// The next pull is the membership's next retry, which FailMembership
			// set from the case's dunning policy. None means that failure ended
			// it; one period on keeps this record from hot-looping meanwhile.
			failed, err := subscriptions.NewSubscriptionRepo(w.DB).GetByID(ctx, subID)
			if err != nil {
				return crankOutcome{}, fmt.Errorf("solana crank: reload membership: %w", err)
			}
			var nextRetry time.Time
			if failed.NextRetryAt != nil {
				nextRetry = failed.NextRetryAt.UTC()
			} else {
				periodHoursI64, err := safecast.Convert[int64](periodHours)
				if err != nil {
					return crankOutcome{}, fmt.Errorf("solana crank: period hours overflow: %w", err)
				}
				nextRetry = w.now().Add(time.Duration(periodHoursI64) * time.Hour)
			}
			if err := repo.SetNextPullAt(ctx, row.ID, nextRetry); err != nil {
				return crankOutcome{}, err
			}
			return crankOutcome{
				kind:     crankDunned,
				reason:   reason,
				evidence: map[string]any{"decline_code": code},
			}, nil
		}
	}

	if err := w.finalizePull(ctx, repo, row, plan, sig); err != nil {
		// The pull HAPPENED (sig is live on-chain); surface the signature so
		// the caller resolves via verification instead of a blind re-pull.
		return crankOutcome{signature: sig}, err
	}
	return crankOutcome{kind: crankSucceeded, signature: sig}, nil
}

// finalizePull is the renewal repair for a confirmed on-chain pull: renew the
// membership window (payment row, period advance, notifications; idempotent
// on the tx signature) and advance the row past the pulled period. Shared by
// the inline success path and the recorded-signature verify leg.
func (w *SolanaCrankWorker) finalizePull(ctx context.Context, repo solanaSubStore, row *models.SolanaSubscription, plan resolvedPlan, sig string) error {
	ctx = db.WithPSPID(ctx, row.PspID)
	periodHoursI64, err := safecast.Convert[int64](plan.periodHours)
	if err != nil {
		return fmt.Errorf("solana crank: period hours overflow: %w", err)
	}
	now := w.now()
	periodEnd := now.Add(time.Duration(periodHoursI64) * time.Hour)
	if err := w.Lifecycle.RenewMembership(ctx, &subscriptions.RenewMembershipParams{
		Rail:                  models.RailSolana,
		RailSubscriptionID:    row.SubscriptionPDA,
		TransactionID:         sig,
		Amount:                plan.fiatAmount,
		AmountProvided:        true,
		Currency:              plan.currency,
		CurrentPeriodStartsAt: &now,
		CurrentPeriodEndsAt:   &periodEnd,
		PaymentMetadata: map[string]any{
			"solana_subscriber_wallet": row.SubscriberWallet,
			"solana_subscription_pda":  row.SubscriptionPDA,
			"solana_token_mint":        row.Mint,
			"solana_token_amount":      plan.amountBaseUnits,
			"solana_recipient_wallet":  row.MerchantAddress,
		},
	}); err != nil {
		return fmt.Errorf("solana crank: renew membership: %w", err)
	}
	return repo.AdvanceAfterPull(ctx, row.ID, now, sig, periodEnd)
}

// recordPullAttempt records a refused on-chain pull as its cycle's attempt:
// the cycle's rebill, or a dunning retry after an earlier failure. It is keyed
// on the pull's durable intent (one per subscription and pull slot), so a
// crank retried before the lifecycle applied the failure records nothing new.
// The failpoint after it lets tests crash there.
func (w *SolanaCrankWorker) recordPullAttempt(ctx context.Context, row *models.SolanaSubscription, plan resolvedPlan, pullIntent uuid.UUID, code, text string) error {
	if plan.periodEnd == nil {
		return nil // no paid period: nothing came due
	}
	var intent *uuid.UUID
	if pullIntent != uuid.Nil {
		intent = &pullIntent
	}
	if err := attempts.Record(ctx, w.DB.Gen(ctx), attempts.Attempt{
		MerchantID: row.MerchantID, CustomerID: plan.customerID, PSPID: plan.pspID, Rail: string(models.RailSolana),
		Kind: attempts.RebillKind(false, plan.retryAttempts), Owner: attempts.OwnerOf(plan.policy),
		Answer: decline.Evidence{Code: code, Text: text}, Amount: plan.fiatAmount, Currency: plan.currency, At: w.now(),
		SubscriptionID: &row.SubscriptionID, Cycle: &attempts.Cycle{SubscriptionID: row.SubscriptionID, DueAt: *plan.periodEnd},
		ProviderIntentID: intent, Step: "pull",
	}); err != nil {
		return err
	}
	return failpoint.Hit(ctx, failpoint.Site{Point: failpoint.AfterAttempt, Kind: TypeSolanaPull, Operation: pullIntent, Subscription: row.SubscriptionID})
}

// resolvePlan loads the on-chain pull amount + period + fingerprint and the fiat
// amount/currency for the subscription's price.
func (w *SolanaCrankWorker) resolvePlan(ctx context.Context, row *models.SolanaSubscription) (resolvedPlan, error) {
	subRepo := subscriptions.NewSubscriptionRepo(w.DB)
	sub, err := subRepo.GetByID(ctx, row.SubscriptionID)
	if err != nil {
		return resolvedPlan{}, fmt.Errorf("solana crank: load subscription: %w", err)
	}

	// A due scheduled change applies before the pull plan resolves: OpenRails
	// itself picks the amount withdrawn on-chain.
	if err := w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := w.DB.NewWithPgxTx(tx)
		repo := subscriptions.NewSubscriptionRepo(d)
		locked, err := repo.GetByIDForUpdate(ctx, sub.ID)
		if err != nil {
			return err
		}
		change, err := subscriptions.PendingChange(ctx, d, locked.ID)
		if err != nil || !change.IsDue(w.now()) || change.FromPriceID != locked.PriceID {
			return err
		}
		price, err := catalog.NewPriceService(d).GetByID(ctx, change.PriceID)
		if err != nil {
			return err
		}
		locked.PriceID, locked.ProductID = price.ID, price.ProductID
		if err := repo.UpdateAt(ctx, locked, w.now()); err != nil {
			return err
		}
		sub = locked
		return subscriptions.ApplyChange(ctx, d, change.ID, w.now())
	}); err != nil {
		return resolvedPlan{}, fmt.Errorf("solana crank: apply the scheduled change: %w", err)
	}

	price, err := catalog.NewPriceService(w.DB).GetByID(ctx, sub.PriceID)
	if err != nil {
		return resolvedPlan{}, fmt.Errorf("solana crank: load price: %w", err)
	}
	retryAttempts := 0
	if sub.RetryAttempts != nil {
		retryAttempts = *sub.RetryAttempts
	}
	cfg := price.ForPSP(sub.PspID).PSPLinkForRail(models.RailSolana)
	if cfg == nil {
		return resolvedPlan{}, fmt.Errorf("solana crank: price %s has no solana rail config", price.ID)
	}
	amountBaseUnits, err := strconv.ParseUint(cfg["amount_base_units"], 10, 64)
	if err != nil || amountBaseUnits == 0 {
		return resolvedPlan{}, fmt.Errorf("solana crank: invalid amount_base_units for price %s", price.ID)
	}
	periodHours, err := strconv.ParseUint(cfg["period_hours"], 10, 64)
	if err != nil || periodHours == 0 {
		return resolvedPlan{}, fmt.Errorf("solana crank: invalid period_hours for price %s", price.ID)
	}
	fingerprint, _ := strconv.ParseInt(cfg["created_at"], 10, 64)
	return resolvedPlan{
		amountBaseUnits: amountBaseUnits,
		periodHours:     periodHours,
		fingerprint:     fingerprint,
		fiatAmount:      price.Amount,
		currency:        price.Currency,
		retryAttempts:   retryAttempts,
		customerID:      sub.CustomerID,
		pspID:           sub.PspID,
		policy:          sub.CollectionPolicy,
		periodEnd:       sub.CurrentPeriodEndsAt,
	}, nil
}
