package billing

import "time"

// OrderCheckoutParams asks for a hosted checkout on a merchant order: a
// short-lived checkout URL on the deployment's checkout host where the
// customer pays, then returns to SuccessURL. {ORDER_ID} in either URL
// becomes the order's id. Fulfil on order.completed, never the redirect.
type OrderCheckoutParams struct {
	SuccessURL string `json:"success_url"`
	// CancelURL is the page's back link; without one the page shows none.
	CancelURL *string `json:"cancel_url,omitempty"`
	// ExpiresAt is 30 minutes to 24 hours away and no later than the order;
	// omitted is an hour. An order created with no ExpiresAt of its own
	// expires with its checkout.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// SavedPaymentMethods offers the customer's saved cards they allowed to
	// be shown again. Set it only when your site signed the customer in.
	SavedPaymentMethods bool `json:"saved_payment_methods,omitempty"`
}

// OrderCheckout is an order's hosted checkout.
type OrderCheckout struct {
	// URL is the checkout URL. Only the response that created it carries
	// it; every other read is null.
	URL                 *string   `json:"url"`
	ExpiresAt           time.Time `json:"expires_at"`
	SuccessURL          string    `json:"success_url"`
	CancelURL           *string   `json:"cancel_url"`
	SavedPaymentMethods bool      `json:"saved_payment_methods"`
}
