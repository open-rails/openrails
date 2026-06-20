package riverjobs

import (
	"context"
	"fmt"
	"strconv"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/billing/declinecode"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	dbrepo "github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
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
// *dbrepo.SolanaSubscriptionRepo). Extracted as an interface so the
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
	// cycleDays is the price's billing cycle in days (0 = unknown) and
	// retryAttempts the subscription's consecutive-failure count so far —
	// together they feed the cadence-relative dunning schedule (#359).
	cycleDays     int
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
	batch := w.BatchSize
	if batch <= 0 {
		batch = solanaCrankBatchSize
	}
	repo := dbrepo.NewSolanaSubscriptionRepo(w.DB)
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
		// Per-row isolation: a failure on one subscriber never aborts the batch.
		if err := w.crankOne(ctx, repo, row); err != nil {
			log.WithContext(ctx).WithError(err).WithField("subscription_pda", row.SubscriptionPDA).
				Warn("Solana cranker: subscription crank failed")
		}
	}
	return nil
}

func (w *SolanaCrankWorker) crankOne(ctx context.Context, repo solanaSubStore, row *models.SolanaSubscription) error {
	merchantID := merchant.ID(row.MerchantID)

	// Resolve the plan amount (token base units) + period + ghost-plan fingerprint
	// from the linked price's Solana processor config.
	resolve := w.resolvePlanFn
	if resolve == nil {
		resolve = w.resolvePlan
	}
	plan, err := resolve(ctx, row)
	if err != nil {
		return err
	}
	amountBaseUnits := plan.amountBaseUnits
	periodHours := plan.periodHours
	fingerprint := plan.fingerprint
	fiatAmount := plan.fiatAmount
	currency := plan.currency

	// Ghost-plan guard: the plan was deleted + recreated at the same PDA. The
	// on-chain pull would fail PlanTermsMismatch; terminate this record.
	if fingerprint != 0 && row.PlanCreatedAtFingerprint != 0 && fingerprint != row.PlanCreatedAtFingerprint {
		log.WithContext(ctx).WithField("subscription_pda", row.SubscriptionPDA).
			Warn("Solana cranker: plan created_at fingerprint mismatch (ghost plan); expiring")
		return repo.SetStatus(ctx, row.ID, models.SolanaSubscriptionExpired)
	}

	sig, crankErr := w.Cranker.Crank(ctx, merchantID, row, amountBaseUnits)
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
			return crankErr
		case declinecode.AlreadyPaid:
			// The period was already pulled on-chain but our DB did not record it
			// (the partial-failure window). Advance past this period so we neither
			// re-attempt nor dun; the reconcile worker (#258) repairs the ledger.
			llog.Warn("Solana cranker: period already paid on-chain (idempotent); advancing, ledger repair via reconcile (#258)")
			periodHoursI64, err := safecast.Convert[int64](periodHours)
			if err != nil {
				return fmt.Errorf("solana crank: period hours overflow: %w", err)
			}
			next := w.now().Add(time.Duration(periodHoursI64) * time.Hour)
			return repo.SetNextPullAt(ctx, row.ID, next)
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
				return fmt.Errorf("solana crank: set cancelled status: %w", err)
			}
			subID := row.SubscriptionID
			proc := models.ProcessorSolana
			reason := fmt.Sprintf("%s (%s)", crankErr.Error(), cf.Code)
			if err := w.Lifecycle.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
				Processor:      &proc,
				SubscriptionID: &subID,
				CancelType:     models.CancelTypeUser,
				CancelFeedback: &reason,
				RevokeAccess:   true,
			}); err != nil {
				return fmt.Errorf("solana crank: cancel membership: %w", err)
			}
			return nil
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
				Processor:      models.ProcessorSolana,
				SubscriptionID: &subID,
				FailureReason:  &reason,
				FailureCode:    &code,
			}); err != nil {
				return fmt.Errorf("solana crank: fail membership: %w", err)
			}
			// plan.retryAttempts was loaded BEFORE the FailMembership above
			// recorded this failure, so the schedule gap is looked up at +1.
			gap := subscriptions.DunningNextRetryIn(plan.cycleDays, plan.retryAttempts+1)
			if gap <= 0 {
				// That failure was terminal under the schedule (FailMembership
				// cancelled the membership); advance one period so this record
				// doesn't hot-loop while the cancellation settles.
				periodHoursI64, err := safecast.Convert[int64](periodHours)
				if err != nil {
					return fmt.Errorf("solana crank: period hours overflow: %w", err)
				}
				gap = time.Duration(periodHoursI64) * time.Hour
			}
			nextRetry := w.now().Add(gap)
			return repo.SetNextPullAt(ctx, row.ID, nextRetry)
		}
	}

	periodHoursI64, err := safecast.Convert[int64](periodHours)
	if err != nil {
		return fmt.Errorf("solana crank: period hours overflow: %w", err)
	}
	now := w.now()
	periodEnd := now.Add(time.Duration(periodHoursI64) * time.Hour)
	if err := w.Lifecycle.RenewMembership(ctx, &subscriptions.RenewMembershipParams{
		Processor:               models.ProcessorSolana,
		ProcessorSubscriptionID: row.SubscriptionPDA,
		TransactionID:           sig,
		Amount:                  fiatAmount,
		AmountProvided:          true,
		Currency:                currency,
		CurrentPeriodStartsAt:   &now,
		CurrentPeriodEndsAt:     &periodEnd,
	}); err != nil {
		return fmt.Errorf("solana crank: renew membership: %w", err)
	}
	return repo.AdvanceAfterPull(ctx, row.ID, now, sig, periodEnd)
}

// resolvePlan loads the on-chain pull amount + period + fingerprint and the fiat
// amount/currency for the subscription's price.
func (w *SolanaCrankWorker) resolvePlan(ctx context.Context, row *models.SolanaSubscription) (resolvedPlan, error) {
	subRepo := dbrepo.NewSubscriptionRepo(w.DB)
	sub, err := subRepo.GetByID(ctx, row.SubscriptionID)
	if err != nil {
		return resolvedPlan{}, fmt.Errorf("solana crank: load subscription: %w", err)
	}
	price, err := catalog.NewPriceService(w.DB).GetByID(ctx, sub.PriceID)
	if err != nil {
		return resolvedPlan{}, fmt.Errorf("solana crank: load price: %w", err)
	}
	retryAttempts := 0
	if sub.RetryAttempts != nil {
		retryAttempts = *sub.RetryAttempts
	}
	cfg := price.GetProcessorConfig(models.ProcessorSolana)
	if cfg == nil {
		return resolvedPlan{}, fmt.Errorf("solana crank: price %s has no solana processor config", price.ID)
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
		cycleDays:       subscriptions.BillingCycleDaysOf(price),
		retryAttempts:   retryAttempts,
	}, nil
}
