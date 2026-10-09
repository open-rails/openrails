package billing

import "time"

// CheckoutOption is one way checkout can sell a price now: the PSP is armed
// and its rail can make this kind of sale (#1078). Driver and PublicConfig are
// what a browser needs to render it; an empty Driver means no browser flow can
// execute the option.
type CheckoutOption struct {
	// PSP is the PSP's key: the value a checkout attempt's payment.psp names.
	PSP   string `json:"psp"`
	PSPID PSPID  `json:"psp_id"`
	Rail  Rail   `json:"rail"`
	// Mode is one_off or subscription.
	Mode string `json:"mode"`
	// Driver is collect_js, card, stripe_elements, redirect or solana_pay.
	// card: the page posts the card itself to OpenRails (the PSP's card_entry
	// is server) and loads no gateway script.
	Driver string `json:"driver,omitempty"`
	// PublicConfig holds browser-safe values: the PSP's public keys, and for
	// Solana token_symbol, token_name and network.
	PublicConfig map[string]string `json:"public_config,omitempty"`
	// Status is PSPTemporarilyUnavailable when the option's PSP could
	// not be checked just now; it has no driver. RetryAfter is in seconds.
	Status     string `json:"status,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
}

// CheckoutOptionListParams names the price ListCheckoutOptions lists the
// options of: PriceID, or the pair ProductKey + PriceKey.
type CheckoutOptionListParams struct {
	PriceID    PriceID
	PriceKey   string
	ProductKey string
}

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
	// Supply PriceID or the pair ProductKey + PriceKey. Keys are always opaque, even
	// when they resemble ids. Accepted retries retain the original offer.
	PriceID    PriceID `json:"price_id"`
	PriceKey   string  `json:"price_key"`
	ProductKey string  `json:"product_key,omitempty"`
	// Amount selects the deposit in the price currency's native units. Required
	// for customer_amount prices; forbidden for fixed prices. The accepted
	// amount is retained on retries and cannot change within a session.
	Amount *int64 `json:"amount,string,omitempty"`
	// AutoRenew controls this order, independently of the price's recurring
	// cadence. Omitted means true for recurring prices; false buys the initial
	// term without scheduling another charge. Accepted retries retain the choice.
	AutoRenew *bool `json:"auto_renew,omitempty"`
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
	// PSP is the PSP's key (CheckoutOption.PSP); empty lets the merchant's
	// checkout routing pick.
	PSP string `json:"psp,omitempty"`
	// PaymentMethodID is a saved card of the customer's; PaymentToken a card
	// the PSP's browser fields tokenized.
	PaymentMethodID PaymentMethodID `json:"payment_method_id,omitzero"`
	PaymentToken    string          `json:"payment_token,omitempty"`
	// BillingDetails go with a new card. CCBill needs the name, postal code
	// and country; the verified email comes from CheckoutCustomerIdentity.
	BillingDetails *BillingDetails `json:"billing_details,omitempty"`
	// TokenSymbol (USDC, SOL), Flow (transfer_request or
	// transaction_request) and Wallet pay on Solana.
	TokenSymbol string `json:"token_symbol,omitempty"`
	Flow        string `json:"flow,omitempty"`
	Wallet      string `json:"wallet,omitempty"`
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
