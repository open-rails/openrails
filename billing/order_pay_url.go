package billing

import "time"

// OrderPayURLParams asks for a merchant order's pay URL: a short-lived URL on
// the deployment's checkout origin where the customer pays, then returns to
// SuccessURL. {ORDER_ID} in either URL becomes the order's id. Fulfil on
// order.completed, never the redirect.
type OrderPayURLParams struct {
	// Embedded: the merchant's page frames the pay URL (billing-ui's
	// EmbeddedPayment), only on the origins its settings register
	// (checkout.embed_origins). The frame tells its page when the order
	// completes; that page goes to SuccessURL unless it handles the
	// completion itself. Otherwise the customer is sent to the pay URL.
	Embedded   bool   `json:"embedded,omitempty"`
	SuccessURL string `json:"success_url"`
	// CancelURL is the page's back link; without one the page shows none.
	CancelURL *string `json:"cancel_url,omitempty"`
	// ExpiresAt is 30 minutes to 24 hours away and no later than the order;
	// omitted is an hour. An order created with no ExpiresAt of its own
	// expires with its pay URL.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// SavedPaymentMethods offers the customer's saved cards they allowed to
	// be shown again. Set it only when your site signed the customer in.
	SavedPaymentMethods bool `json:"saved_payment_methods,omitempty"`
}

// MyOrderPayURLParams asks POST /v1/me/orders for the order's pay URL,
// embedded (the only kind a customer asks for) and framed by the merchant's
// registered origins. It returns nowhere: the frame tells its page when the
// order completes, and redirect PSPs come back inside it. Only the
// merchant's own person, signed in, asks for one: it offers their saved
// cards. It lasts as long as the order is open.
type MyOrderPayURLParams struct {
	Embedded bool `json:"embedded"`
}
