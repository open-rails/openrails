package subscriptions

// #816: the plan-migration re-driver — the background half that makes #813
// migrations fully automatic. Two blocked-row classes heal here, on time,
// with no operator involvement:
//
//  1. Deferred pushes: a far-future effective date blocked the rail push
//     (stripe_deferred_push_required / nmi_deferred_push_required) because
//     pushing early would flip a whole rebill period too soon. Each
//     subscription enters its final pre-effective period on its OWN schedule;
//     the re-driver fires the push inside that window.
//  2. Crash-window rows: a rail push that succeeded (NMI verified / Stripe
//     schedule created) followed by a DB failure left the rail on the target
//     with the row blocked. The re-drive converges — set-to-value re-pushes
//     are idempotent on both rails, and a subscription already carrying the
//     target internally just has its ledger row marked applied.
//
// The re-driver does EXACTLY what a manual re-run would, through the same
// executeScheduled paths and the same status-predicated transitions — no
// forked logic, no new state vocabulary. Concurrency with a manual re-run (or
// an overlapping tick) is safe by construction: the one-scheduled partial
// unique index makes Unblock lose loudly when another scheduled row exists,
// and every transition is status-predicated so exactly one actor wins.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PlanMigrationRedriveResult summarizes one re-driver pass.
type PlanMigrationRedriveResult struct {
	Examined int `json:"examined"`
	// Redriven: rows whose push (or convergence) succeeded this pass.
	Redriven int `json:"redriven"`
	// Converged: the Redriven subset healed WITHOUT a rail push — the
	// subscription already carried the target (crash-window rows).
	Converged int `json:"converged"`
	// Deferred: still waiting for the subscription's final pre-effective
	// period. Left blocked; a later pass picks them up.
	Deferred int `json:"deferred"`
	// Skipped: stale rows the re-driver must not touch (subscription gone,
	// cancelled, moved to a different price, target archived, or another
	// scheduled row owns the subscription).
	Skipped int `json:"skipped"`
	// Failed: the re-push failed again; the row is re-blocked with the fresh
	// reason and a later pass retries.
	Failed int `json:"failed"`
}

// RedriveBlocked re-drives blocked plan-change rows across every merchant that
// has one. batchSize bounds one pass (default 200).
//
// or#861: this used to read the ROWS deployment-wide off the base pool, which
// is not a cross-merchant read path — subscription_reprices FORCEs RLS, so the
// list came back empty and the #816 re-driver has never re-driven anything.
// The enumeration is now a SECURITY DEFINER work queue returning merchant IDS
// (migration 0022), and every row read, every rail push and every batch-header
// re-sync happens inside that merchant's own pinned scope. A definer must not
// vend whole merchant rows, and this way it does not have to.
func (s *PlanMigrationService) RedriveBlocked(ctx context.Context, batchSize int) (*PlanMigrationRedriveResult, error) {
	if batchSize <= 0 {
		batchSize = 200
	}
	merchantIDs, err := s.reprice.repo.ListRedrivableMerchants(ctx, batchSize)
	if err != nil {
		return nil, fmt.Errorf("redrive: list merchants with blocked plan changes: %w", err)
	}
	res := &PlanMigrationRedriveResult{}
	for _, mid := range merchantIDs {
		if res.Examined >= batchSize {
			break
		}
		remaining := batchSize - res.Examined
		if err := s.reprice.repo.db.RunInMerchantScope(ctx, merchant.ID(mid), "plan-migration re-driver",
			func(ctx context.Context) error { return s.redriveMerchant(ctx, res, remaining) },
		); err != nil {
			// Driver errors (DB unavailable mid-pass etc.) end the pass;
			// everything already re-driven stays done and the next tick
			// resumes — same partial-failure posture as Migrate itself.
			return res, err
		}
	}
	return res, nil
}

// redriveMerchant runs one merchant's re-drive inside its pinned scope: read
// the blocked rows, re-drive each, then re-sync every touched batch header.
func (s *PlanMigrationService) redriveMerchant(ctx context.Context, res *PlanMigrationRedriveResult, limit int) error {
	rows, err := s.reprice.repo.ListRedrivableBlockedPlanChanges(ctx, limit)
	if err != nil {
		return fmt.Errorf("redrive: list blocked plan changes: %w", err)
	}
	touchedBatches := map[uuid.UUID]struct{}{}
	for _, row := range rows {
		res.Examined++
		outcome, rerr := s.redriveRow(ctx, row)
		if rerr != nil {
			return fmt.Errorf("redrive row %s: %w", row.ID, rerr)
		}
		switch outcome {
		case redriveOutcomePushed:
			res.Redriven++
		case redriveOutcomeConverged:
			res.Redriven++
			res.Converged++
		case redriveOutcomeDeferred:
			res.Deferred++
		case redriveOutcomeSkipped:
			res.Skipped++
		case redriveOutcomeFailed:
			res.Failed++
		}
		if row.RepriceBatchID != nil && outcome != redriveOutcomeDeferred && outcome != redriveOutcomeSkipped {
			touchedBatches[*row.RepriceBatchID] = struct{}{}
		}
	}
	// Re-sync every touched batch header from its actual rows — the header
	// must always agree with the per-subscription ledger (#813 invariant).
	for batchID := range touchedBatches {
		scheduled, blocked, cerr := s.reprice.repo.CountBatchRows(ctx, batchID)
		if cerr != nil {
			return fmt.Errorf("redrive: count batch %s rows: %w", batchID, cerr)
		}
		if uerr := s.reprice.repo.UpdatePlanMigrationBatchCounts(ctx, batchID, scheduled, blocked); uerr != nil {
			return fmt.Errorf("redrive: sync batch %s counts: %w", batchID, uerr)
		}
	}
	return nil
}

type redriveOutcome int

const (
	redriveOutcomePushed redriveOutcome = iota
	redriveOutcomeConverged
	redriveOutcomeDeferred
	redriveOutcomeSkipped
	redriveOutcomeFailed
)

// redriveRow re-drives ONE blocked plan-change row under its merchant
// context. Returns a non-nil error only for driver failures (the row itself
// is left untouched then); rail failures re-block the row and return
// redriveOutcomeFailed.
func (s *PlanMigrationService) redriveRow(ctx context.Context, row *models.SubscriptionReprice) (redriveOutcome, error) {
	logger := log.WithContext(ctx).WithFields(log.Fields{
		"reprice_id":      row.ID,
		"subscription_id": row.SubscriptionID,
	})
	sub, err := s.reprice.subscriptions.GetByID(ctx, row.SubscriptionID)
	if err != nil || sub == nil {
		logger.Debug("redrive: subscription not found; leaving row blocked")
		return redriveOutcomeSkipped, nil
	}
	if sub.Status != models.StatusActive && sub.Status != models.StatusPastDue {
		// Left the migratable cohort (cancelled etc.) — the row stays blocked
		// as the honest ledger of what never happened.
		return redriveOutcomeSkipped, nil
	}

	// Crash-window convergence: the subscription already carries the target
	// internally (the NMI cutover's sub update landed but the row transition
	// died, or another actor migrated it). No push — just converge the ledger
	// row through the same scheduled->applied transition every other path
	// uses.
	if sub.PriceID == row.ToPriceID {
		if err := s.reprice.repo.Unblock(ctx, row.ID); err != nil {
			// Concurrent transition or another scheduled row owns the sub.
			return redriveOutcomeSkipped, nil //nolint:nilerr // skip is the contract
		}
		if err := s.reprice.repo.Apply(ctx, row.ID); err != nil {
			return redriveOutcomeSkipped, nil //nolint:nilerr // concurrent transition won
		}
		logger.Info("redrive: converged blocked row — subscription already on target")
		return redriveOutcomeConverged, nil
	}
	if sub.PriceID != row.FromPriceID {
		// The subscription moved somewhere else entirely; this row's migration
		// never happened and never will. Leave the honest blocked ledger row.
		return redriveOutcomeSkipped, nil
	}
	if err := s.reprice.scheduledConflict(ctx, sub.ID); err != nil {
		// Another scheduled row owns the subscription (a manual re-run got
		// here first). Its own path completes the migration.
		return redriveOutcomeSkipped, nil //nolint:nilerr // skip is the contract
	}
	if deferredPushRequired(row.EffectiveAt, sub) {
		return redriveOutcomeDeferred, nil
	}

	source, err := s.reprice.prices.GetByID(ctx, row.FromPriceID)
	if err != nil {
		return redriveOutcomeSkipped, nil //nolint:nilerr // price gone; leave blocked
	}
	target, err := s.reprice.prices.GetByID(ctx, row.ToPriceID)
	if err != nil {
		return redriveOutcomeSkipped, nil //nolint:nilerr // price gone; leave blocked
	}
	if target.Archived {
		// The operator archived the target since the migration was committed —
		// migrating onto a dead price would violate the same gate Migrate
		// enforces. Leave blocked for the operator.
		return redriveOutcomeSkipped, nil
	}
	targetProduct, err := s.reprice.products(ctx, target.ProductID)
	if err != nil {
		return redriveOutcomeSkipped, nil //nolint:nilerr // product gone; leave blocked
	}

	// Take ownership: blocked -> scheduled. The one-scheduled unique index
	// and the status predicate make this the concurrency gate — exactly one
	// actor (this tick, a parallel tick, a manual re-run) wins the row.
	if err := s.reprice.repo.Unblock(ctx, row.ID); err != nil {
		return redriveOutcomeSkipped, nil //nolint:nilerr // lost the race; skip
	}

	// Re-drive through the SAME per-rail execute path Migrate uses. Boundary
	// mode always: by re-drive time the operator's original Immediate intent
	// (not persisted, and stale by definition) must not manufacture an
	// off-schedule entitlement cutover — the renewal-boundary flip is the
	// forced-migration invariant-safe mode on every rail.
	req := &PlanMigrationRequest{
		SourcePriceID: source.ID,
		TargetPriceID: target.ID,
		EffectiveAt:   row.EffectiveAt,
	}
	if perr := s.executeScheduled(ctx, req, sub, source, target, targetProduct, row); perr != nil {
		if berr := s.reprice.repo.BlockScheduledReprice(ctx, row.ID, migrationBlockedRailPushFailed+": "+perr.Error()); berr != nil {
			return redriveOutcomeFailed, fmt.Errorf("re-block after failed push: %w", berr)
		}
		logger.WithError(perr).Warn("redrive: rail push failed again; row re-blocked")
		return redriveOutcomeFailed, nil
	}
	// The original Migrate never emitted this row's schedule-time disclosure
	// (rows block BEFORE the notification step) — emit it now that the
	// migration is genuinely in motion.
	s.reprice.emitPlanChangeNotification(ctx, sub, source, target, targetProduct, row.EffectiveAt)
	logger.WithField("effective_at", row.EffectiveAt.UTC().Format(time.RFC3339)).Info("redrive: rail push succeeded")
	return redriveOutcomePushed, nil
}
