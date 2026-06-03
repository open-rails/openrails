package riverjobs

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	dbrepo "github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const (
	// KindSolanaCrank is the recurring Solana pull ("cranking") job (#256). It is
	// the Solana analog of the NMI DunningWorker: each run charges every due
	// subscription.
	KindSolanaCrank = "billing.solana_crank"

	solanaCrankBatchSize = 200
)

// SolanaCrankArgs triggers a cranking run over all due Solana subscriptions.
type SolanaCrankArgs struct{}

func (SolanaCrankArgs) Kind() string { return KindSolanaCrank }

// solanaCranker is the on-chain pull surface (satisfied by *recurring.CrankService).
type solanaCranker interface {
	Crank(ctx context.Context, tenantID tenant.ID, sub *models.SolanaSubscription, amountBaseUnits uint64) (string, error)
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

// SolanaCrankWorker queries due Solana subscriptions and cranks each: pull the
// plan amount on-chain, then RenewMembership (extends the paid period + records
// the payment, idempotent on the tx signature) and advance next_pull_at. A failed
// pull routes to FailMembership -> the existing dunning state machine.
type SolanaCrankWorker struct {
	river.WorkerDefaults[SolanaCrankArgs]
	DB        *db.DB
	Config    *config.Config
	Clock     clockwork.Clock
	Cranker   solanaCranker
	Lifecycle membershipManager
	BatchSize int
}

func (SolanaCrankWorker) Kind() string { return KindSolanaCrank }

func (w *SolanaCrankWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *SolanaCrankWorker) Work(ctx context.Context, _ *river.Job[SolanaCrankArgs]) error {
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

func (w *SolanaCrankWorker) crankOne(ctx context.Context, repo *dbrepo.SolanaSubscriptionRepo, row *models.SolanaSubscription) error {
	tenantID := tenant.ID(row.TenantID)

	// Resolve the plan amount (token base units) + period + ghost-plan fingerprint
	// from the linked price's Solana processor config.
	amountBaseUnits, periodHours, fingerprint, fiatAmount, currency, err := w.resolvePlan(ctx, row)
	if err != nil {
		return err
	}

	// Ghost-plan guard: the plan was deleted + recreated at the same PDA. The
	// on-chain pull would fail PlanTermsMismatch; terminate this record.
	if fingerprint != 0 && row.PlanCreatedAtFingerprint != 0 && fingerprint != row.PlanCreatedAtFingerprint {
		log.WithContext(ctx).WithField("subscription_pda", row.SubscriptionPDA).
			Warn("Solana cranker: plan created_at fingerprint mismatch (ghost plan); expiring")
		return repo.SetStatus(ctx, row.ID, models.SolanaSubscriptionExpired)
	}

	sig, crankErr := w.Cranker.Crank(ctx, tenantID, row, amountBaseUnits)
	if crankErr != nil {
		// Operational failure (RPC/network, or the cranker wallet out of SOL gas):
		// retry on the next run, NEVER dun the subscriber (#257). A SOL-gas outage
		// hits a tenant's whole book at once, so dunning on it would wrongly
		// past-due every subscriber. Leave next_pull_at unchanged so it stays due.
		if recurring.IsOperationalFailure(crankErr) {
			log.WithContext(ctx).WithError(crankErr).WithField("subscription_pda", row.SubscriptionPDA).
				Warn("Solana cranker: operational pull failure; will retry next run (no dunning)")
			return crankErr
		}
		// Terminal failure (#265): the subscriber cancelled or revoked the
		// delegation directly on-chain (via a wallet/explorer, bypassing the app),
		// so the subscription is gone/inactive. Cancel immediately and STOP — never
		// dun, since dunning would burn ~15 days of pointless retries against a
		// subscription the user already killed.
		if recurring.IsTerminalFailure(crankErr) {
			log.WithContext(ctx).WithError(crankErr).WithField("subscription_pda", row.SubscriptionPDA).
				Warn("Solana cranker: terminal pull failure (out-of-band cancel/revoke); cancelling subscription (no dunning)")
			if err := repo.SetStatus(ctx, row.ID, models.SolanaSubscriptionCancelled); err != nil {
				return fmt.Errorf("solana crank: set cancelled status: %w", err)
			}
			subID := row.SubscriptionID
			proc := models.ProcessorSolana
			reason := crankErr.Error()
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
		}
		// Subscriber fault (insufficient USDC, revoked delegation, etc.) -> dunning.
		// Advance next_pull_at by the dunning interval so the next attempt aligns
		// with the dunning cadence instead of re-failing every hourly run (which
		// would collapse the multi-day dunning window to a few hours).
		reason := crankErr.Error()
		subID := row.SubscriptionID
		if err := w.Lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
			Processor:      models.ProcessorSolana,
			SubscriptionID: &subID,
			FailureReason:  &reason,
		}); err != nil {
			return fmt.Errorf("solana crank: fail membership: %w", err)
		}
		nextRetry := w.now().Add(subscriptions.DunningInterval)
		return repo.SetNextPullAt(ctx, row.ID, nextRetry)
	}

	now := w.now()
	periodEnd := now.Add(time.Duration(periodHours) * time.Hour)
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
func (w *SolanaCrankWorker) resolvePlan(ctx context.Context, row *models.SolanaSubscription) (amountBaseUnits uint64, periodHours uint64, fingerprint int64, fiatAmount int64, currency string, err error) {
	subRepo := dbrepo.NewSubscriptionRepo(w.DB)
	sub, err := subRepo.GetByID(ctx, row.SubscriptionID)
	if err != nil {
		return 0, 0, 0, 0, "", fmt.Errorf("solana crank: load subscription: %w", err)
	}
	price, err := catalog.NewPriceService(w.DB).GetByID(ctx, sub.PriceID)
	if err != nil {
		return 0, 0, 0, 0, "", fmt.Errorf("solana crank: load price: %w", err)
	}
	cfg := price.GetProcessorConfig(models.ProcessorSolana)
	if cfg == nil {
		return 0, 0, 0, 0, "", fmt.Errorf("solana crank: price %s has no solana processor config", price.ID)
	}
	amountBaseUnits, err = strconv.ParseUint(cfg["amount_base_units"], 10, 64)
	if err != nil || amountBaseUnits == 0 {
		return 0, 0, 0, 0, "", fmt.Errorf("solana crank: invalid amount_base_units for price %s", price.ID)
	}
	periodHours, err = strconv.ParseUint(cfg["period_hours"], 10, 64)
	if err != nil || periodHours == 0 {
		return 0, 0, 0, 0, "", fmt.Errorf("solana crank: invalid period_hours for price %s", price.ID)
	}
	fingerprint, _ = strconv.ParseInt(cfg["created_at"], 10, 64)
	return amountBaseUnits, periodHours, fingerprint, price.Amount, price.Currency, nil
}
