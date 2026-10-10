package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/payments"
)

// ReadPaymentMethodCard reads one payment method of this account; nil when
// Stripe no longer has it.
func (s *StripeService) ReadPaymentMethodCard(ctx context.Context, methodRef string) (*payments.StripePaymentMethodState, error) {
	if !stripeEngineID(methodRef, "pm_") {
		return nil, errors.New("invalid Stripe payment method identity")
	}
	body, status, err := s.stripeGet(ctx, "/v1/payment_methods/"+url.PathEscape(methodRef), nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status >= 400 {
		return nil, parseStripeAPIError(status, body)
	}
	return payments.ParseStripePaymentMethodState(body)
}

// EditPaymentMethodCard sets a saved card's expiry (when month and year are
// given) and billing details at Stripe: the fields Stripe lets a merchant
// edit on a payment method.
func (s *StripeService) EditPaymentMethodCard(ctx context.Context, methodRef string, expMonth, expYear int, details *billing.BillingDetails, idempotencyKey string) error {
	if !stripeEngineID(methodRef, "pm_") {
		return errors.New("invalid Stripe payment method identity")
	}
	v := url.Values{}
	if expMonth > 0 && expYear > 0 {
		v.Set("card[exp_month]", strconv.Itoa(expMonth))
		v.Set("card[exp_year]", strconv.Itoa(expYear))
	}
	if d := details; d != nil {
		set := func(key string, value *string) {
			if value != nil {
				v.Set(key, strings.TrimSpace(*value))
			}
		}
		set("billing_details[name]", d.Name)
		set("billing_details[email]", d.Email)
		set("billing_details[phone]", d.Phone)
		if a := d.Address; a != nil {
			set("billing_details[address][line1]", a.Line1)
			set("billing_details[address][line2]", a.Line2)
			set("billing_details[address][city]", a.City)
			set("billing_details[address][state]", a.State)
			set("billing_details[address][postal_code]", a.PostalCode)
			set("billing_details[address][country]", a.Country)
		}
	}
	if len(v) == 0 {
		return nil
	}
	_, err := s.stripePostForm(ctx, "/v1/payment_methods/"+url.PathEscape(methodRef), v, idempotencyKey)
	return err
}

// StripeVerification is a SetupIntent confirmed with the customer present.
type StripeVerification struct {
	ID, Status string
}

// VerifyPaymentMethod confirms a SetupIntent for an attached card while its
// customer is present: the customer-initiated storing transaction a new
// stored-credential agreement for off-session charges cites. Stripe answers
// requires_action when the issuer asks for authentication.
func (s *StripeService) VerifyPaymentMethod(ctx context.Context, customerRef, methodRef, idempotencyKey string) (StripeVerification, error) {
	if !stripeEngineID(customerRef, "cus_") || !stripeEngineID(methodRef, "pm_") {
		return StripeVerification{}, errors.New("invalid Stripe customer or payment method identity")
	}
	v := url.Values{"customer": {customerRef}, "payment_method": {methodRef}, "usage": {"off_session"}, "confirm": {"true"}, "payment_method_types[]": {"card"}}
	body, err := s.stripePostForm(ctx, "/v1/setup_intents", v, idempotencyKey)
	if err != nil {
		return StripeVerification{}, err
	}
	var si struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if json.Unmarshal(body, &si) != nil || !stripeEngineID(si.ID, "seti_") || si.Status == "" {
		return StripeVerification{}, errors.New("Stripe setup verification answer is malformed")
	}
	return StripeVerification{ID: si.ID, Status: si.Status}, nil
}

// CancelSetup cancels a SetupIntent that is still waiting.
func (s *StripeService) CancelSetup(ctx context.Context, setupID, idempotencyKey string) error {
	if !stripeEngineID(setupID, "seti_") {
		return errors.New("invalid Stripe setup identity")
	}
	_, err := s.stripePostForm(ctx, "/v1/setup_intents/"+url.PathEscape(setupID)+"/cancel", url.Values{}, idempotencyKey)
	return err
}
