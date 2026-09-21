package checkout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// tierCompletion is private, transaction-bound authority for one accepted tier
// result. Preparation follows the subscription lock and precedes all domain
// writes; its operation lock remains held through the final commit.
type tierCompletion struct {
	db        *db.DB
	current   gen.OpenrailsRailIntent
	outcome   intents.Outcome
	now       time.Time
	committed bool
}

func prepareTierCompletion(ctx context.Context, d *db.DB, in gen.OpenrailsRailIntent, outcome intents.Outcome, now time.Time) (*tierCompletion, error) {
	if d == nil || d.Pool() != nil {
		return nil, errors.New("tier completion requires a transaction")
	}
	if outcome.Class != intents.OutcomeSucceeded && outcome.Class != intents.OutcomeTerminal {
		return nil, errors.New("tier completion requires a terminal outcome")
	}
	current, err := d.Gen(ctx).LockRailIntentForTierCompletion(ctx, gen.LockRailIntentForTierCompletionParams{ID: in.ID, MerchantID: in.MerchantID})
	if err != nil {
		return nil, err
	}
	if current.IntentType != in.IntentType || current.Rail != in.Rail || current.IdempotencyKey != in.IdempotencyKey || current.Origin != in.Origin || !reflect.DeepEqual(current.Actor, in.Actor) || !reflect.DeepEqual(current.CustodianID, in.CustodianID) || !reflect.DeepEqual(current.PspID, in.PspID) || !reflect.DeepEqual(current.SubscriptionID, in.SubscriptionID) || !reflect.DeepEqual(current.PriceID, in.PriceID) || !bytes.Equal(current.Payload, in.Payload) {
		return nil, errors.New("tier completion no longer names the accepted operation")
	}
	expected, err := json.Marshal(outcome.Evidence)
	if err != nil {
		return nil, err
	}
	switch current.IntentType {
	case subscriptions.TypeNMIUpgrade:
		err = validateNMITierCompletion(current, expected, outcome.Class)
	case TypeStripeTierChange:
		err = validateStripeTierCompletion(current, expected, outcome.Class)
	default:
		return nil, errors.New("unsupported tier completion kind")
	}
	if err != nil {
		return nil, err
	}
	status := intents.StatusFailedTerminal
	if outcome.Class == intents.OutcomeSucceeded {
		status = intents.StatusSucceeded
	}
	if current.Status == intents.StatusSucceeded || current.Status == intents.StatusFailedTerminal {
		if current.Status != status {
			return nil, errors.New("tier terminal replay contradicts its committed outcome")
		}
		return &tierCompletion{db: d, current: current, outcome: outcome, now: now, committed: true}, nil
	}
	return &tierCompletion{db: d, current: current, outcome: outcome, now: now}, nil
}

func (c *tierCompletion) commit(ctx context.Context) error {
	if c.committed {
		return nil
	}
	d, current, outcome, now := c.db, c.current, c.outcome, c.now
	status := intents.StatusFailedTerminal
	if outcome.Class == intents.OutcomeSucceeded {
		status = intents.StatusSucceeded
	}
	expected, err := json.Marshal(outcome.Evidence)
	if err != nil {
		return err
	}
	// Preserve retained provider custody and attribution; the command projection
	// may add its final resource/result but cannot replace independent custody.
	evidence := map[string]json.RawMessage{}
	if len(current.ResultEvidence) > 0 {
		if err := json.Unmarshal(current.ResultEvidence, &evidence); err != nil {
			return err
		}
	}
	if evidence == nil {
		evidence = map[string]json.RawMessage{}
	}
	projection := map[string]json.RawMessage{}
	if err := json.Unmarshal(expected, &projection); err != nil {
		return err
	}
	for k, v := range projection {
		if k == "qualified_receipt" || k == "account_requalifications" {
			return errors.New("tier projection cannot replace receipt custody")
		}
		evidence[k] = v
	}
	if record := intents.OperatorResolutionRecord(ctx); record != nil {
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		evidence["operator_resolution"] = raw
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	reason := outcome.Reason
	rows, err := d.Gen(ctx).CompleteTierChangeOutcome(ctx, gen.CompleteTierChangeOutcomeParams{ID: current.ID, MerchantID: current.MerchantID, Status: status, Reason: &reason, Evidence: raw, Now: now})
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("tier operation did not commit its terminal outcome")
	}
	return nil
}

func validateNMITierCompletion(in gen.OpenrailsRailIntent, expected []byte, class intents.OutcomeClass) error {
	p, err := subscriptions.DecodeNMIUpgradePayload(in)
	if err != nil {
		return err
	}
	_, paid, err := intents.LoadCollectedReceipt(in)
	if err != nil {
		return err
	}
	var actual, want nmiUpgradeProgress
	if len(in.ResultEvidence) > 0 {
		if err := json.Unmarshal(in.ResultEvidence, &actual); err != nil {
			return err
		}
	}
	if err := json.Unmarshal(expected, &want); err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, want) {
		return errors.New("tier receipt progress changed before completion")
	}
	if class == intents.OutcomeSucceeded {
		if actual.Successor == nil || actual.Successor.Enrollment == nil || actual.Successor.Enrollment.SubscriptionID == "" || actual.Successor.Refusal != "" {
			return errors.New("upgrade has no accepted successor result")
		}
		if p.ProrationAmount > 0 && (!paid || actual.Proration == nil || actual.Proration.Refusal != "") {
			return errors.New("upgrade has no qualified payment result")
		}
		return nil
	}
	if paid {
		return errors.New("paid upgrade cannot become a refusal")
	}
	if (actual.Successor != nil && actual.Successor.Refusal != "") || (actual.Proration != nil && actual.Proration.Refusal != "") {
		return nil
	}
	if actual.Successor != nil || actual.Proration != nil {
		return errors.New("submitted upgrade has no definitive refusal")
	}
	return nil
}

func validateStripeTierCompletion(in gen.OpenrailsRailIntent, expected []byte, class intents.OutcomeClass) error {
	p, actual, err := decodeStripeTierChange(in)
	if err != nil {
		return err
	}
	if in.PspID == nil || in.SubscriptionID == nil || *in.SubscriptionID != p.SubscriptionID || in.PriceID == nil || *in.PriceID != p.PriceID || in.Rail != "stripe" {
		return errors.New("stripe completion contradicts accepted target")
	}
	var want stripeTierChangeProgress
	if err := json.Unmarshal(expected, &want); err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, want) {
		return errors.New("stripe receipt progress changed before completion")
	}
	if class == intents.OutcomeTerminal {
		if actual.Update != nil && actual.Update.Subscription != nil {
			return errors.New("confirmed stripe update cannot become a refusal")
		}
		if actual.Phases != nil && actual.Phases.Schedule != nil {
			return errors.New("confirmed stripe schedule cannot become a refusal")
		}
		if actual.refused() != nil {
			return nil
		}
		if actual.Update != nil || actual.Schedule != nil || actual.Phases != nil {
			return errors.New("submitted stripe tier change has no definitive refusal")
		}
		return nil
	}
	if actual.refused() != nil {
		return errors.New("stripe refusal cannot become success")
	}
	if p.Action == "upgrade" {
		if actual.Update == nil || actual.Update.Subscription == nil {
			return errors.New("stripe update receipt is missing")
		}
		return actual.Update.Subscription.MatchesPriceChange(p.StripeSubscriptionID, in.ID.String(), p.StripePriceID, p.PriceID.String())
	}
	if actual.Schedule == nil || actual.Schedule.Schedule == nil || actual.Phases == nil || actual.Phases.Schedule == nil {
		return errors.New("stripe schedule receipt is missing")
	}
	if err := actual.Schedule.Schedule.MatchesCreation(p.StripeSubscriptionID, p.OldStripePriceID); err != nil {
		return err
	}
	if actual.Phases.Schedule.ID != actual.Schedule.Schedule.ID {
		return errors.New("stripe phases name another schedule")
	}
	return actual.Phases.Schedule.MatchesPhases(p.StripeSubscriptionID, in.ID.String(), p.OldStripePriceID, p.StripePriceID, currentPhase(actual.Schedule.Schedule, p).EndDate)
}

func commitTierRefusal(ctx context.Context, d *db.DB, in gen.OpenrailsRailIntent, outcome intents.Outcome, now time.Time) intents.Outcome {
	ctx, cancel := intents.LedgerWriteContext(ctx)
	defer cancel()
	err := d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		bound := d.NewWithPgxTx(tx)
		if in.SubscriptionID == nil {
			return errors.New("tier operation has no subscription")
		}
		if _, err := subscriptions.NewSubscriptionRepo(bound).GetByIDForUpdate(ctx, *in.SubscriptionID); err != nil {
			return err
		}
		completion, err := prepareTierCompletion(ctx, bound, in, outcome, now)
		if err != nil {
			return err
		}
		return completion.commit(ctx)
	})
	if err != nil {
		return intents.Ambiguous("tier terminal outcome could not commit: " + err.Error())
	}
	return outcome
}
