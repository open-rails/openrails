package checkoutsession

import (
	"fmt"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/cardguard"
)

// The browser's wire: the session document, the pay body and its answer.
// billing-ui reads these; no Go client method takes or returns them.

// CheckoutSession is one checkout session as the browser reads it (GET
// /v1/checkout-sessions/{id}). Every amount is an int64 decimal string of
// Plan.Currency's native unit and Plan.UnitDecimals is that currency's
// registered scale; the browser never assumes one.
type CheckoutSession struct {
	ID string `json:"id"`
	// Status is created, requires_action, processing, succeeded, failed,
	// blocked, expired or canceled.
	Status   string                  `json:"status"`
	Merchant CheckoutSessionMerchant `json:"merchant"`
	Plan     CheckoutSessionPlan     `json:"plan"`
	// LineItems itemize the order.
	LineItems []CheckoutSessionLineItem `json:"line_items"`
	Tax       *int64                    `json:"tax,string"`
	// DueToday overrides the browser's sum of line items and tax.
	DueToday     *int64                       `json:"due_today,string"`
	Options      []CheckoutSessionOption      `json:"options"`
	SavedMethods []CheckoutSessionSavedMethod `json:"saved_methods"`
	// NextAction is the step a payment awaits (a redirect or a Solana Pay
	// link), null when none.
	NextAction *billing.NextAction `json:"next_action"`
	// Operation is the card payment awaiting the buyer's authentication
	// (3-D Secure) when status is requires_action, null otherwise.
	Operation      *billing.PaymentOperation `json:"operation"`
	PaymentID      *billing.PaymentID        `json:"payment_id"`
	SubscriptionID *billing.SubscriptionID   `json:"subscription_id"`
	FailureMessage *string                   `json:"failure_message"`
	Failure        *billing.PaymentFailure   `json:"failure"`
	SuccessURL     *string                   `json:"success_url"`
	// EmbedOrigin is the origin of the app that minted the session, set when
	// the serving host lists it in Config.Checkout.EmbedOrigins: the only
	// origin the payment page exchanges frame messages with.
	EmbedOrigin *string   `json:"embed_origin"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type CheckoutSessionMerchant struct {
	DisplayName string `json:"display_name"`
}

// CheckoutSessionPlan is the offer: what the buyer pays and how often.
type CheckoutSessionPlan struct {
	// AutoRenew is the customer's accepted order preference, not a catalog term.
	AutoRenew            bool   `json:"auto_renew"`
	DisplayName          string `json:"display_name"`
	UnitAmount           int64  `json:"unit_amount,string"`
	Currency             string `json:"currency"`
	UnitDecimals         int    `json:"unit_decimals"`
	BillingIntervalHours *int   `json:"billing_interval_hours"`
	AccessDurationHours  *int   `json:"access_duration_hours"`
}

// NewPlan is the plan for a price, stamped with its currency's registered
// scale. An unregistered currency is billing.ErrInvalid: a scale is never
// guessed.
func NewPlan(displayName string, amount int64, currency string, billingIntervalHours, accessDurationHours *int) (CheckoutSessionPlan, error) {
	units, ok := billing.LookupCurrency(currency)
	if !ok {
		return CheckoutSessionPlan{}, fmt.Errorf("%w: currency %q is not registered", billing.ErrInvalid, currency)
	}
	return CheckoutSessionPlan{DisplayName: displayName, UnitAmount: amount, Currency: units.Code, UnitDecimals: units.Decimals, BillingIntervalHours: billingIntervalHours, AccessDurationHours: accessDurationHours, AutoRenew: billingIntervalHours != nil}, nil
}

// CheckoutSessionLineItem is one order line in the plan currency.
type CheckoutSessionLineItem struct {
	Label    string  `json:"label"`
	Sublabel *string `json:"sublabel"`
	Amount   int64   `json:"amount,string"`
}

// CheckoutSessionOption is one way the browser can pay. ID is an opaque handle
// bound to a PSP at mint; Driver and PublicConfig are the billing.CheckoutOption's.
type CheckoutSessionOption struct {
	ID string `json:"id"`
	// PSPID is the PSP the option pays on; a page that saves a card in the
	// PSP's own fields (stripe_elements) names it.
	PSPID billing.PSPID `json:"psp_id"`
	Rail  string        `json:"rail"`
	// Mode is one_off or subscription.
	Mode string `json:"mode"`
	// Driver is collect_js, card, stripe_elements, redirect or solana_pay.
	Driver       string            `json:"driver"`
	PublicConfig map[string]string `json:"public_config"`
}

// CheckoutSessionSavedMethod is a stored card the buyer may reuse, display
// data only.
type CheckoutSessionSavedMethod struct {
	ID       billing.PaymentMethodID `json:"id"`
	OptionID string                  `json:"option_id"`
	Rail     string                  `json:"rail"`
	Card     *billing.CardDetails    `json:"card"`
}

// PayCheckoutSessionParams is the browser's POST .../pay body: the option,
// and a saved card, a token, a card entered in the page, or nothing (redirect,
// Solana Pay), with the new card's billing details.
type PayCheckoutSessionParams struct {
	OptionID        string                  `json:"option_id"`
	PaymentMethodID billing.PaymentMethodID `json:"payment_method_id,omitzero"`
	PaymentToken    string                  `json:"payment_token,omitempty"`
	// Card is a new card entered in the page, for an option whose driver is card.
	Card           *cardguard.Card         `json:"card,omitempty"`
	BillingDetails *billing.BillingDetails `json:"billing_details,omitempty"`
	TokenSymbol    string                  `json:"token_symbol,omitempty"`
}

// CheckoutSessionPayResult answers a pay request. With status
// requires_action the buyer completes NextAction, or authenticates Operation.
// Failure explains a definite decline; the buyer may pay again with another
// instrument.
type CheckoutSessionPayResult struct {
	Status         string                    `json:"status"`
	NextAction     *billing.NextAction       `json:"next_action"`
	Operation      *billing.PaymentOperation `json:"operation"`
	PaymentID      *billing.PaymentID        `json:"payment_id"`
	SubscriptionID *billing.SubscriptionID   `json:"subscription_id"`
	FailureMessage *string                   `json:"failure_message"`
	Failure        *billing.PaymentFailure   `json:"failure"`
}
