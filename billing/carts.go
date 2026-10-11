package billing

import "time"

// MaxCartLines bounds one cart, as MaxOrderLines bounds an order.
const MaxCartLines = MaxOrderLines

// Cart is the signed-in customer's cart, priced on every read as an order
// preview prices its lines. One per customer of a merchant, so every site of
// the merchant shares it. It reserves, claims and charges nothing; it expires
// 30 days after its last change, and an order made from it (cart: true)
// takes its lines.
type Cart struct {
	Currency       string               `json:"currency"`
	Total          int64                `json:"total,string"`
	Lines          []OrderPreviewLine   `json:"lines"`
	PaymentOptions []OrderPaymentOption `json:"payment_options"`
	// LastChangedAt and ExpiresAt are null while the cart is empty.
	LastChangedAt *time.Time `json:"last_changed_at"`
	ExpiresAt     *time.Time `json:"expires_at"`
}

// SetCartLineParams puts a price in the cart, or changes its line. Quantity
// is as on an order line (omitted: the price's default); 0 removes the line.
// Amount is the customer's choice on a price that takes one. A price of a
// product already in the cart replaces that product's line.
type SetCartLineParams struct {
	Quantity *int   `json:"quantity,omitempty"`
	Amount   *int64 `json:"amount,omitempty,string"`
}
