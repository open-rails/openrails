package subscriptions

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// ErrPriceMigrationInProgress answers a request identical to one still
// recording its migration.
var ErrPriceMigrationInProgress = apperr.New(http.StatusConflict, billing.CodePriceMigrationInProgress, "the same price migration is being created by another request; retry once it answers")

// errCommandClosed refuses to record a migration whose command already ended.
var errCommandClosed = apperr.Conflictf("the price migration's command has ended; create the migration again")

// admitMigration admits a migration's units: its subscribers counted and the
// sources it archives, before any is read. Holds and ceilings are checked
// here, once: a hold opened later stops no page, only a cancel does. The
// same request again shares its command: it answers the migration recorded
// under it, or records it when none was, or, while another request holds
// the command, ErrPriceMigrationInProgress, never waiting. The command is
// answered with a prior migration too, for a replay's archive.
func (s *PriceMigrationService) admitMigration(ctx context.Context, request billing.CreatePriceMigrationParams, plan *migrationPlan, units int, through uuid.UUID) (*destructive.Action, *billing.PriceMigration, error) {
	if s.admissions == nil {
		if units == 0 {
			return nil, nil, nil
		}
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
		if units == 0 {
			return nil, nil, nil
		}
		if action, err = s.admissions.Admit(ctx, destructive.Command{MerchantID: mid.UUID(), Kind: destructive.CatalogChange, Target: target,
			Key: "price-migration:" + destructive.Digest([]any{digest, plan.sourceIDs(), units, through}), Digest: digest, Units: int64(units), Actor: actor}); err != nil {
			return nil, nil, err
		}
	}
	if action.Replayed {
		state, prior, err := s.claim(ctx, action)
		if err != nil || prior != nil {
			return &action, prior, err
		}
		if state != destructive.StateReserved {
			if state != destructive.StateStarted {
				return nil, nil, errCommandClosed
			}
			return &action, nil, nil
		}
	}
	if action, err = s.admissions.Start(ctx, mid.UUID(), action.ID); err != nil {
		return nil, nil, err
	}
	return &action, nil, nil
}

// claim reads a command an identical request already admitted, without
// waiting on it: the migration recorded under it, or its state when none
// was and no one holds it, or ErrPriceMigrationInProgress when another
// request does. The admissions lock is never held while it looks.
func (s *PriceMigrationService) claim(ctx context.Context, action destructive.Action) (destructive.State, *billing.PriceMigration, error) {
	var state destructive.State
	var recorded uuid.UUID
	busy := false
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := s.db.NewWithPgxTx(tx).Gen(ctx)
		row, err := q.GetPriceMigrationByDestructiveAction(ctx, gen.GetPriceMigrationByDestructiveActionParams{MerchantID: action.MerchantID, DestructiveActionID: action.ID})
		if err == nil {
			recorded = row.ID
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		locked, err := q.LockDestructiveActionNowait(ctx, gen.LockDestructiveActionNowaitParams{MerchantID: action.MerchantID, ID: action.ID})
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "55P03" {
			busy = true
			return nil
		}
		if err != nil {
			return err
		}
		state = destructive.State(locked.State)
		return nil
	})
	switch {
	case err != nil:
		return "", nil, err
	case recorded != uuid.Nil:
		out, err := s.Get(ctx, billing.PriceMigrationID(recorded))
		return "", out, err
	case busy:
		return "", nil, ErrPriceMigrationInProgress
	}
	return state, nil, nil
}

// Abandon handles the command of a migration never recorded. One that
// archived a source stays started: the identical retry shares it, spending
// no new unit, and records the migration. One that changed nothing ends
// with no effect. A recording that committed though its answer was lost
// stands.
func (s *PriceMigrationService) Abandon(ctx context.Context, pending *PendingMigration, archived bool, cause error) error {
	if pending == nil || pending.Action == nil || archived {
		return cause
	}
	action := pending.Action
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
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
		return s.admissions.FinishTx(ctx, tx, action.MerchantID, action.ID, destructive.OutcomeNoEffect, "")
	})
	return errors.Join(cause, err)
}
