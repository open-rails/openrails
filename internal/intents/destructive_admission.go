package intents

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"time"
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

// startProtectedEffect is called immediately before the provider write. A
// canonical unsent refusal retains its reservation but must face holds again.
func startProtectedEffect(ctx context.Context, database *db.DB, in gen.BillingProviderIntent, now time.Time) (context.Context, error) {
	if in.DestructiveActionID == nil {
		if in.Origin == string(OriginAdmin) && staffDestructiveIntent(in.IntentType) {
			return ctx, errors.New("staff destructive operation has no admission")
		}
		return ctx, nil
	}
	admissions := destructive.AdmissionsFromContext(ctx)
	if admissions == nil {
		return ctx, errors.New("destructive effect admission is not wired")
	}
	action, ok := destructive.ActionFromContext(ctx)
	if !ok || action.ID != *in.DestructiveActionID {
		return ctx, errors.New("destructive effect has no original action")
	}
	var evidence struct {
		BeforeEffect bool `json:"destructive_before_effect"`
	}
	if len(in.ResultEvidence) > 0 && json.Unmarshal(in.ResultEvidence, &evidence) != nil {
		return ctx, errors.New("destructive effect evidence is malformed")
	}
	var err error
	if action.Kind != destructive.CancelSubscription && !cancelIntent(in.IntentType) {
		action, err = admissions.StartPending(ctx, in.MerchantID, action.ID)
	} else {
		action, err = admissions.Start(ctx, in.MerchantID, action.ID)
	}
	if err != nil {
		return ctx, err
	}
	if database == nil {
		return ctx, errors.New("destructive send fence has no database")
	}
	n, err := database.Gen(ctx).FenceDestructiveProviderSend(ctx, gen.FenceDestructiveProviderSendParams{MerchantID: in.MerchantID, ID: in.ID, DestructiveActionID: *in.DestructiveActionID, Attempts: in.Attempts, Now: now})
	if err != nil {
		return ctx, err
	}
	if n != 1 {
		return ctx, errors.New("destructive send fence lost its operation claim")
	}
	return destructive.BindAction(ctx, action), nil
}
func cancelIntent(kind string) bool {
	return kind == TypeNMIDeleteSubscription || kind == TypeCCBillCancelSubscription || kind == TypeStripeCancelSubscription
}
func parkedBeforeEffect(reason string) Outcome {
	out := Parked(reason)
	out.Evidence = map[string]any{"destructive_before_effect": true}
	return out
}

// startLocalCompletion is for a verified provider fact or a local-only detach.
// Recovery of an effect already sent never starts a replacement command.
func startLocalCompletion(ctx context.Context, in gen.BillingProviderIntent) (context.Context, error) {
	if in.DestructiveActionID == nil {
		return ctx, nil
	}
	admissions := destructive.AdmissionsFromContext(ctx)
	if admissions == nil {
		return ctx, errors.New("destructive local completion is not wired")
	}
	action, err := admissions.Start(ctx, in.MerchantID, *in.DestructiveActionID)
	if err != nil {
		return ctx, err
	}
	return destructive.BindAction(ctx, action), nil
}

// CompleteTerminalAction reconciles a receipt before a dispatcher retires an
// already-terminal provider job. In particular a resumed subscription can
// supersede its pending stop without taking admission locks inside resume.
func (r *Runner) CompleteTerminalAction(ctx context.Context, in gen.BillingProviderIntent) error {
	if in.DestructiveActionID == nil {
		return nil
	}
	if !OperationTerminal(in.Status) {
		return errors.New("destructive operation is not terminal")
	}
	if r.Admissions == nil {
		return errors.New("destructive terminal recovery is not wired")
	}
	action, err := r.Admissions.GetAction(ctx, in.MerchantID, *in.DestructiveActionID)
	if err != nil {
		return err
	}
	if action.State == destructive.StateSucceeded || action.State == destructive.StateNoEffect {
		return nil
	}
	outcome := destructive.OutcomeUncertain
	switch in.Status {
	case StatusSucceeded:
		outcome = destructive.OutcomeSucceeded
	case StatusSuperseded, StatusExpired:
		if action.State == destructive.StateReserved {
			outcome = destructive.OutcomeNoEffect
		} else if cancelIntent(in.IntentType) {
			outcome = destructive.OutcomeSucceeded
		}
	case StatusFailedTerminal:
		var evidence struct {
			NoEffect bool `json:"destructive_no_effect"`
		}
		if len(in.ResultEvidence) > 0 && json.Unmarshal(in.ResultEvidence, &evidence) != nil {
			return errors.New("destructive terminal evidence is malformed")
		}
		if evidence.NoEffect {
			outcome = destructive.OutcomeNoEffect
		}
	}
	return r.Admissions.Finish(ctx, in.MerchantID, action.ID, outcome, in.ID.String())
}
