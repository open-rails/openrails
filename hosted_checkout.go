package openrails

import (
	"fmt"
	"strings"
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
	PaymentID      string                      `json:"payment_id,omitempty"`
	SubscriptionID string                      `json:"subscription_id,omitempty"`
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
// host's opaque handle for the engine selector; PublicConfig carries only
// browser-safe values (NMI tokenization key/URL, Solana token symbol).
type HostedCheckoutRail struct {
	ID           string            `json:"id"`
	Rail         string            `json:"rail"`
	Mode         string            `json:"mode"`   // one_off or subscription
	Driver       string            `json:"driver"` // collect_js, redirect or solana_pay
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
}

// HostedCheckoutPayRequest is the browser's POST .../pay body.
type HostedCheckoutPayRequest struct {
	OptionID        string `json:"option_id"`
	PaymentToken    string `json:"payment_token,omitempty"`
	PaymentMethodID string `json:"payment_method_id,omitempty"`
	Email           string `json:"email,omitempty"`
	NameOnCard      string `json:"name_on_card,omitempty"`
	Address1        string `json:"address1,omitempty"`
	City            string `json:"city,omitempty"`
	State           string `json:"state,omitempty"`
	Zip             string `json:"zip,omitempty"`
	Country         string `json:"country,omitempty"`
	TokenSymbol     string `json:"token_symbol,omitempty"`
}

// HostedCheckoutPayResult is the host's answer to a pay request.
type HostedCheckoutPayResult struct {
	Status         string `json:"status"`
	RedirectURL    string `json:"redirect_url,omitempty"`
	TransactionURL string `json:"transaction_url,omitempty"`
	PaymentID      string `json:"payment_id,omitempty"`
	SubscriptionID string `json:"subscription_id,omitempty"`
	FailureMessage string `json:"failure_message,omitempty"`
}

// NewHostedCheckoutPlan derives the plan from a catalog product and price and
// stamps the price currency's registered scale. An unregistered currency is
// ErrInvalid: a scale is never guessed.
func NewHostedCheckoutPlan(product *CatalogProduct, price *CatalogPrice) (HostedCheckoutPlan, error) {
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

// HostedCheckoutDriver names the browser driver for a rail kind; false for a
// rail the browser package cannot execute, which must not be offered.
func HostedCheckoutDriver(rail string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(rail)) {
	case "nmi":
		return "collect_js", true
	case "stripe", "ccbill":
		return "redirect", true
	case "solana":
		return "solana_pay", true
	}
	return "", false
}
