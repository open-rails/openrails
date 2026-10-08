package billing

import "time"

// CreateCheckoutSessionParams mints a checkout session: one customer buying
// one price, handed to that customer's browser. Supply exactly one of PriceID
// or PriceKey.
type CreateCheckoutSessionParams struct {
	Customer   CheckoutCustomerIdentity `json:"customer"`
	PriceID    PriceID                  `json:"price_id"`
	PriceKey   string                   `json:"price_key"`
	ProductKey string                   `json:"product_key,omitempty"`
	// Amount selects the deposit in the price currency's native units. Required
	// for customer_amount prices; forbidden for fixed prices. The accepted
	// amount is retained on retries and cannot change within a session.
	Amount *int64 `json:"amount,string,omitempty"`
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
