package subscriptions

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/merchant"
)

// admitMigration admits a migration of matched subscribers, one unit each,
// before any is read. The same request again answers the migration it
// recorded; one whose recording never committed is admitted again.
func (s *PriceMigrationService) admitMigration(ctx context.Context, request billing.CreatePriceMigrationParams, plan *migrationPlan, matched int, through uuid.UUID) (*destructive.Action, *billing.PriceMigration, error) {
	if matched == 0 {
		return nil, nil, nil
	}
	if s.admissions == nil {
		return nil, nil, errors.New("price migration destructive admission is not wired")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, nil, err
	}
	actor, err := destructive.ActorFromContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	target := "price-migration/" + plan.target.PublicID.String()
	digest := destructive.Digest(request)
	action, found, err := s.admissions.FindPending(ctx, mid.UUID(), target, actor, digest)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		if action, err = s.admissions.Admit(ctx, destructive.Command{MerchantID: mid.UUID(), Kind: destructive.CatalogChange, Target: target,
			Key: "price-migration:" + destructive.Digest([]any{digest, plan.sourceIDs(), matched, through}), Digest: digest, Units: int64(matched), Actor: actor}); err != nil {
			return nil, nil, err
		}
	}
	if action.Replayed {
		row, err := s.db.Gen(ctx).GetPriceMigrationByDestructiveAction(ctx, gen.GetPriceMigrationByDestructiveActionParams{MerchantID: mid.UUID(), DestructiveActionID: action.ID})
		if err == nil {
			out, err := s.Get(ctx, billing.PriceMigrationID(row.ID))
			return nil, out, err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, err
		}
	}
	if action, err = s.admissions.Start(ctx, mid.UUID(), action.ID); err != nil {
		return nil, nil, err
	}
	return &action, nil, nil
}

// abandonMigration ends an admission whose migration was never recorded:
// recording is one transaction with the first page, so nothing moved. A
// commit whose answer was lost recorded it, and stands.
func (s *PriceMigrationService) abandonMigration(ctx context.Context, action *destructive.Action, cause error) error {
	if action == nil {
		return cause
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, err := s.db.Gen(finish).GetPriceMigrationByDestructiveAction(finish, gen.GetPriceMigrationByDestructiveActionParams{MerchantID: action.MerchantID, DestructiveActionID: action.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		err = s.admissions.Finish(finish, action.MerchantID, action.ID, destructive.OutcomeNoEffect, "")
	}
	return errors.Join(cause, err)
}
