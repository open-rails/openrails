package intents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

const stripeRecurringDeclineKey = "stripe_recurring_decline"

type stripeRecurringDecline struct {
	DeclineCode        string         `json:"decline_code,omitempty"`
	AdviceCode         string         `json:"advice_code,omitempty"`
	NetworkAdviceCode  string         `json:"network_advice_code,omitempty"`
	NetworkDeclineCode string         `json:"network_decline_code,omitempty"`
	Binding            receiptBinding `json:"binding"`
	FailureCode        string         `json:"failure_code"`
	PaymentIntentID    string         `json:"payment_intent_id"`
}

// StripeRecurringDecline is a sealed canceled-PI refusal: its code (the
// issuer's decline code, else the failure code) and the advice it carried.
type StripeRecurringDecline struct {
	Code, PaymentIntentID                         string
	AdviceCode, NetworkAdviceCode, NetworkDecline string
}

// Evidence is the refusal as the decline table reads it.
func (d StripeRecurringDecline) Evidence() decline.Evidence {
	return decline.Evidence{Rail: "stripe", Code: d.Code, Advice: d.AdviceCode, NetworkAdvice: d.NetworkAdviceCode, IssuerCode: d.NetworkDecline}
}

// LoadStripeRecurringDecline exposes a sealed canceled-PI refusal, never a
// mutable requires_payment_method status or synthetic NMI response code.
func LoadStripeRecurringDecline(in gen.BillingProviderIntent) (StripeRecurringDecline, bool, error) {
	var all map[string]json.RawMessage
	if len(in.ResultEvidence) == 0 {
		return StripeRecurringDecline{}, false, nil
	}
	if err := json.Unmarshal(in.ResultEvidence, &all); err != nil {
		return StripeRecurringDecline{}, false, err
	}
	raw, found := all[stripeRecurringDeclineKey]
	if !found {
		return StripeRecurringDecline{}, false, nil
	}
	var fact stripeRecurringDecline
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fact); err != nil {
		return StripeRecurringDecline{}, true, err
	}
	binding, err := collectionBinding(in)
	if err != nil {
		return StripeRecurringDecline{}, true, err
	}
	if in.Rail != "stripe" || in.IntentType != subscriptions.TypeSubscriptionCollection || fact.Binding != binding || fact.FailureCode != "canceled" || !strings.HasPrefix(fact.PaymentIntentID, "pi_") || EvidenceString(in, "submitted_at") == "" {
		return StripeRecurringDecline{}, true, errors.New("Stripe recurring refusal is not bound to submitted operation")
	}
	for _, key := range []string{qualifiedReceiptKey, qualifiedCollectionNonexecutionKey, rebillDeclineKey} {
		if _, exists := all[key]; exists {
			return StripeRecurringDecline{}, true, errors.New("Stripe recurring refusal contradicts retained provider custody")
		}
	}
	code := fact.DeclineCode
	if code == "" {
		code = fact.FailureCode
	}
	return StripeRecurringDecline{Code: code, PaymentIntentID: fact.PaymentIntentID, AdviceCode: fact.AdviceCode, NetworkAdviceCode: fact.NetworkAdviceCode, NetworkDecline: fact.NetworkDeclineCode}, true, nil
}
func (s *Store) RetainStripeRecurringDecline(ctx context.Context, in gen.BillingProviderIntent, service *subscriptions.StripeService, reference string) error {
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
		result = s.retainedDecline(ctx, in, result)
	}
	binding, err := collectionBinding(in)
	if err != nil {
		return err
	}
	fact := stripeRecurringDecline{Binding: binding, FailureCode: result.FailureCode, PaymentIntentID: result.PaymentIntentID, DeclineCode: result.DeclineCode,
		AdviceCode: result.AdviceCode, NetworkAdviceCode: result.NetworkAdviceCode, NetworkDeclineCode: result.NetworkDeclineCode}
	current, err := s.retainQualifiedEvidence(ctx, in, stripeRecurringDeclineKey, fact)
	if err != nil {
		return err
	}
	_, found, err = LoadStripeRecurringDecline(current)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("Stripe recurring refusal custody missing")
	}
	return nil
}

// retainedDecline is the issuer's decline code and advice the execute leg
// retained before canceling: Stripe clears them from the canceled
// PaymentIntent.
func (s *Store) retainedDecline(ctx context.Context, in gen.BillingProviderIntent, result subscriptions.StripeEnginePaymentResult) subscriptions.StripeEnginePaymentResult {
	current, err := s.Get(ctx, in.ID)
	if err != nil {
		return result
	}
	result.DeclineCode = EvidenceString(current, "decline_code")
	result.AdviceCode = EvidenceString(current, "advice_code")
	result.NetworkAdviceCode = EvidenceString(current, "network_advice_code")
	result.NetworkDeclineCode = EvidenceString(current, "network_decline_code")
	return result
}
