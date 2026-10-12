package destructive

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
)

type actionContextKey struct{}
type admissionsContextKey struct{}

// BindAdmissions preserves the shared service through internal provider work.
func BindAdmissions(ctx context.Context, a *Admissions) context.Context {
	return context.WithValue(ctx, admissionsContextKey{}, a)
}

// AdmissionsFromContext is the service bound by command admission or recovery.
func AdmissionsFromContext(ctx context.Context) *Admissions {
	a, _ := ctx.Value(admissionsContextKey{}).(*Admissions)
	return a
}

// BindAction carries the original admission into its internal consequences.
// Only command admission and provider recovery establish this context.
func BindAction(ctx context.Context, action Action) context.Context {
	return context.WithValue(ctx, actionContextKey{}, action)
}

// ActionFromContext returns the command owning this internal effect.
func ActionFromContext(ctx context.Context) (Action, bool) {
	action, ok := ctx.Value(actionContextKey{}).(Action)
	return action, ok && action.ID != uuid.Nil && action.MerchantID != uuid.Nil
}

// CommandFromContext binds safe command terms to the verified merchant invoker.
func CommandFromContext(ctx context.Context, kind Kind, target, key string, terms any) (Command, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return Command{}, err
	}
	actor, err := ActorFromContext(ctx)
	if err != nil {
		return Command{}, err
	}
	return Command{MerchantID: mid.UUID(), Kind: kind, Target: target, Key: key, Digest: Digest(terms), Units: 1, Actor: actor}, nil
}

// AdmitCommand reserves one command before domain locks are acquired. An
// internal consequence carries the original receipt instead of spending again.
func AdmitCommand(ctx context.Context, admissions *Admissions, command Command) (context.Context, Action, error) {
	if admissions == nil {
		return ctx, Action{}, errors.New("destructive command admission is not wired")
	}
	if prior, ok := ActionFromContext(ctx); ok {
		if prior.MerchantID != command.MerchantID {
			return ctx, Action{}, errors.New("destructive action merchant mismatch")
		}
		return ctx, prior, nil
	}
	action, err := admissions.Admit(ctx, command)
	if err != nil {
		return ctx, Action{}, err
	}
	return BindAction(BindAdmissions(ctx, admissions), action), action, nil
}

// RunLocal commits a local command and its outcome together, after the short
// independent admission transaction has released its lock. fn must contain
// database work only. A rolled-back callback proves no effect; a lost commit
// response remains uncertain and retains its admission for recovery.
func RunLocal(ctx context.Context, database *db.DB, admissions *Admissions, command Command, fn func(context.Context, pgx.Tx, *db.DB) (bool, error)) error {
	_, inherited := ActionFromContext(ctx)
	ctx, action, err := AdmitCommand(ctx, admissions, command)
	if err != nil {
		return err
	}
	if !inherited {
		action, err = admissions.Start(ctx, command.MerchantID, action.ID)
		if err != nil {
			return err
		}
		ctx = BindAction(ctx, action)
	}
	callbackCompleted := false
	err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		changed, err := fn(ctx, tx, database.NewWithPgxTx(tx))
		if err != nil {
			return err
		}
		if !inherited {
			outcome := OutcomeSucceeded
			if !changed && !action.Replayed && action.State != StateSucceeded {
				outcome = OutcomeNoEffect
			}
			if err := admissions.FinishTx(ctx, tx, action.MerchantID, action.ID, outcome, command.Target); err != nil {
				return err
			}
		}
		callbackCompleted = true
		return nil
	})
	if err == nil || inherited {
		return err
	}
	outcome := OutcomeNoEffect
	if callbackCompleted && !errors.Is(err, pgx.ErrTxCommitRollback) {
		outcome = OutcomeUncertain
	}
	writeCtx, cancel := db.DetachedWriteContext(ctx, 10*time.Second)
	defer cancel()
	return errors.Join(err, admissions.Finish(writeCtx, action.MerchantID, action.ID, outcome, ""))
}
