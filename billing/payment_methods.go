package billing

import (
	"time"
)

// PaymentMethodListParams pages a customer's saved cards, newest first. IDs
// instead reads 1 to MaxBatchItems of the customer's named cards in one page;
// unknown ones are absent.
type PaymentMethodListParams struct {
	PageRequest
	IDs []PaymentMethodID
}

// PaymentMethodStatus is where a saved card stands. Only an active card is
// charged; the others are final.
type PaymentMethodStatus string

const (
	PaymentMethodActive PaymentMethodStatus = "active"
	// PaymentMethodClosed: the bank closed the account. Subscriptions it paid
	// wait for another card; none is canceled because of it.
	PaymentMethodClosed PaymentMethodStatus = "closed"
	// PaymentMethodReplaced: another payment method took its place.
	PaymentMethodReplaced PaymentMethodStatus = "replaced"
	PaymentMethodRemoved  PaymentMethodStatus = "removed"
)

// PaymentMethod is the card account a customer chose. Its issuer may reissue
// it (a new number, expiry or brand) under the same payment method; a card
// the customer enters is a new one. PSPID names the PSP holding it, null for
// a card a third-party custodian holds (any PSP of its rail that reaches the
// custodian charges it).
type PaymentMethod struct {
	ID         PaymentMethodID     `json:"id"`
	CustomerID CustomerID          `json:"customer_id"`
	Rail       string              `json:"rail"`
	PSPID      *PSPID              `json:"psp_id"`
	Status     PaymentMethodStatus `json:"status"`
	// ReplacedBy is the payment method that replaced this one.
	ReplacedBy *PaymentMethodID `json:"replaced_by"`
	// Card is the card as its issuer last reported it.
	Card           *CardDetails        `json:"card"`
	BillingDetails *BillingDetails     `json:"billing_details"`
	Health         PaymentMethodHealth `json:"health"`
	// ContactCardholderAt is when the issuer last asked the cardholder to
	// contact it; the card still pays until it says more.
	ContactCardholderAt *time.Time `json:"contact_cardholder_at"`
	// Reusable: the customer keeps the card for one-click buys (an active
	// card_on_file mandate).
	Reusable bool `json:"reusable"`
	// Mandates are the agreements the card carries, newest first: on the
	// admin read every one, ended included (evidence for disputes); on the
	// customer's own, those that can still authorize a charge. One that
	// requires reconsent waits for POST /v1/me/payment-methods/{id}/verify.
	Mandates []Mandate `json:"mandates"`
	// Subscriptions are the subscriptions the card pays: as their own card,
	// or as the default they follow.
	Subscriptions []PaymentMethodSubscription `json:"subscriptions"`
	// DefaultCurrencies are the currencies the card is the customer's default
	// for (PUT /me/default-payment-methods/{currency}).
	DefaultCurrencies []string  `json:"default_currencies"`
	CreatedAt         time.Time `json:"created_at"`
}

// PaymentMethodSubscription is a subscription a card pays.
type PaymentMethodSubscription struct {
	ID          SubscriptionID `json:"id"`
	DisplayName string         `json:"display_name"`
	CreatedAt   time.Time      `json:"created_at"`
}

// CardExpiryStatus is where a card stands against its expiry.
type CardExpiryStatus string

const (
	CardExpiryValid        CardExpiryStatus = "valid"
	CardExpiryExpiringSoon CardExpiryStatus = "expiring_soon"
	CardExpiryExpired      CardExpiryStatus = "expired"
)

// ChargeOutcome is how a card's most recent charge ended.
type ChargeOutcome string

const (
	ChargeSucceeded ChargeOutcome = "succeeded"
	ChargeFailed    ChargeOutcome = "failed"
)

// PaymentMethodHealth is derived when read: the card's expiry and its most
// recent charge. Active is false when the card expired or that charge failed.
type PaymentMethodHealth struct {
	// ExpiryStatus is null when the expiry is unknown.
	ExpiryStatus      *CardExpiryStatus `json:"expiry_status"`
	LastChargedAt     *time.Time        `json:"last_charged_at"`
	LastChargeOutcome *ChargeOutcome    `json:"last_charge_outcome"`
	Active            bool              `json:"active"`
}

// BillingDetails is who the card bills and where.
type BillingDetails struct {
	Name    *string         `json:"name"`
	Email   *string         `json:"email"`
	Phone   *string         `json:"phone"`
	Address *BillingAddress `json:"address"`
}

// BillingAddress is a card's billing address; Country is ISO 3166-1 alpha-2.
type BillingAddress struct {
	Line1      *string `json:"line1"`
	Line2      *string `json:"line2"`
	City       *string `json:"city"`
	State      *string `json:"state"`
	PostalCode *string `json:"postal_code"`
	Country    *string `json:"country"`
}

// CardDetails is a card's display facts, the one card shape of the API: never
// the number or the security code. A fact the provider did not report is
// null. Brand is lower case (visa, mastercard, amex, ...); ExpMonth is 1-12
// and ExpYear four digits, and the card is valid through the end of that
// month.
type CardDetails struct {
	Brand    *string `json:"brand"`
	Last4    *string `json:"last4"`
	ExpMonth *int    `json:"exp_month"`
	ExpYear  *int    `json:"exp_year"`
}

// CreatePaymentMethodParams saves a card with the PSP PSPID: a token from the
// PSP's own card fields (PaymentToken), or, for a PSP whose card_entry is
// server, the card itself (Card). OpenRails reads the saved card's display
// facts from the provider.
type CreatePaymentMethodParams struct {
	PSPID          PSPID           `json:"psp_id"`
	PaymentToken   string          `json:"payment_token,omitempty"`
	Card           *Card           `json:"card,omitempty"`
	BillingDetails *BillingDetails `json:"billing_details,omitempty"`
}

// ReplacePaymentMethodCardParams replaces a saved card in place, with a new
// token or card; subscriptions it pays keep paying with the new card. Billing
// details that are present replace the saved ones.
type ReplacePaymentMethodCardParams struct {
	PaymentToken   string          `json:"payment_token,omitempty"`
	Card           *Card           `json:"card,omitempty"`
	BillingDetails *BillingDetails `json:"billing_details,omitempty"`
}

// UpdatePaymentMethodParams edits a saved card in place: its expiry (both
// fields together), its billing details, and whether it is kept for one-click
// buys. An absent field is unchanged; the card number never changes, so a new
// card is a new payment method.
type UpdatePaymentMethodParams struct {
	ExpMonth       *int            `json:"exp_month,omitempty"`
	ExpYear        *int            `json:"exp_year,omitempty"`
	BillingDetails *BillingDetails `json:"billing_details,omitempty"`
	Reusable       *bool           `json:"reusable,omitempty"`
}

// Default payment method codes.
const (
	// CodeDefaultPaymentMethodRequired: the customer has no default card for
	// the currency (400).
	CodeDefaultPaymentMethodRequired = "default_payment_method_required"
	// CodeDefaultPaymentMethodInvalid: the card cannot be the customer's
	// default for the currency (400).
	CodeDefaultPaymentMethodInvalid = "default_payment_method_invalid"
)

// SetDefaultPaymentMethodParams makes a saved card the customer's default for
// one currency: it collects their invoices there and pays every card
// subscription in it without a card of its own.
type SetDefaultPaymentMethodParams struct {
	PaymentMethodID PaymentMethodID `json:"payment_method_id"`
}

// DefaultPaymentMethod is the customer's default card for one currency.
type DefaultPaymentMethod struct {
	Currency        string          `json:"currency"`
	PaymentMethodID PaymentMethodID `json:"payment_method_id"`
}
