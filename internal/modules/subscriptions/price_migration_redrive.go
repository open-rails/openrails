package subscriptions

// The re-driver retries a price migration's failed pushes on time:
//   - a deferred push (an effective date past the current period) once the
//     subscription is in its last period before it;
//   - a push that reached the provider before a database failure: the
//     subscription already carries the target, so the move is just applied.
// It runs the same push as Create, through the same status-predicated
// transitions; the one-scheduled index lets exactly one actor win a move.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// PriceMigrationRedriveResult summarizes one re-driver pass.
type PriceMigrationRedriveResult struct {
	Examined int `json:"examined"`
	// Redriven: moves whose push (or convergence) succeeded.
	Redriven int `json:"redriven"`
	// Converged: the Redriven moves whose subscription already carried the
	// target.
	Converged int `json:"converged"`
	// Deferred: still before the subscription's last period.
	Deferred int `json:"deferred"`
	// Skipped: moves that no longer apply (subscription ended or moved,
	// target archived, another change scheduled).
	Skipped int `json:"skipped"`
	// Failed: the push failed again; a later pass retries.
	Failed int `json:"failed"`
}

// RedriveBlocked re-drives push-failed moves across the merchants holding
// one; batchSize bounds a pass (default 200). Each merchant's moves are read
// and pushed inside its own scope.
func (s *PriceMigrationService) RedriveBlocked(ctx context.Context, batchSize int) (*PriceMigrationRedriveResult, error) {
	if batchSize <= 0 {
		batchSize = 200
	}
	limit := int32(batchSize) // #nosec G115 -- a small batch size
	merchantIDs, err := s.db.GenDirectory().ListRedrivableScheduledChangeMerchants(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("redrive: list merchants: %w", err)
	}
	res := &PriceMigrationRedriveResult{}
	for _, mid := range merchantIDs {
		if res.Examined >= batchSize {
			break
		}
		remaining := batchSize - res.Examined
		if err := s.db.RunInMerchantScope(ctx, billing.MerchantID(mid), "price-migration re-driver",
			func(ctx context.Context) error { return s.redriveMerchant(ctx, res, remaining) },
		); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (s *PriceMigrationService) redriveMerchant(ctx context.Context, res *PriceMigrationRedriveResult, limit int) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	rows, err := s.db.Gen(ctx).ListRedrivableScheduledChanges(ctx, gen.ListRedrivableScheduledChangesParams{MerchantID: mid.UUID(), BatchSize: int32(limit)}) // #nosec G115 -- bounded by batchSize
	if err != nil {
		return fmt.Errorf("redrive: list moves: %w", err)
	}
	for _, change := range models.ScheduledChangesFromGen(rows) {
		res.Examined++
		outcome, err := s.redriveChange(ctx, change)
		if err != nil {
			return fmt.Errorf("redrive %s: %w", change.ID, err)
		}
		switch outcome {
		case redrivePushed:
			res.Redriven++
		case redriveConverged:
			res.Redriven++
			res.Converged++
		case redriveDeferred:
			res.Deferred++
		case redriveSkipped:
			res.Skipped++
		case redriveFailed:
			res.Failed++
		}
	}
	return nil
}

type redriveOutcome int

const (
	redrivePushed redriveOutcome = iota
	redriveConverged
	redriveDeferred
	redriveSkipped
	redriveFailed
)

// unblock takes a blocked move back to scheduled under the subscription's
// lock; false when another actor won it or another change is scheduled.
func (s *PriceMigrationService) unblock(ctx context.Context, change *models.ScheduledChange) bool {
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		if _, err := NewSubscriptionRepo(d).GetByIDForUpdate(ctx, change.SubscriptionID); err != nil {
			return err
		}
		return changed(d.Gen(ctx).UnblockScheduledChange(ctx, gen.UnblockScheduledChangeParams{MerchantID: change.MerchantID, ID: change.ID}))
	})
	return err == nil
}

// redriveChange errs only for driver failures; a failed push re-blocks the
// move and answers redriveFailed.
func (s *PriceMigrationService) redriveChange(ctx context.Context, change *models.ScheduledChange) (redriveOutcome, error) {
	logger := log.WithContext(ctx).WithFields(log.Fields{"scheduled_change_id": change.ID, "subscription_id": change.SubscriptionID})
	sub, err := s.subscriptions.GetByID(ctx, change.SubscriptionID)
	if err != nil || sub == nil {
		return redriveSkipped, nil //nolint:nilerr // the move stays blocked
	}
	if sub.Status != models.StatusActive && sub.Status != models.StatusPastDue && sub.Status != models.StatusAwaitingMethod {
		return redriveSkipped, nil
	}
	if sub.PriceID == change.PriceID {
		// The push landed before the database write: apply the move.
		if !s.unblock(ctx, change) {
			return redriveSkipped, nil
		}
		if err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return ApplyChange(ctx, s.db.NewWithPgxTx(tx), change.ID, s.now())
		}); err != nil {
			return redriveSkipped, nil //nolint:nilerr // a concurrent transition won
		}
		logger.Info("redrive: subscription already on the target; move applied")
		return redriveConverged, nil
	}
	if sub.PriceID != change.FromPriceID {
		return redriveSkipped, nil
	}
	if pending, err := PendingChange(ctx, s.db, sub.ID); err != nil || pending != nil {
		return redriveSkipped, nil //nolint:nilerr // another change owns the subscription
	}
	if deferredPushRequired(change.EffectiveAt, sub) {
		return redriveDeferred, nil
	}
	source, err := s.prices.GetByID(ctx, change.FromPriceID)
	if err != nil {
		return redriveSkipped, nil //nolint:nilerr // price gone; stays blocked
	}
	target, err := s.prices.GetByID(ctx, change.PriceID)
	if err != nil || target.Archived {
		return redriveSkipped, nil //nolint:nilerr // a dead target stays blocked
	}
	targetProduct, err := catalog.NewProductService(s.db).GetByID(ctx, target.ProductID)
	if err != nil {
		return redriveSkipped, nil //nolint:nilerr // product gone; stays blocked
	}
	if !s.unblock(ctx, change) {
		return redriveSkipped, nil
	}
	change.Status, change.BlockedReason = models.ScheduledChangeScheduled, ""
	if err := s.push(ctx, s.capability(ctx, sub), sub, source, target, targetProduct, change); err != nil {
		if berr := s.block(ctx, change.ID, migrationBlockedRailPushFailed+": "+err.Error()); berr != nil {
			return redriveFailed, fmt.Errorf("re-block after failed push: %w", berr)
		}
		logger.WithError(err).Warn("redrive: push failed again")
		return redriveFailed, nil
	}
	// Create never noticed a move that blocked before its push.
	s.notify(ctx, sub, source, target, targetProduct, change.EffectiveAt)
	logger.WithField("effective_at", change.EffectiveAt.UTC().Format(time.RFC3339)).Info("redrive: push succeeded")
	return redrivePushed, nil
}
