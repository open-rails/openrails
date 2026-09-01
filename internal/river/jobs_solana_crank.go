package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/billing/declinecode"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/progress"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const (
	// KindSolanaCrank is the recurring Solana pull ("cranking") job (#256). It is
	// the Solana analog of the NMI DunningWorker: each run charges every due
	// subscription.
	KindSolanaCrank = "openrails.solana_crank"

	solanaCrankBatchSize = 200
)

// SolanaCrankArgs triggers a cranking run over all due Solana subscriptions.
type SolanaCrankArgs struct{}

func (SolanaCrankArgs) Kind() string { return KindSolanaCrank }

// solanaCranker is the on-chain pull surface (satisfied by *recurring.CrankService).
type solanaCranker interface {
	Crank(ctx context.Context, merchantID merchant.ID, sub *models.SolanaSubscription, amountBaseUnits uint64) (string, error)
}

// presubmitCranker is the optional signature write-ahead capability (#674):
// presubmit(sig) persists the signed tx signature BEFORE submission.
// memoLocalID (the durable pull-intent id; Nil = unstamped) is the #713
// self-recognition memo stamped on the pull tx.
// *recurring.CrankService implements it; fakes without it skip the write-ahead.
type presubmitCranker interface {
	CrankWithPresubmit(ctx context.Context, merchantID merchant.ID, sub *models.SolanaSubscription, amountBaseUnits uint64, memoLocalID uuid.UUID, presubmit func(signature string) error) (string, error)
}

// membershipManager is the lifecycle surface the cranker drives (satisfied by
// *subscriptions.SubscriptionLifecycleService): renew on a confirmed pull, fail
// (-> dunning) on a charge failure.
type membershipManager interface {
	RenewMembership(ctx context.Context, params *subscriptions.RenewMembershipParams) error
	FailMembership(ctx context.Context, params *subscriptions.FailMembershipParams) error
	// CancelMembership terminates the subscription (used for out-of-band cancels
	// detected at rebill time, #265: subscriber revoked the delegation on-chain).
	CancelMembership(ctx context.Context, params *subscriptions.CancelMembershipParams) error
}

// solanaSubStore is the persistence surface crankOne mutates (satisfied by
// *solanasubs.SolanaSubscriptionRepo). Extracted as an interface so the
// state-machine/scheduling logic can be exercised by a fake in fast, network-
// free unit tests (#275) while production keeps using the real repo.
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
	// cycleHours is the price's billing cycle in hours (0 = unknown) and
	// retryAttempts the subscription's consecutive-failure count so far —
	// together they feed the cadence-relative dunning schedule (#359).
	cycleHours    int
	retryAttempts int
}

// SolanaCrankWorker queries due Solana subscriptions and cranks each: pull the
// plan amount on-chain, then RenewMembership (extends the paid period + records
// the payment, idempotent on the tx signature) and advance next_pull_at. A failed
// pull routes to FailMembership -> the existing dunning state machine.
//
// MISSED PERIODS ARE NEVER BACK-BILLED. If the cranker was down (outage,
// mode=limited/readonly, #345/#346) across one or more whole periods, resuming produces
// exactly ONE pull per subscription, and the new period anchors at the pull
// moment (RenewMembership gets CurrentPeriodStartsAt=now, not the lapsed
// boundary): one due row -> one pull -> next_pull_at = now + period. There is
// deliberately no catch-up loop charging elapsed periods. The on-chain program
// independently enforces the same bound — the subscriber's delegate approval
// authorizes ONE plan-amount per period (a second pull in the same period
// fails Custom:400 "period already paid"), and missed periods do not bank up
// into a withdrawable balance — so even a buggy cranker cannot collect more
// than the current period. Access stays fair: the entitlement lapsed with the
// unpaid period, so the subscriber pays one period and receives one period.
type SolanaCrankWorker struct {
	river.WorkerDefaults[SolanaCrankArgs]
	DB        *db.DB
	Config    *config.Config
	Clock     clockwork.Clock
	Cranker   solanaCranker
	Lifecycle membershipManager
	BatchSize int
	// Intents is the write-through provider-intent runner (#674): each due row
	// posts a durable solana_pull intent (keyed on the persisted next_pull_at)
	// and executes it inline through SolanaPullIntentHandler; ambiguity resolves
	// via the recorded pre-submit signature instead of a blind re-pull.
	//
	// or#893: REQUIRED by Work. It used to be optional, with a "legacy direct
	// crank (unit-test harnesses only)" branch below — an unstamped pull with no
	// durable record, so a crash between signing and finalize left a subscriber
	// charged and unrenewed with nothing to recover from. Tests drive crankOne
	// (the production seam the intent handler itself calls) instead. The one
	// legitimate runner-less SolanaCrankWorker is the intent HANDLER's own core:
	// it is never registered as a River worker, so Work never runs on it.
	Intents *intents.Runner

	// resolvePlanFn loads the billing terms for a row. nil in production (the
	// DB-backed w.resolvePlan is used); tests inject a fake to drive crankOne
	// without a database (#275).
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
	if w.Config != nil && w.Config.IsLimitedMode() {
		log.WithContext(ctx).Warn("limited mode: skipping Solana recurring pulls (#345)")
		return nil
	}
	if w.Cranker == nil || w.Lifecycle == nil {
		log.WithContext(ctx).Warn("Solana cranker not fully wired (no cranker/lifecycle); skipping run")
		return nil
	}
	// or#893: a pull without a durable intent is a charge nothing can recover.
	// This is a wiring defect, not a runtime condition — fail the job loudly
	// rather than quietly pulling money on the unrecoverable path.
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

	for _, row := range due {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		progress.Mark(ctx, "solana crank subscription "+row.ID.String())
		// Per-row isolation: a failure on one subscriber never aborts the batch.
		//
		// #674 write-through: durable intent first (keyed on the persisted
		// next_pull_at anchor), inline execution, pre-submit signature
		// write-ahead. Crash at any point ⇒ verify-then-resolve off the
		// recorded signature, never a paid-but-unrenewed subscriber.
		if _, err := w.Intents.EnqueueAndExecute(ctx, intents.EnqueueParams{
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
		}); err != nil {
			log.WithContext(ctx).WithError(err).WithField("subscription_pda", row.SubscriptionPDA).
				Warn("Solana cranker: post pull intent failed")
		}
	}
	return nil
}

// crankKind classifies a completed (error-free) crankOne for the intent
// ledger (#674).
type crankKind int

const (
	crankSucceeded    crankKind = iota // pulled + renewed + advanced
	crankAlreadyPaid                   // period already paid on-chain; advanced without local renewal
	crankCancelled                     // delegate revoked → membership cancelled (no dunning)
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
// durably records the signed tx signature BEFORE submission (#674) when the
// cranker supports it. memoLocalID (the pull intent id; Nil = unstamped) is
// stamped on the pull tx as the #713 self-recognition SPL Memo — it exists
// durably BEFORE the send, so a chain-side reader can resolve the tx back to
// the intent (and through it the subscription) with no local send record.
// The returned error means the pull is UNRESOLVED
// (operational failure, or a pull that happened but whose local finalize
// failed — the caller distinguishes via the recorded signature); a nil error
// means the state machine resolved the period (see crankKind).
func (w *SolanaCrankWorker) crankOne(ctx context.Context, repo solanaSubStore, row *models.SolanaSubscription, memoLocalID uuid.UUID, presubmit func(signature string) error) (crankOutcome, error) {
	merchantID := merchant.ID(row.MerchantID)

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
		// One classifier maps the on-chain error onto the shared billing
		// decline-code vocabulary + the action to take (#270). Codes confirmed on
		// devnet (#263): Custom:400 = period already paid (idempotent); token
		// OwnerMismatch (Custom:4) = subscriber revoked the SPL delegate (terminal);
		// token InsufficientFunds (Custom:1) = recoverable; RPC/gas = operational.
		cf := recurring.ClassifyCrankError(crankErr)
		llog := log.WithContext(ctx).WithError(crankErr).WithFields(log.Fields{
			"subscription_pda": row.SubscriptionPDA,
			"decline_code":     string(cf.Code),
			"onchain_code":     cf.OnChainCode,
		})
		switch cf.Category {
		case declinecode.Operational:
			// RPC/network or the cranker wallet out of SOL gas: retry next run, NEVER
			// dun — a shared outage would wrongly past-due a merchant's whole book.
			// Leave next_pull_at unchanged so it stays due.
			llog.Warn("Solana cranker: operational pull failure; retry next run (no dunning)")
			return crankOutcome{}, crankErr
		case declinecode.AlreadyPaid:
			// The period was already pulled on-chain but our DB did not record it
			// (the partial-failure window). Advance past this period so we neither
			// re-attempt nor dun; the reconcile worker (#258) repairs the ledger.
			// (#674: an intent-driven crank whose OWN recorded signature landed
			// never reaches here — the handler repairs the renewal via the
			// signature BEFORE re-cranking.)
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
		case declinecode.Terminal:
			// The subscriber revoked the SPL token delegate on-chain — transfer_subscription
			// can no longer move funds. Mirror it: cancel + stop, never dun. NOTE: a
			// plain cancel_subscription does NOT reach here (it stops FUTURE-period
			// pulls but not the current period, so it produces no crank error this
			// period, #263); the standard user cancel is an immediate on-chain
			// cancel_subscription the user signs, which OpenRails then mirrors here/at
			// confirm. Solana never uses a scheduled-cancel (the card "cancel at period
			// end" deferral) — Solana cancels are immediate and on-chain.
			llog.Warn("Solana cranker: terminal pull failure (delegate revoked); cancelling subscription (no dunning)")
			if err := repo.SetStatus(ctx, row.ID, models.SolanaSubscriptionCancelled); err != nil {
				return crankOutcome{}, fmt.Errorf("solana crank: set cancelled status: %w", err)
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
				kind:     crankCancelled,
				reason:   reason,
				evidence: map[string]any{"decline_code": string(cf.Code)},
			}, nil
		default:
			// Recoverable subscriber decline (insufficient USDC, etc.) -> dunning.
			// Advance next_pull_at by the cadence-relative dunning interval
			// (#359, derived from the price's billing cycle) so we align with
			// the dunning cadence instead of re-failing every hourly run.
			llog.Warn("Solana cranker: recoverable pull failure; routing to dunning")
			reason := crankErr.Error()
			code := string(cf.Code)
			subID := row.SubscriptionID
			if err := w.Lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
				Rail:                models.RailSolana,
				SubscriptionID:      &subID,
				FailureReason:       &reason,
				FailureCode:         &code,
				RecordFailedAttempt: true,
			}); err != nil {
				return crankOutcome{}, fmt.Errorf("solana crank: fail membership: %w", err)
			}
			// plan.retryAttempts was loaded BEFORE the FailMembership above
			// recorded this failure, so the schedule gap is looked up at +1.
			gap := collection.NextRetryIn(plan.cycleHours, plan.retryAttempts+1)
			if gap <= 0 {
				// That failure was terminal under the schedule (FailMembership
				// cancelled the membership); advance one period so this record
				// doesn't hot-loop while the cancellation settles.
				periodHoursI64, err := safecast.Convert[int64](periodHours)
				if err != nil {
					return crankOutcome{}, fmt.Errorf("solana crank: period hours overflow: %w", err)
				}
				gap = time.Duration(periodHoursI64) * time.Hour
			}
			nextRetry := w.now().Add(gap)
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

// finalizePull is the renewal repair for a CONFIRMED on-chain pull: renew the
// membership window (payment row, period advance, notifications — idempotent
// on the tx signature) and advance the row past the pulled period. Shared by
// the inline success path and the #674 recorded-signature verify leg.
func (w *SolanaCrankWorker) finalizePull(ctx context.Context, repo solanaSubStore, row *models.SolanaSubscription, plan resolvedPlan, sig string) error {
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

// resolvePlan loads the on-chain pull amount + period + fingerprint and the fiat
// amount/currency for the subscription's price.
func (w *SolanaCrankWorker) resolvePlan(ctx context.Context, row *models.SolanaSubscription) (resolvedPlan, error) {
	subRepo := subscriptions.NewSubscriptionRepo(w.DB)
	sub, err := subRepo.GetByID(ctx, row.SubscriptionID)
	if err != nil {
		return resolvedPlan{}, fmt.Errorf("solana crank: load subscription: %w", err)
	}

	// #773: pick up a due scheduled reprice BEFORE resolving the on-chain pull
	// plan below, so a scheduled price change actually changes what gets
	// pulled (Solana is engine-decided: unlike NMI/Stripe's own recurring
	// engines, OpenRails itself picks the amount to withdraw on-chain).
	repriceRepo := subscriptions.NewRepriceRepo(w.DB)
	scheduledReprice, repriceErr := repriceRepo.GetScheduledForSubscription(ctx, sub.ID)
	switch {
	case repriceErr == nil && scheduledReprice.IsDue(w.now()):
		sub.PriceID = scheduledReprice.ToPriceID
		if err := subRepo.Update(ctx, sub); err != nil {
			return resolvedPlan{}, fmt.Errorf("solana crank: re-pin repriced subscription: %w", err)
		}
		if err := repriceRepo.Apply(ctx, scheduledReprice.ID); err != nil && !errors.Is(err, subscriptions.ErrRepriceNotScheduled) {
			return resolvedPlan{}, fmt.Errorf("solana crank: mark reprice applied: %w", err)
		}
	case repriceErr != nil && !errors.Is(repriceErr, pgx.ErrNoRows):
		return resolvedPlan{}, fmt.Errorf("solana crank: check scheduled reprice: %w", repriceErr)
	}

	price, err := catalog.NewPriceService(w.DB).GetByID(ctx, sub.PriceID)
	if err != nil {
		return resolvedPlan{}, fmt.Errorf("solana crank: load price: %w", err)
	}
	retryAttempts := 0
	if sub.RetryAttempts != nil {
		retryAttempts = *sub.RetryAttempts
	}
	cfg := price.PSPLinkForRail(models.RailSolana)
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
		cycleHours:      collection.BillingCycleHoursOf(price),
		retryAttempts:   retryAttempts,
	}, nil
}
