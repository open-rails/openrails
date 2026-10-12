package intents

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
)

// staffDestructiveIntent is the durable portion of a protected staff command.
// Customer and autonomous work continue through their existing policies.
func staffDestructiveIntent(kind string) bool {
	switch kind {
	case TypeNMIDeleteSubscription, TypeCCBillCancelSubscription, TypeStripeCancelSubscription,
		TypeNMIPaymentMethodDelete, TypeHyperSwitchMethodDelete, TypeNMIRefund, TypeStripeRefund, TypeCCBillRefund:
		return true
	}
	return false
}

// bindIntentAction restores accepted authority for background verification and
// required consequences; it does not mint a replacement actor or admission.
func (r *Runner) bindIntentAction(ctx context.Context, in gen.BillingProviderIntent) (context.Context, error) {
	if in.DestructiveActionID == nil {
		if in.Origin == string(OriginAdmin) && staffDestructiveIntent(in.IntentType) {
			return ctx, errors.New("staff destructive operation has no admission")
		}
		return ctx, nil
	}
	if r.Admissions == nil {
		return ctx, errors.New("destructive command recovery is not wired")
	}
	action, err := r.Admissions.GetAction(ctx, in.MerchantID, *in.DestructiveActionID)
	if err != nil {
		return ctx, err
	}
	return destructive.BindAction(destructive.BindAdmissions(ctx, r.Admissions), action), nil
}

// finishIntentActionTx commits receipt completion with the canonical intent
// outcome. Required descendants keep the original receipt unresolved.
func finishIntentActionTx(ctx context.Context, tx pgx.Tx, in gen.BillingProviderIntent, outcome destructive.Outcome) error {
	if in.DestructiveActionID == nil {
		return nil
	}
	admissions := destructive.AdmissionsFromContext(ctx)
	if admissions == nil {
		return errors.New("destructive command completion is not wired")
	}
	return admissions.FinishTx(ctx, tx, in.MerchantID, *in.DestructiveActionID, outcome, in.ID.String())
}

func (s *Store) finishIntentAction(ctx context.Context, id uuid.UUID, transition func(context.Context, *Store) error, outcome destructive.Outcome) error {
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		local := s.withTxDB(s.db.NewWithPgxTx(tx))
		if err := transition(ctx, local); err != nil {
			return err
		}
		row, err := local.Get(ctx, id)
		if err != nil {
			return err
		}
		return finishIntentActionTx(ctx, tx, row, outcome)
	})
}
