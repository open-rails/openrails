package openrails

import (
	"fmt"
	"time"
)

// HostedCheckoutSession is the document a host serves to the openrails-checkout
// browser package for one hosted checkout session (GET .../checkout/sessions/{id}).
// Every amount is an int64 decimal string of Plan.Currency's native unit and
// Plan.UnitDecimals is that currency's registered scale; the browser never
// assumes one. The host owns the session (its id, expiry and attempt state);
// OpenRails owns the shape so every host renders the same checkout.
type HostedCheckoutSession struct {
	ID       string                 `json:"id"`
	Status   string                 `json:"status"` // created, requires_action, succeeded, failed, blocked, expired, canceled
	Merchant HostedCheckoutMerchant `json:"merchant"`
	Plan     HostedCheckoutPlan     `json:"plan"`
	// LineItems itemize the order; absent, the browser shows the plan as one line.
	LineItems []HostedCheckoutLineItem `json:"line_items,omitempty"`
	Tax       *int64                   `json:"tax,omitempty,string"`
	// DueToday overrides the browser's sum of line items and tax.
	DueToday       *int64                      `json:"due_today,omitempty,string"`
	Rails          []HostedCheckoutRail        `json:"rails"`
	SavedMethods   []HostedCheckoutSavedMethod `json:"saved_methods,omitempty"`
	TransactionURL string                      `json:"transaction_url,omitempty"` // solana: URL awaiting confirmation
	PaymentID      string                      `json:"payment_id,omitzero"`
	SubscriptionID string                      `json:"subscription_id,omitzero"`
	FailureMessage string                      `json:"failure_message,omitempty"`
	SuccessURL     string                      `json:"success_url,omitempty"`
	ExpiresAt      time.Time                   `json:"expires_at"`
}

type HostedCheckoutMerchant struct {
	DisplayName string `json:"display_name"`
}

// HostedCheckoutPlan is the offer: what the buyer pays and how often.
type HostedCheckoutPlan struct {
	DisplayName         string `json:"display_name"`
	UnitAmount          int64  `json:"unit_amount,string"`
	Currency            string `json:"currency"`
	UnitDecimals        int    `json:"unit_decimals"`
	PeriodHours         *int   `json:"period_hours,omitempty"`
	AutomaticallyRenews bool   `json:"automatically_renews"`
}

// HostedCheckoutLineItem is one order line in the plan currency.
type HostedCheckoutLineItem struct {
	Label    string `json:"label"`
	Sublabel string `json:"sublabel,omitempty"`
	Amount   int64  `json:"amount,string"`
}

// HostedCheckoutRail is one payment option the browser can execute. ID is the
// host's opaque handle for the engine selector; Driver and PublicConfig are
// copied from the CheckoutRailOption OpenRails advertised.
type HostedCheckoutRail struct {
	ID           string            `json:"id"`
	Rail         string            `json:"rail"`
	Mode         string            `json:"mode"`   // one_off or subscription
	Driver       string            `json:"driver"` // collect_js, stripe_elements, redirect or solana_pay
	PublicConfig map[string]string `json:"public_config,omitempty"`
}

// HostedCheckoutSavedMethod is a stored card the buyer may reuse, display data only.
type HostedCheckoutSavedMethod struct {
	ID       string `json:"id"`
	OptionID string `json:"option_id"`
	Rail     string `json:"rail"`
	Brand    string `json:"brand,omitempty"`
	LastFour string `json:"last_four,omitempty"`
	ExpMonth *int   `json:"exp_month,omitempty"`
	ExpYear  *int   `json:"exp_year,omitempty"`
	// Default pre-selects this card; paying still sends its id explicitly.
	Default bool `json:"default,omitempty"`
}

// HostedCheckoutPayRequest is the browser's POST .../pay body.
type HostedCheckoutPayRequest struct {
	OptionID        string `json:"option_id"`
	PaymentToken    string `json:"payment_token,omitempty"`
	PaymentMethodID string `json:"payment_method_id,omitzero"`
	Email           string `json:"email,omitempty"`
	NameOnCard      string `json:"name_on_card,omitempty"`
	Address1        string `json:"address1,omitempty"`
	City            string `json:"city,omitempty"`
	State           string `json:"state,omitempty"`
	Zip             string `json:"zip,omitempty"`
	Country         string `json:"country,omitempty"`
	TokenSymbol     string `json:"token_symbol,omitempty"`
	// The tokenized new card's display facts, as billing-ui sends them.
	LastFour   string `json:"last_four,omitempty"`
	CardType   string `json:"card_type,omitempty"`
	ExpiryDate string `json:"expiry_date,omitempty"`
}

// HostedCheckoutPayResult is the host's answer to a pay request.
type HostedCheckoutPayResult struct {
	Status         string `json:"status"`
	RedirectURL    string `json:"redirect_url,omitempty"`
	TransactionURL string `json:"transaction_url,omitempty"`
	PaymentID      string `json:"payment_id,omitzero"`
	SubscriptionID string `json:"subscription_id,omitzero"`
	FailureMessage string `json:"failure_message,omitempty"`
}

// NewHostedCheckoutPlan derives the plan from a catalog product and price and
// stamps the price currency's registered scale. An unregistered currency is
// ErrInvalid: a scale is never guessed.
func NewHostedCheckoutPlan(product *Product, price *Price) (HostedCheckoutPlan, error) {
	if product == nil || price == nil {
		return HostedCheckoutPlan{}, fmt.Errorf("%w: hosted checkout plan needs a product and a price", ErrInvalid)
	}
	units, ok := LookupCurrency(price.Currency)
	if !ok {
		return HostedCheckoutPlan{}, fmt.Errorf("%w: price %s currency %q is not registered", ErrInvalid, price.ID, price.Currency)
	}
	return HostedCheckoutPlan{
		DisplayName:         product.DisplayName,
		UnitAmount:          price.UnitAmount,
		Currency:            units.Code,
		UnitDecimals:        units.Decimals,
		PeriodHours:         price.AccessDurationHours,
		AutomaticallyRenews: price.AutoRenew,
	}, nil
}
