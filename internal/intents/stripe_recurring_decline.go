package intents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

const stripeRecurringDeclineKey = "stripe_recurring_decline"

type stripeRecurringDecline struct {
	DeclineCode     string         `json:"decline_code,omitempty"`
	Binding         receiptBinding `json:"binding"`
	FailureCode     string         `json:"failure_code"`
	PaymentIntentID string         `json:"payment_intent_id"`
}

// LoadStripeRecurringDecline exposes a sealed canceled-PI refusal, never a
// mutable requires_payment_method status or synthetic NMI response code.
func LoadStripeRecurringDecline(in gen.OpenrailsRailIntent) (string, string, bool, error) {
	var all map[string]json.RawMessage
	if len(in.ResultEvidence) == 0 {
		return "", "", false, nil
	}
	if err := json.Unmarshal(in.ResultEvidence, &all); err != nil {
		return "", "", false, err
	}
	raw, found := all[stripeRecurringDeclineKey]
	if !found {
		return "", "", false, nil
	}
	var fact stripeRecurringDecline
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fact); err != nil {
		return "", "", true, err
	}
	binding, err := collectionBinding(in)
	if err != nil {
		return "", "", true, err
	}
	if in.Rail != "stripe" || in.IntentType != subscriptions.TypeSubscriptionCollection || fact.Binding != binding || fact.FailureCode != "canceled" || !strings.HasPrefix(fact.PaymentIntentID, "pi_") || EvidenceString(in, "submitted_at") == "" {
		return "", "", true, errors.New("Stripe recurring refusal is not bound to submitted operation")
	}
	for _, key := range []string{qualifiedReceiptKey, qualifiedCollectionNonexecutionKey, rebillDeclineKey} {
		if _, exists := all[key]; exists {
			return "", "", true, errors.New("Stripe recurring refusal contradicts retained provider custody")
		}
	}
	code := fact.DeclineCode
	if code == "" {
		code = fact.FailureCode
	}
	return code, fact.PaymentIntentID, true, nil
}
func (s *Store) RetainStripeRecurringDecline(ctx context.Context, in gen.OpenrailsRailIntent, service *subscriptions.StripeService, reference string) error {
	if in.IntentType != subscriptions.TypeSubscriptionCollection {
		return errors.New("not recurring Stripe collection")
	}
	params, err := StripeEngineParams(in)
	if err != nil {
		return err
	}
	if service == nil {
		return errors.New("Stripe recurring reader unavailable")
	}
	result, found, err := service.ReadEnginePayment(ctx, params, reference)
	if err != nil {
		return err
	}
	if !found || result.State != subscriptions.StripeEngineDeclined || result.FailureCode != "canceled" {
		return errors.New("Stripe recurring decline is still executable")
	}
	if result.DeclineCode == "" {
		result.DeclineCode = s.retainedDeclineCode(ctx, in)
	}
	binding, err := collectionBinding(in)
	if err != nil {
		return err
	}
	fact := stripeRecurringDecline{Binding: binding, FailureCode: result.FailureCode, PaymentIntentID: result.PaymentIntentID, DeclineCode: result.DeclineCode}
	current, err := s.retainQualifiedEvidence(ctx, in, stripeRecurringDeclineKey, fact)
	if err != nil {
		return err
	}
	_, _, found, err = LoadStripeRecurringDecline(current)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("Stripe recurring refusal custody missing")
	}
	return nil
}

// retainedDeclineCode is the issuer's decline code the execute leg retained
// before cancelling: Stripe clears it from the cancelled PaymentIntent.
func (s *Store) retainedDeclineCode(ctx context.Context, in gen.OpenrailsRailIntent) string {
	current, err := s.Get(ctx, in.ID)
	if err != nil {
		return ""
	}
	return EvidenceString(current, "decline_code")
}
