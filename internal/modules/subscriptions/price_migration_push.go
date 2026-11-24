package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/alerting/alertjob"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// MovePush is what one push of a migration's move came to.
type MovePush struct {
	// Until is set when the move is not due yet: push again then.
	Until time.Time
	// Blocked is why the provider cannot take the move; the move is blocked.
	Blocked string
	// Resolution is what happened otherwise: pushed, applied, skipped (with
	// its reason) or the move's status when it had already resolved.
	Resolution string
}

// pushRefusal is a push the provider can never take as it stands: retrying
// it changes nothing, so the move blocks with Reason, a stable blocked reason.
type pushRefusal struct{ Reason, Detail string }

func (e *pushRefusal) Error() string {
	if e.Detail == "" {
		return e.Reason
	}
	return e.Reason + ": " + e.Detail
}

func refusePush(reason, detail string, args ...any) error {
	return &pushRefusal{Reason: reason, Detail: fmt.Sprintf(detail, args...)}
}

// MigrationBlockedFindingType is the finding a move blocked at its push
// raises, one per migration and reason: nothing retries it.
const MigrationBlockedFindingType = "life.price_migration.blocked"

// PushMove carries one scheduled move to its provider: the body of its
// price_migration_push operation. A move that no longer applies is skipped,
// one the provider already carries is applied, one before the subscription's
// last period before effective_at waits. An error is a push to retry.
func (s *PriceMigrationService) PushMove(ctx context.Context, changeID uuid.UUID) (MovePush, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return MovePush{}, err
	}
	row, err := s.db.Gen(ctx).GetScheduledChange(ctx, gen.GetScheduledChangeParams{MerchantID: mid.UUID(), ID: changeID})
	if err != nil {
		return MovePush{}, err
	}
	change := models.ScheduledChangeFromGen(row)
	if change.Status != models.ScheduledChangeScheduled {
		return MovePush{Resolution: string(change.Status)}, nil
	}
	sub, err := s.subscriptions.GetByID(ctx, change.SubscriptionID)
	if db.IsNotFound(err) {
		return s.skip(ctx, change, "the subscription is gone")
	}
	if err != nil {
		return MovePush{}, err
	}
	switch {
	case sub.Status != models.StatusActive && sub.Status != models.StatusPastDue && sub.Status != models.StatusAwaitingMethod:
		return s.skip(ctx, change, "the subscription is "+string(sub.Status))
	case sub.PriceID == change.PriceID:
		// The push landed before its database write.
		if err := s.resolveMove(ctx, change, func(ctx context.Context, d *db.DB) error {
			return ApplyChange(ctx, d, change.ID, s.now())
		}); err != nil {
			return MovePush{}, err
		}
		return MovePush{Resolution: "applied"}, nil
	case sub.PriceID != change.FromPriceID:
		return s.skip(ctx, change, "the subscription left the source price")
	case deferredPushRequired(change.EffectiveAt, sub):
		return MovePush{Until: *sub.CurrentPeriodEndsAt}, nil
	}
	source, err := s.prices.GetByID(ctx, change.FromPriceID)
	if db.IsNotFound(err) {
		return s.skip(ctx, change, "the source price is gone")
	}
	if err != nil {
		return MovePush{}, err
	}
	target, err := s.prices.GetByID(ctx, change.PriceID)
	if db.IsNotFound(err) {
		return s.skip(ctx, change, "the target price is gone")
	}
	if err != nil {
		return MovePush{}, err
	}
	if target.Archived {
		return s.skip(ctx, change, "the target price is archived")
	}
	product, err := catalog.NewProductService(s.db).GetByID(ctx, target.ProductID)
	if err != nil {
		return MovePush{}, err
	}
	capability, err := s.capability(ctx, sub)
	if err != nil {
		return MovePush{}, err
	}
	switch capability {
	case capabilityEngine:
		// The renewal applies it.
		return MovePush{Resolution: "engine"}, nil
	case capabilityUserAction:
		return s.block(ctx, change, sub, &pushRefusal{Reason: migrationBlockedRailUserAction, Detail: "no declared account reaches the subscription's provider"})
	}
	err = s.push(ctx, capability, sub, source, target, product, change)
	if refused := (*pushRefusal)(nil); errors.As(err, &refused) {
		return s.block(ctx, change, sub, refused)
	}
	if err != nil {
		return MovePush{}, err
	}
	return MovePush{Resolution: "pushed"}, nil
}

// skip and block end a move that will not happen, under the subscription's
// lock.
func (s *PriceMigrationService) skip(ctx context.Context, change *models.ScheduledChange, reason string) (MovePush, error) {
	err := s.resolveMove(ctx, change, func(ctx context.Context, d *db.DB) error {
		return resolved(ctx, d.Gen(ctx), s.now())(d.Gen(ctx).SkipScheduledChange(ctx, gen.SkipScheduledChangeParams{MerchantID: change.MerchantID, ID: change.ID, Reason: reason}))
	})
	return MovePush{Resolution: "skipped: " + reason}, err
}

// block ends a move its provider refuses for good and raises the
// migration's finding for that reason in the same transaction.
func (s *PriceMigrationService) block(ctx context.Context, change *models.ScheduledChange, sub *models.Subscription, refused *pushRefusal) (MovePush, error) {
	err := s.resolveMove(ctx, change, func(ctx context.Context, d *db.DB) error {
		q := d.Gen(ctx)
		if err := resolved(ctx, q, s.now())(q.BlockScheduledChange(ctx, gen.BlockScheduledChangeParams{MerchantID: change.MerchantID, ID: change.ID, Reason: refused.Reason})); err != nil {
			return err
		}
		return s.raiseMigrationBlocked(ctx, d, change, sub, refused)
	})
	return MovePush{Blocked: refused.Reason}, err
}

// raiseMigrationBlocked opens, or observes again, the finding of moves the
// migration's provider refused for one reason, and alerts the operator once.
func (s *PriceMigrationService) raiseMigrationBlocked(ctx context.Context, d *db.DB, change *models.ScheduledChange, sub *models.Subscription, refused *pushRefusal) error {
	if change.PriceMigrationID == nil {
		return nil
	}
	evidence, err := json.Marshal(map[string]any{
		"price_migration_id":   billing.PriceMigrationID(*change.PriceMigrationID).String(),
		"subscription_id":      billing.SubscriptionID(sub.ID).String(),
		"rail":                 string(sub.Rail),
		"psp_id":               billing.PSPID(sub.PspID).String(),
		"rail_subscription_id": sub.RailSubscriptionID,
		"reason":               refused.Reason,
		"detail":               refused.Detail,
	})
	if err != nil {
		return err
	}
	action := "The provider cannot move these subscriptions to the migration's price, and nothing retries it: they keep billing their old price. " + blockedRemedy(refused.Reason)
	row, err := d.Gen(ctx).UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID: change.MerchantID, FindingType: MigrationBlockedFindingType, SubjectKey: billing.PriceMigrationID(*change.PriceMigrationID).String() + "/" + refused.Reason,
		Severity: "medium", Status: "requires_review", RecommendedAction: &action, Evidence: evidence,
	})
	if err != nil {
		return fmt.Errorf("raise %s: %w", MigrationBlockedFindingType, err)
	}
	if err := alertjob.Notify(ctx, d, alertjob.FindingOf(row), s.now()); err != nil && ctx.Err() == nil {
		s.Log.WarnContext(ctx, "alerting the operator of a blocked price migration failed; the finding stays in the console", logx.Err(err), "price_migration_id", *change.PriceMigrationID)
	}
	return nil
}

func blockedRemedy(reason string) string {
	switch reason {
	case migrationBlockedNMINamedPlan:
		return "NMI changes a named plan's schedule only by moving it to another plan: change each subscription's plan at NMI, or keep it grandfathered."
	case migrationBlockedNMIPaymentsUnknown:
		return "NMI does not state the schedule's plan_payments, which an amount update must send: check the schedule at NMI."
	case migrationBlockedNMIScheduleMissing:
		return "NMI has no such schedule: check the subscription at NMI."
	case migrationBlockedRailUserAction:
		return "No declared account reaches the subscription's provider: declare it, then migrate these subscriptions again."
	case migrationBlockedMissingRailPrice:
		return "Map both prices to their Stripe prices, then migrate these subscriptions again."
	}
	return "Check the subscription at its provider, then migrate it again."
}

// resolveMove runs one transition of change under its subscription's lock; one
// that lost to another resolution is no error.
func (s *PriceMigrationService) resolveMove(ctx context.Context, change *models.ScheduledChange, transition func(context.Context, *db.DB) error) error {
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		if _, err := NewSubscriptionRepo(d).GetByIDForUpdate(ctx, change.SubscriptionID); err != nil && !db.IsNotFound(err) {
			return err
		}
		return transition(ctx, d)
	})
	if errors.Is(err, errChangeNotScheduled) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("price migration: resolve move %s: %w", change.ID, err)
	}
	return nil
}
