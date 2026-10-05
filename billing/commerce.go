package billing

import "time"

// CheckoutOption is one way checkout can sell a price now: the PSP is armed
// and its rail can make this kind of sale (#1078). Driver and PublicConfig are
// what a browser needs to render it; an empty Driver means no browser flow can
// execute the option.
type CheckoutOption struct {
	// Selector is the checkout payment.rail value (the PSP key).
	Selector string `json:"selector"`
	PSPID    PSPID  `json:"psp_id"`
	Rail     string `json:"rail"`
	// Mode is one_off or subscription.
	Mode string `json:"mode"`
	// Driver is collect_js, card, stripe_elements, redirect or solana_pay.
	// card: the page posts the card itself to OpenRails (the PSP's card_entry
	// is server) and loads no gateway script.
	Driver string `json:"driver,omitempty"`
	// PublicConfig holds browser-safe values: the PSP's public keys, and for
	// Solana token_symbol, token_name and network.
	PublicConfig map[string]string `json:"public_config,omitempty"`
	// Status is CheckoutPSPTemporarilyUnavailable when the option's PSP could
	// not be checked just now; it has no driver. RetryAfter is in seconds.
	Status     string `json:"status,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
}

// GetCheckoutConfigParams selects the price whose options GetCheckoutConfig
// lists: at most one of PriceID or PriceKey.
type GetCheckoutConfigParams struct {
	PriceID  PriceID
	PriceKey string
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
	// Options are the ways checkout can sell the price the request named, in
	// routing order; null when it named none.
	Options []CheckoutOption `json:"options"`
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
	PSPID PSPID `json:"psp_id"`
	// Key is the checkout payment.rail selector.
	Key string `json:"key"`
	// Rail is the gateway kind: nmi, ccbill, stripe or solana.
	Rail string `json:"rail"`
	// Custodian holds the card: "psp", or the third party whose page tokenizes it.
	Custodian   string `json:"custodian"`
	DisplayName string `json:"display_name"`
	// Flow is how a browser drives this PSP: tokenize, card, elements, redirect
	// or wallet. card: the page posts the card to OpenRails, which vaults it.
	// elements: the page saves the card with the PSP's own fields and checkout
	// charges the saved card, with authentication in the page.
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

// CheckoutPSPTemporarilyUnavailable marks a PSP (or option) whose
// credentials could not be checked just now; retry after RetryAfter seconds.
const CheckoutPSPTemporarilyUnavailable = "temporarily_unavailable"

// CheckoutCustomerIdentity is the buyer as the host knows it.
type CheckoutCustomerIdentity struct {
	ID            CustomerID `json:"id"`
	VerifiedEmail string     `json:"verified_email"`
	Username      string     `json:"username"`
	// ClientIP is the address the customer's request came from, as the host
	// resolved it behind its trusted proxies. Checkouts count declined cards
	// against it, as the customer HTTP routes do; without it a host's card
	// testers are counted per customer only.
	ClientIP string `json:"client_ip"`
}

// CreateCheckoutAttemptParams charges one price for a merchant-owned
// customer now, relaying that customer's pay action: a recurring price is
// enrolled and charged in this call. The server derives one-off versus
// recurring from the price. Retries with the same IdempotencyKey never charge
// twice. Merchant credentials authorize the host; they do not prove customer
// interaction, so never call this unattended to establish a stored-card
// agreement. To hand a purchase to the customer instead, create a checkout
// session.
type CreateCheckoutAttemptParams struct {
	Customer CheckoutCustomerIdentity `json:"customer"`
	// Supply exactly one of PriceID or PriceKey. Keys are always opaque, even
	// when they resemble ids. Accepted retries retain the original offer.
	PriceID  PriceID `json:"price_id"`
	PriceKey string  `json:"price_key"`
	// Entitlement optionally binds admission to the opaque resource the host
	// showed. OpenRails verifies the selected product grants this key.
	Entitlement    string                 `json:"entitlement"`
	OfferKind      OfferKind              `json:"offer_kind"`
	PaymentOptions CheckoutPaymentOptions `json:"payment"`
	Metadata       map[string]string      `json:"metadata"`
	IdempotencyKey string                 `json:"-"`
	// SuccessURL and CancelURL bring the buyer back from a redirect step
	// (Stripe or CCBill hosted pages); their origins must be one of
	// Config.ReturnOrigins.
	SuccessURL string `json:"success_url"`
	CancelURL  string `json:"cancel_url"`
}

// CheckoutPaymentOptions is how an attempt pays: the PSP, and the instrument
// or wallet. A Client cannot carry a card number: cards are entered on a
// checkout session.
type CheckoutPaymentOptions struct {
	PSPID           PSPID  `json:"psp_id,omitzero"`
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
	NameOnCard string `json:"name_on_card"` // Full name as it appears on the card.
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

// CheckoutAttemptStatus is where an attempt stands. processing: the provider
// outcome is not known yet; read the attempt again.
type CheckoutAttemptStatus string

const (
	CheckoutAttemptCreated        CheckoutAttemptStatus = "created"
	CheckoutAttemptRequiresAction CheckoutAttemptStatus = "requires_action"
	CheckoutAttemptProcessing     CheckoutAttemptStatus = "processing"
	CheckoutAttemptSucceeded      CheckoutAttemptStatus = "succeeded"
	CheckoutAttemptFailed         CheckoutAttemptStatus = "failed"
	CheckoutAttemptExpired        CheckoutAttemptStatus = "expired"
	CheckoutAttemptCanceled       CheckoutAttemptStatus = "canceled"
)

// CheckoutAttempt is one charge of one price on one PSP (chk_ id). Amount is
// in the currency's native unit, a decimal string over HTTP. With status
// requires_action the buyer completes NextAction, or the card payment's
// Operation authentication (3-D Secure).
type CheckoutAttempt struct {
	Object          string                `json:"object"`
	ID              CheckoutAttemptID     `json:"id"`
	CustomerID      CustomerID            `json:"customer_id"`
	Status          CheckoutAttemptStatus `json:"status"`
	Mode            string                `json:"mode"` // one_off, subscription or payment_method (a card setup)
	PriceID         *PriceID              `json:"price_id"`
	Amount          *int64                `json:"amount,string"`
	Currency        *string               `json:"currency"`
	PaymentID       *PaymentID            `json:"payment_id"`
	SubscriptionID  *SubscriptionID       `json:"subscription_id"`
	PaymentMethodID *PaymentMethodID      `json:"payment_method_id"`
	NextAction      *NextAction           `json:"next_action"`
	Operation       *PaymentOperation     `json:"operation"`
	// Failure explains a definite decline in customer terms.
	Failure   *PaymentFailure   `json:"failure"`
	ExpiresAt *time.Time        `json:"expires_at"`
	CreatedAt time.Time         `json:"created_at"`
	Metadata  map[string]string `json:"metadata"`
}

// NextAction is the step the buyer takes to finish a payment that requires
// action. redirect_to_url: open URL (a Stripe or CCBill page) in the top
// window; it returns to the success URL. solana_pay: show URL (a solana: Solana
// Pay link) as a QR code or wallet link. solana_sign_transactions: the wallet
// signs and sends Transactions (base64, unsigned) in order, then the attempt is
// confirmed with each signature.
type NextAction struct {
	Type         string   `json:"type"`
	URL          *string  `json:"url"`
	Transactions []string `json:"transactions"`
}

// ConfirmCheckoutAttemptParams completes a Solana attempt the buyer's wallet
// signed.
type ConfirmCheckoutAttemptParams struct {
	Signature string `json:"signature"`
	Wallet    string `json:"wallet"`
}

// PaymentFailure is the customer-facing reason a card payment was definitely
// declined. Reason is provider-neutral (incorrect_cvc, incorrect_zip,
// incorrect_address, incorrect_number, expired_card, invalid_expiry,
// insufficient_funds, over_limit, card_not_supported, currency_not_supported,
// processing_error, try_again_later, authentication_required, do_not_honor,
// generic_decline); fraud-related declines always read generic_decline.
// Message is OpenRails' copy for the buyer. Field names the card field to
// correct: cvc, postal_code, number, expiry or "". DeclineReason.Failure
// renders one.
type PaymentFailure struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}
