package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/open-rails/openrails/internal/config"
)

// ReadEngineRenewal reconciles the persistent provider objects for a new
// renewal identity before another create. Customer listing is fully paginated;
// the eventually consistent Search API is not used. An empty list is not a
// remote lock: the provider's same-attempt idempotency key remains necessary,
// and independently writable books with divergent attempts remain unsupported.
func (s *StripeService) ReadEngineRenewal(ctx context.Context, p StripeEnginePaymentParams) (StripeEnginePaymentResult, bool, error) {
	if p.Renewal == nil {
		// Do not reinterpret an operation accepted before stable renewal IDs.
		return StripeEnginePaymentResult{}, false, nil
	}
	scoped, err := s.engineScoped(p)
	if err != nil {
		return StripeEnginePaymentResult{}, false, err
	}
	// Large customer histories must not occupy a worker indefinitely. Timeout
	// or incomplete pagination is inconclusive, never permission to charge.
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var reference string
	err = scoped.stripeListAll(ctx, "/v1/payment_intents", url.Values{"customer": {p.Instrument.RailCustomerRef}}, func(raw json.RawMessage) error {
		var pi stripeEngineIntent
		if err := json.Unmarshal(raw, &pi); err != nil {
			return errors.New("Stripe renewal list malformed")
		}
		if pi.Metadata["openrails_engine_operation"] == p.OperationID.String() {
			if reference != "" {
				return errors.New("multiple Stripe payments claim this renewal attempt")
			}
			if err := pi.matches(p); err != nil {
				return err
			}
			reference = pi.ID
			return nil
		}
		if pi.Metadata["openrails_renewal_obligation"] != p.Renewal.Obligation {
			return nil
		}
		if pi.Object != "payment_intent" || !stripeEngineID(pi.ID, "pi_") || pi.LiveMode == nil || *pi.LiveMode == config.IsTestMode(scoped.Config) || pi.Metadata["openrails_initial"] != "false" || pi.Metadata["openrails_one_time"] != "" {
			return errors.New("Stripe renewal history has invalid provider scope")
		}
		attempt, err := strconv.Atoi(pi.Metadata["openrails_renewal_attempt"])
		if err != nil || attempt < 0 || strconv.Itoa(attempt) != pi.Metadata["openrails_renewal_attempt"] ||
			pi.Metadata["openrails_merchant"] != p.MerchantID.String() || pi.Metadata["openrails_psp"] != p.PSPID.String() ||
			pi.Metadata["openrails_customer"] != p.CustomerID.String() || rawID(pi.Customer) != p.Instrument.RailCustomerRef ||
			pi.Metadata["openrails_engine_operation"] != renewalOperationID(p.MerchantID, p.PSPID, p.Renewal.Obligation, attempt).String() {
			return errors.New("Stripe renewal history has conflicting attempt identity")
		}
		if attempt >= p.Renewal.Attempt || pi.Status != "canceled" || pi.AmountReceived != 0 {
			return errors.New("Stripe renewal has another paid, executable or later attempt; reconcile before charging")
		}
		return nil
	})
	if err != nil || reference == "" {
		return StripeEnginePaymentResult{}, false, err
	}
	// A list entry is only a candidate. Re-read the exact object and captured
	// charge through the ordinary qualifier before adopting its result.
	result, found, err := scoped.ReadEnginePayment(ctx, p, reference)
	if err != nil || !found {
		// Preserve the observed candidate even when the detailed read is
		// unavailable. A subsequent empty lookup must not erase this evidence.
		return StripeEnginePaymentResult{PaymentIntentID: reference}, false, errors.New("listed Stripe renewal could not be qualified by exact readback")
	}
	return result, true, nil
}
