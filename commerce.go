package openrails

import "time"

type CheckoutRailOption struct {
	Selector string `json:"selector"`
	PSPID    string `json:"psp_id"`
	Rail     string `json:"rail"`
	Mode     string `json:"mode"`
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
	// Key is the checkout payment.rail selector.
	Key string `json:"key"`
	// Rail is the gateway kind: nmi, ccbill, stripe or solana.
	Rail string `json:"rail"`
	// Custodian holds the card: "psp", or the third party whose page tokenizes it.
	Custodian   string `json:"custodian"`
	DisplayName string `json:"display_name"`
	// Flow is how a browser drives this PSP: tokenize, redirect or wallet.
	Flow string `json:"flow"`
	// Config holds whitelisted public values, such as a tokenization key.
	Config map[string]string `json:"config,omitempty"`
}

type CheckoutCustomerIdentity struct {
	ID            CustomerID `json:"id"`
	VerifiedEmail string     `json:"verified_email"`
	Username      string     `json:"username"`
}

// CreateCheckoutSessionRequest creates a purchase for a merchant-owned customer.
// IdempotencyKey must identify this checkout attempt across retries.
type CreateCheckoutSessionRequest struct {
	Customer       CheckoutCustomerIdentity `json:"customer"`
	SubscriptionID SubscriptionID           `json:"subscription_id,omitzero"`
	NewPriceID     PriceID                  `json:"new_price_id,omitzero"`
	PriceID        PriceID                  `json:"price_id"`
	Mode           string                   `json:"mode"` // "one_off" or "subscription" (optional, inferred from price)
	Payment        CheckoutPayment          `json:"payment"`
	Metadata       map[string]string        `json:"metadata"`
	IdempotencyKey string                   `json:"-"`
	SuccessURL     string                   `json:"success_url"` // Required for Stripe hosted checkout
	CancelURL      string                   `json:"cancel_url"`  // Required for Stripe hosted checkout
}

type CheckoutPayment struct {
	Rail            string          `json:"rail"`                       // "nmi", "ccbill", "solana", "stripe"
	PaymentMethodID PaymentMethodID `json:"payment_method_id,omitzero"` // For returning customers with saved payment methods
	PaymentToken    string          `json:"payment_token"`              // For new card tokenization (NMI Collect.js)

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
	ID             CheckoutSessionID `json:"id"`
	Status         string            `json:"status"` // "created", "requires_action", "succeeded", "failed", "expired", "canceled"
	Mode           string            `json:"mode"`   // "subscription", "one_off"
	PriceID        PriceID           `json:"price_id"`
	Amount         int64             `json:"amount,string"`
	Currency       string            `json:"currency"`
	PaymentStatus  string            `json:"payment_status"` // "unpaid", "paid", "no_payment_required"
	ClientSecret   *string           `json:"client_secret"`
	URL            *string           `json:"url"` // Redirect URL for CCBill/Stripe
	SubscriptionID *SubscriptionID   `json:"subscription_id"`
	PaymentID      *PaymentID        `json:"payment_id"`
	ExpiresAt      *time.Time        `json:"expires_at,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	Metadata       map[string]string `json:"metadata"`
	RailData       map[string]any    `json:"rail_data"` // Rail-specific response data
}

type ConfirmCheckoutSessionRequest struct {
	CustomerID CustomerID     `json:"customer_id"`
	Payment    ConfirmPayment `json:"payment"`
}

type ConfirmPayment struct {
	Rail      string `json:"rail"`      // Must match session rail
	Signature string `json:"signature"` // Solana transaction signature
	Wallet    string `json:"wallet"`    // Solana wallet that signed
}

type EffectiveTier struct {
	Group       string    `json:"group"`
	Entitlement string    `json:"entitlement"`
	DisplayName string    `json:"display_name"`
	TierRank    int       `json:"tier_rank"`
	ProductID   ProductID `json:"product_id"`
	ProductKey  string    `json:"product_key"`
}
