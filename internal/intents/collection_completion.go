package intents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
)

// CompleteInvoiceCollection is called at the end of the domain transaction,
// after its payer/invoice/attempt locks. It is deliberately collection-specific:
// a successful state requires retained, validated provider receipt custody.
func (s *Store) CompleteInvoiceCollection(ctx context.Context, in gen.OpenrailsRailIntent, outcome Outcome, now time.Time) error {
	if in.IntentType != "invoice_collection" {
		return errors.New("invoice completion received another operation kind")
	}
	return s.completeCollectedPayment(ctx, in, outcome, now)
}

func (s *Store) CompleteManualRebill(ctx context.Context, in gen.OpenrailsRailIntent, outcome Outcome, now time.Time) error {
	if in.IntentType != TypeManualRebill {
		return errors.New("rebill completion received another operation kind")
	}
	return s.completeCollectedPayment(ctx, in, outcome, now)
}

func (s *Store) completeCollectedPayment(ctx context.Context, in gen.OpenrailsRailIntent, outcome Outcome, now time.Time) error {
	if s == nil || s.db == nil || s.db.Pool() != nil {
		return errors.New("collection completion requires a transaction-bound database")
	}
	binding, err := collectionBinding(in)
	if err != nil {
		return err
	}
	if outcome.Class != OutcomeSucceeded && outcome.Class != OutcomeTerminal {
		return errors.New("collection completion requires a terminal outcome")
	}
	if err := refuseCustodyKeys(outcome.Evidence); err != nil {
		return err
	}
	current, err := s.db.Gen(ctx).LockRailIntentForCollectionCompletion(ctx, gen.LockRailIntentForCollectionCompletionParams{ID: in.ID, MerchantID: in.MerchantID})
	if err != nil {
		return err
	}
	accepted, err := collectionBinding(current)
	if err != nil {
		return err
	}
	if binding != accepted {
		return errors.New("collection completion no longer names the accepted operation")
	}
	receipt, found, err := LoadCollectedReceipt(current)
	if err != nil {
		return fmt.Errorf("invalid retained collection receipt: %w", err)
	}
	status := StatusFailedTerminal
	evidence := map[string]any{}
	reason := outcome.Reason
	if outcome.Class == OutcomeSucceeded {
		if !found {
			return errors.New("collection cannot succeed without qualified receipt custody")
		}
		status = StatusSucceeded
		reason = ""
		evidence["transaction_id"] = receipt.TransactionID()
		evidence["rail"] = current.Rail
		evidence["verified_existing"] = true
		if invoice := receipt.ExternalInvoiceID(); invoice != "" {
			evidence["external_invoice_id"] = invoice
		}
		evidence[qualifiedReceiptKey] = receipt.data
	} else {
		if found {
			return errors.New("qualified collected payment cannot become nonexecution or refusal")
		}
		for key, value := range outcome.Evidence {
			evidence[key] = value
		}
	}
	// Operator attribution is prepared by the runner before invoking the handler.
	// A verifier finishing an earlier operator attempt retains that attribution.
	var existing map[string]json.RawMessage
	if len(current.ResultEvidence) > 0 {
		if err := json.Unmarshal(current.ResultEvidence, &existing); err != nil {
			return err
		}
	}
	if current.IntentType == TypeManualRebill {
		for _, key := range []string{rebillPreparationKey, rebillDeclineKey} {
			if value, ok := existing[key]; ok {
				evidence[key] = value
			}
		}
		if outcome.Class == OutcomeTerminal {
			refusal, found, err := loadRebillDecline(current)
			if err != nil {
				return err
			}
			if found {
				if outcome.Evidence["declined"] != true || outcome.Evidence["response_code"] != refusal.ResponseCode {
					return errors.New("rebill terminal result contradicts retained refusal")
				}
			} else if EvidenceString(current, rebillSubmittedAt) != "" || outcome.Evidence["not_executed"] != true {
				return errors.New("submitted rebill cannot be released without a definitive refusal")
			}
		}
	}
	if record, ok := existing["operator_resolution"]; ok {
		evidence["operator_resolution"] = record
	}
	if record, ok := ctx.Value(operatorResolutionContextKey{}).(map[string]any); ok {
		evidence["operator_resolution"] = record
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	if current.Status == StatusSucceeded || current.Status == StatusFailedTerminal {
		if current.Status != status {
			return errors.New("collection terminal result contradicts accepted completion")
		}
		// On terminal replay, compare the immutable result projection. Attribution
		// stays as committed even when a caller supplies a later observer context.
		delete(evidence, "operator_resolution")
		delete(existing, "operator_resolution")
		expected, err := json.Marshal(evidence)
		if err != nil {
			return err
		}
		actual, err := json.Marshal(existing)
		if err != nil {
			return err
		}
		var a, b any
		da := json.NewDecoder(bytes.NewReader(expected))
		da.UseNumber()
		if err := da.Decode(&a); err != nil {
			return err
		}
		db := json.NewDecoder(bytes.NewReader(actual))
		db.UseNumber()
		if err := db.Decode(&b); err != nil {
			return err
		}
		expected, _ = json.Marshal(a)
		actual, _ = json.Marshal(b)
		if !bytes.Equal(expected, actual) {
			return errors.New("collection terminal evidence contradicts accepted completion")
		}
		return nil
	}
	if current.Status != StatusInFlight && current.Status != StatusUnknownNeedsVerify {
		return errors.New("collection operation is not completing an active attempt")
	}
	result, err := s.db.Gen(ctx).CompleteRailIntentCollection(ctx, gen.CompleteRailIntentCollectionParams{ID: in.ID, MerchantID: in.MerchantID, Status: status, Evidence: raw, Reason: reason, Now: now})
	if err != nil {
		return err
	}
	if result != 1 {
		return errors.New("collection terminal write did not update its accepted operation")
	}
	return nil
}
