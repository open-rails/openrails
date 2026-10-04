package billing

import "time"

// CreateCheckoutSessionRequest mints a checkout session: one customer buying
// one price, handed to that customer's browser. Supply exactly one of PriceID
// or PriceKey.
type CreateCheckoutSessionRequest struct {
	Customer CheckoutCustomerIdentity `json:"customer"`
	PriceID  PriceID                  `json:"price_id"`
	PriceKey string                   `json:"price_key"`
	// SuccessURL is where a redirect step returns the buyer; its origin must
	// be one of Config.ReturnOrigins.
	SuccessURL string `json:"success_url"`
}

// CheckoutSessionLink is a minted checkout session. ID (ocs_) is the bearer
// credential for reading and paying it: hand it to the buyer's browser only.
// URL is the payment page for the session (Config.HTTP.Checkout.PageURL#ID),
// null when the app renders checkout itself.
type CheckoutSessionLink struct {
	ID        string    `json:"id"`
	URL       *string   `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}
