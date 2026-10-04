package billing

import (
	"time"
)

// PaymentMethod is a customer's stored card. PSPID names the PSP holding it,
// null for a card a third-party custodian holds (any PSP of its rail that
// reaches the custodian charges it).
type PaymentMethod struct {
	ID             PaymentMethodID     `json:"id"`
	CustomerID     CustomerID          `json:"customer_id"`
	Rail           string              `json:"rail"`
	PSPID          *PSPID              `json:"psp_id"`
	Card           *CardDetails        `json:"card"`
	BillingDetails *BillingDetails     `json:"billing_details"`
	Health         PaymentMethodHealth `json:"health"`
	// Subscriptions are the subscriptions the card pays.
	Subscriptions []PaymentMethodSubscription `json:"subscriptions"`
	// CollectionCurrencies are the currencies whose invoices the card
	// collects (PUT /me/collection-payment-method).
	CollectionCurrencies []string  `json:"collection_currencies"`
	CreatedAt            time.Time `json:"created_at"`
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

// CollectionPaymentMethod is the card that collects a customer's invoices in
// one currency.
type CollectionPaymentMethod struct {
	Currency        string          `json:"currency"`
	PaymentMethodID PaymentMethodID `json:"payment_method_id"`
}

// PaymentMethodDeletion distinguishes completed deletion from a durable
// operation awaiting provider reconciliation. Pending is never reported deleted.
type PaymentMethodDeletion struct{ Pending bool }
