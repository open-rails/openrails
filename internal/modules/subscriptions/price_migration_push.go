package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
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

// errPushRefused is a push the provider cannot take as configured.
var errPushRefused = errors.New(migrationBlockedRailPushFailed)

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
	capability := s.capability(ctx, sub)
	switch capability {
	case capabilityEngine:
		// The renewal applies it.
		return MovePush{Resolution: "engine"}, nil
	case capabilityUserAction:
		return MovePush{}, errors.New("no declared account reaches the subscription's provider")
	}
	err = s.push(ctx, capability, sub, source, target, product, change)
	if errors.Is(err, errPushRefused) {
		return s.block(ctx, change, err.Error())
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

func (s *PriceMigrationService) block(ctx context.Context, change *models.ScheduledChange, reason string) (MovePush, error) {
	err := s.resolveMove(ctx, change, func(ctx context.Context, d *db.DB) error {
		return resolved(ctx, d.Gen(ctx), s.now())(d.Gen(ctx).BlockScheduledChange(ctx, gen.BlockScheduledChangeParams{MerchantID: change.MerchantID, ID: change.ID, Reason: reason}))
	})
	return MovePush{Blocked: reason}, err
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
