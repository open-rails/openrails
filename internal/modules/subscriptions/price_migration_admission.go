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

// admitMigration admits a migration's units: its subscribers counted and the
// sources it archives, before any is read. Holds and ceilings are checked
// here, once: a hold opened later stops no page, only a cancel does. The
// same request again answers the migration it recorded; one whose
// recording never committed is admitted again.
func (s *PriceMigrationService) admitMigration(ctx context.Context, request billing.CreatePriceMigrationParams, plan *migrationPlan, units int, through uuid.UUID) (*destructive.Action, *billing.PriceMigration, error) {
	if units == 0 {
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
			Key: "price-migration:" + destructive.Digest([]any{digest, plan.sourceIDs(), units, through}), Digest: digest, Units: int64(units), Actor: actor}); err != nil {
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

// Abandon ends the command of a migration never recorded: no effect, unless
// it archived a source. A recording that committed though its answer was
// lost stands.
func (s *PriceMigrationService) Abandon(ctx context.Context, pending *PendingMigration, archived bool, cause error) error {
	if pending == nil || pending.Action == nil {
		return cause
	}
	action := pending.Action
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	outcome := destructive.OutcomeNoEffect
	if archived {
		outcome = destructive.OutcomeSucceeded
	}
	err := s.db.MerchantTx(finish, func(ctx context.Context, tx pgx.Tx) error {
		q := s.db.NewWithPgxTx(tx).Gen(ctx)
		locked, err := q.LockDestructiveAction(ctx, gen.LockDestructiveActionParams{MerchantID: action.MerchantID, ID: action.ID})
		if err != nil {
			return err
		}
		if locked.State != string(destructive.StateStarted) && locked.State != string(destructive.StateReserved) {
			return nil
		}
		if _, err := q.GetPriceMigrationByDestructiveAction(ctx, gen.GetPriceMigrationByDestructiveActionParams{MerchantID: action.MerchantID, DestructiveActionID: action.ID}); !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return s.admissions.FinishTx(ctx, tx, action.MerchantID, action.ID, outcome, "")
	})
	return errors.Join(cause, err)
}
