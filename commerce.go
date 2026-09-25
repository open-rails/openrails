package openrails

import (
	"github.com/google/uuid"
	"time"
)

// CheckoutRailOption is one way checkout can sell a price now: the PSP is
// armed and its rail can make this kind of sale (#1078). Driver and
// PublicConfig are what a browser needs to render it; an empty Driver means no
// browser flow can execute the option.
type CheckoutRailOption struct {
	// Selector is the checkout payment.rail value (the PSP key).
	Selector string `json:"selector"`
	PSPID    string `json:"psp_id"`
	Rail     string `json:"rail"`
	// Mode is one_off or subscription.
	Mode string `json:"mode"`
	// Driver is collect_js, stripe_elements, redirect or solana_pay.
	Driver string `json:"driver,omitempty"`
	// PublicConfig holds browser-safe values: the PSP's public keys, and for
	// Solana token_symbol, token_name and network.
	PublicConfig map[string]string `json:"public_config,omitempty"`
	// Status is CheckoutPSPTemporarilyUnavailable when the option's PSP could
	// not be checked just now; it has no driver. RetryAfter is in seconds.
	Status     string `json:"status,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
}

// CheckoutConfig lists the merchant's armed PSPs and the public values a
// browser needs to drive each one. It never contains merchant secrets.
type CheckoutConfig struct {
	Object string              `json:"object"`
	PSPs   []CheckoutPSPConfig `json:"psps"`
	// Solana is present when a Solana PSP is armed: the network and the
	// tokens the merchant accepts, so a host renders wallet options from the
	// same document it renders card options from.
	Solana *SolanaCheckoutConfig `json:"solana,omitempty"`
}

// SolanaCheckoutConfig is the merchant's public Solana acceptance policy.
type SolanaCheckoutConfig struct {
	Network        string                `json:"network"`
	Chain          string                `json:"chain"`
	PreferredToken string                `json:"preferred_token"`
	Tokens         []SolanaCheckoutToken `json:"tokens"`
}

// SolanaCheckoutToken is one accepted SPL token.
type SolanaCheckoutToken struct {
	Symbol            string `json:"symbol"`
	Name              string `json:"name"`
	Mint              string `json:"mint"`
	Decimals          int    `json:"decimals"`
	Preferred         bool   `json:"preferred"`
	RecurringEligible bool   `json:"recurring_eligible"`
}

// CheckoutPSPConfig describes one armed PSP for browser checkout.
type CheckoutPSPConfig struct {
	// PSPID is the public stable account selector used by saved-method setup.
	PSPID string `json:"psp_id"`
	// Key is the checkout payment.rail selector.
	Key string `json:"key"`
	// Rail is the gateway kind: nmi, ccbill, stripe or solana.
	Rail string `json:"rail"`
	// Custodian holds the card: "psp", or the third party whose page tokenizes it.
	Custodian   string `json:"custodian"`
	DisplayName string `json:"display_name"`
	// Flow is how a browser drives this PSP: tokenize, elements, redirect or
	// wallet. elements: the page saves the card with the PSP's own fields and
	// checkout charges the saved card, with authentication in the page.
	Flow string `json:"flow"`
	// Checkout is true when new purchases and newly entered cards use this PSP
	// under the merchant's checkout routing. Other armed PSPs stay listed so
	// their existing cards and agreements keep working.
	Checkout bool `json:"checkout"`
	// Config holds whitelisted public values, such as a tokenization key.
	Config map[string]string `json:"config,omitempty"`
	// Status is CheckoutPSPTemporarilyUnavailable when the PSP's credentials
	// could not be checked just now: it is listed without Config, and the
	// document is not cacheable. Empty is available. RetryAfter is in seconds.
	Status     string `json:"status,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
}

// CheckoutPSPTemporarilyUnavailable marks a PSP (or rail option) whose
// credentials could not be checked just now; retry after RetryAfter seconds.
const CheckoutPSPTemporarilyUnavailable = "temporarily_unavailable"

type CheckoutCustomerIdentity struct {
	ID            string `json:"id"`
	VerifiedEmail string `json:"verified_email"`
	Username      string `json:"username"`
}

// CreateCheckoutSessionRequest creates a purchase for a merchant-owned customer.
// The server derives one-off versus recurring behavior from the selected price.
// IdempotencyKey must identify this checkout attempt across retries.
// The merchant endpoint trusts the host to invoke checkout for a real customer
// action. Merchant credentials authorize the host; they do not prove customer
// interaction. Do not use this command as an unattended way to establish a
// customer-initiated stored-card agreement.
//
// A recurring card price is quoted unless Confirm is set: an NMI PaymentToken
// is saved as the customer's method, and the customer accepts the quote at
// /v1/me/checkout/{id}/confirm.
type CreateCheckoutSessionRequest struct {
	Customer CheckoutCustomerIdentity `json:"customer"`
	// Supply exactly one of PriceID or PriceKey. Keys are always opaque, even
	// when they resemble UUIDs. Accepted retries retain the original offer.
	PriceID  string `json:"price_id,omitzero"`
	PriceKey string `json:"price_key,omitempty"`
	// Entitlement optionally binds admission to the opaque resource the host
	// showed. OpenRails verifies the selected product grants this key.
	Entitlement    string                 `json:"entitlement,omitempty"`
	OfferKind      OfferKind              `json:"offer_kind,omitempty"`
	PaymentOptions CheckoutPaymentOptions `json:"payment"`
	Metadata       map[string]string      `json:"metadata"`
	IdempotencyKey string                 `json:"-"`
	SuccessURL     string                 `json:"success_url"` // Required for Stripe hosted checkout
	CancelURL      string                 `json:"cancel_url"`  // Required for Stripe hosted checkout
	// Confirm relays the present customer's pay action on the displayed price:
	// a recurring price is enrolled and charged in this call instead of quoted.
	// A definite decline creates no subscription and keeps no card saved from
	// PaymentToken. Retries with the same IdempotencyKey never charge twice.
	Confirm bool `json:"confirm,omitempty"`
}

// CreatePaymentMethodSessionRequest authorizes a nonmonetary card setup.
// It cannot carry a price or select a purchase/subscription mode.
type CreatePaymentMethodSessionRequest struct {
	Customer       CheckoutCustomerIdentity `json:"customer"`
	PaymentOptions CheckoutPaymentOptions   `json:"payment"`
	Metadata       map[string]string        `json:"metadata,omitempty"`
	IdempotencyKey string                   `json:"-"`
}

// CreateSolanaCancelSessionRequest prepares an existing subscription cancellation.
type CreateSolanaCancelSessionRequest struct {
	Customer       CheckoutCustomerIdentity `json:"customer"`
	SubscriptionID string                   `json:"subscription_id"`
	PaymentOptions CheckoutPaymentOptions   `json:"payment"`
	Metadata       map[string]string        `json:"metadata,omitempty"`
	IdempotencyKey string                   `json:"-"`
}

// CreateSolanaTierChangeSessionRequest prepares a change to an existing subscription.
type CreateSolanaTierChangeSessionRequest struct {
	Customer       CheckoutCustomerIdentity `json:"customer"`
	SubscriptionID string                   `json:"subscription_id"`
	NewPriceID     string                   `json:"new_price_id"`
	PaymentOptions CheckoutPaymentOptions   `json:"payment"`
	Metadata       map[string]string        `json:"metadata,omitempty"`
	IdempotencyKey string                   `json:"-"`
}

type CheckoutPaymentOptions struct {
	PSPID           string `json:"psp_id,omitzero"`
	Rail            string `json:"rail"`                       // "nmi", "ccbill", "solana", "stripe"
	PaymentMethodID string `json:"payment_method_id,omitzero"` // For returning customers with saved payment methods
	PaymentToken    string `json:"payment_token"`              // For new card tokenization (NMI Collect.js)

	// Solana-specific
	TokenSymbol string `json:"token_symbol"` // e.g., "USDC", "SOL"
	Flow        string `json:"flow"`         // "transfer_request" or "transaction_request"
	Wallet      string `json:"wallet"`       // Solana wallet address

	// Billing details. CCBill requires the canonical name, postal code, and
	// country; its verified email comes from CheckoutCustomerIdentity. Street,
	// city, and state are optional. Stripe hosted Checkout collects its own.
	Email      string `json:"email"`
	NameOnCard string `json:"name_on_card"` // Canonical full name; first/last are legacy aliases.
	FirstName  string `json:"first_name"`
	LastName   string `json:"last_name"`
	Address1   string `json:"address1"`
	City       string `json:"city"`
	State      string `json:"state"`
	Zip        string `json:"zip"`
	Country    string `json:"country"`

	// Card details (for display, from tokenization)
	LastFour   string `json:"last_four"`
	CardType   string `json:"card_type"`
	ExpiryDate string `json:"expiry_date"`
}

// CheckoutSession is the durable result of a checkout attempt. Amount is native
// currency units (micros for fiat), encoded as a decimal string over HTTP;
// timestamps are RFC3339 instants.
type CheckoutSession struct {
	Capture         *CustodianCaptureAction `json:"capture,omitempty"`
	PaymentMethodID *string                 `json:"payment_method_id,omitempty"`
	ID              string                  `json:"id"`
	Status          string                  `json:"status"` // "created", "requires_action", "succeeded", "failed", "expired", "canceled"
	Mode            string                  `json:"mode"`   // "subscription", "one_off"
	PriceID         *string                 `json:"price_id"`
	Amount          *int64                  `json:"amount,string"`
	Currency        *string                 `json:"currency"`
	PaymentStatus   string                  `json:"payment_status"` // "unpaid", "paid", "no_payment_required"
	ClientSecret    *string                 `json:"client_secret"`
	URL             *string                 `json:"url"` // Redirect URL for CCBill/Stripe
	SubscriptionID  *string                 `json:"subscription_id"`
	PaymentID       *string                 `json:"payment_id"`
	ExpiresAt       *time.Time              `json:"expires_at,omitempty"`
	CreatedAt       time.Time               `json:"created_at"`
	Metadata        map[string]string       `json:"metadata"`
	RailData        map[string]any          `json:"rail_data"` // Rail-specific response data
	// Operation is the accepted card payment; with Status "requires_action"
	// the customer completes its provider authentication (3-D Secure) in the page.
	Operation *PaymentOperation `json:"operation,omitempty"`
	// Failure explains a definite decline in customer terms.
	Failure *PaymentFailure `json:"failure,omitempty"`
}

type ConfirmCheckoutSessionRequest struct {
	CustomerID string         `json:"customer_id"`
	Payment    ConfirmPayment `json:"payment"`
}

type ConfirmPayment struct {
	Capture   *CustodianCaptureReference `json:"capture,omitempty"`
	Rail      string                     `json:"rail"`      // Must match session rail
	Signature string                     `json:"signature"` // Solana transaction signature
	Wallet    string                     `json:"wallet"`    // Solana wallet that signed
}

type EffectiveTier struct {
	Group       string `json:"group"`
	Entitlement string `json:"entitlement"`
	DisplayName string `json:"display_name"`
	TierRank    int    `json:"tier_rank"`
	ProductID   string `json:"product_id"`
	ProductKey  string `json:"product_key"`
}

// CustodianCaptureReference is a vendor session token, not a card or payment
// authority. It is accepted only by the exact owned setup session that issued it.
type CustodianCaptureReference struct {
	CustodianID uuid.UUID `json:"custodian_id"`
	SessionID   string    `json:"session_id"`
	Token       string    `json:"token"`
}

func (CustodianCaptureReference) String() string   { return "[private custodian capture reference]" }
func (CustodianCaptureReference) GoString() string { return "[private custodian capture reference]" }

// CustodianCaptureAction initializes vendor-owned browser fields. Its scoped
// authorization is private, short lived, and never a shared cache/log value.
type CustodianCaptureAction struct {
	Kind             string    `json:"kind"`
	CustodianID      uuid.UUID `json:"custodian_id"`
	SessionID        string    `json:"session_id"`
	CustomerID       string    `json:"customer_id"`
	APIBaseURL       string    `json:"api_base_url"`
	SDKURL           string    `json:"sdk_url"`
	PublicAPIKey     string    `json:"public_api_key"`
	SDKAuthorization string    `json:"sdk_authorization"`
	ExpiresAt        time.Time `json:"expires_at"`
}

func (CustodianCaptureAction) String() string   { return "[private custodian capture action]" }
func (CustodianCaptureAction) GoString() string { return "[private custodian capture action]" }

// PaymentFailure is the customer-facing reason a card payment was definitely
// declined. Reason is provider-neutral (incorrect_cvc, incorrect_zip,
// incorrect_address, incorrect_number, expired_card, invalid_expiry,
// insufficient_funds, over_limit, card_not_supported, currency_not_supported,
// processing_error, try_again_later, authentication_required, do_not_honor,
// generic_decline); fraud-related declines always read generic_decline.
// Field names the card field to correct: cvc, postal_code, number, expiry or "".
type PaymentFailure struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}
