package billing

import (
	"time"

	"github.com/open-rails/openrails/catalog"
)

// OrderStatus is where an order stands, as Stripe's Order: open (it takes
// payment or awaits the customer's action), processing (the provider has its
// payment), complete (paid and fulfilled), canceled or expired.
type OrderStatus string

const (
	OrderOpen       OrderStatus = "open"
	OrderProcessing OrderStatus = "processing"
	OrderComplete   OrderStatus = "complete"
	OrderCanceled   OrderStatus = "canceled"
	OrderExpired    OrderStatus = "expired"
)

// OrderPaymentStatus is where an order's payment stands, as a Stripe
// PaymentIntent's: a decline returns it to requires_payment_method with
// LastPaymentError.
type OrderPaymentStatus string

const (
	OrderPaymentRequiresPaymentMethod OrderPaymentStatus = "requires_payment_method"
	// OrderPaymentRequiresAction waits for the customer: NextAction says how.
	OrderPaymentRequiresAction OrderPaymentStatus = "requires_action"
	OrderPaymentProcessing     OrderPaymentStatus = "processing"
	OrderPaymentSucceeded      OrderPaymentStatus = "succeeded"
)

// OrderOrigin says who created an order.
type OrderOrigin string

const (
	OrderOriginCustomer OrderOrigin = "customer"
	OrderOriginMerchant OrderOrigin = "merchant"
)

// MaxOrderLines bounds one order.
const MaxOrderLines = 100

// OrderLineParams is one line to buy: a price and how many. Quantity is
// seats on a per-seat price (omitted: its minimum), units of a consumable
// product (omitted: 1) and 1 on another one-time price; any other recurring
// price takes none (quantity_not_allowed).
type OrderLineParams struct {
	PriceID  PriceID `json:"price_id"`
	Quantity *int    `json:"quantity,omitempty"`
}

// OrderPaymentParams names how to pay: exactly one of a saved card
// (PaymentMethodID) or a card just entered (Token).
type OrderPaymentParams struct {
	PaymentMethodID *PaymentMethodID `json:"payment_method_id,omitempty"`
	// Token is the single-use output of the PSP's own card fields: a Stripe
	// pm_ from Elements, an NMI Collect.js payment_token. The card is saved and
	// charged in the same call; the charge is its storing transaction.
	Token string `json:"token,omitempty"`
	// PSPID is the PSP whose fields made Token; optional while one payment
	// option takes new cards.
	PSPID          *PSPID          `json:"psp_id,omitempty"`
	BillingDetails *BillingDetails `json:"billing_details,omitempty"`
}

// PreviewOrderParams prices lines without creating an order.
type PreviewOrderParams struct {
	Lines []OrderLineParams `json:"lines"`
}

// CreateOrderParams creates an order and, with Payment, pays it in the same
// call. ExpectedTotal is required with Payment: a different total refuses
// the order with order_total_changed. Reusable keeps a new card for one-click
// buys where that needs the customer's opt-in (billing or card country in the
// EEA or the UK); elsewhere every new card is kept.
type CreateOrderParams struct {
	Lines         []OrderLineParams   `json:"lines"`
	ExpectedTotal *int64              `json:"expected_total,omitempty,string"`
	Payment       *OrderPaymentParams `json:"payment,omitempty"`
	Reusable      *bool               `json:"reusable,omitempty"`
}

// PayOrderParams pays an open order; Reusable as in CreateOrderParams.
type PayOrderParams struct {
	Payment       OrderPaymentParams `json:"payment"`
	ExpectedTotal int64              `json:"expected_total,string"`
	Reusable      *bool              `json:"reusable,omitempty"`
}

// Order is one purchase: frozen lines and total. It is read and paid by its
// own customer; its id is not a credential.
type Order struct {
	ID         OrderID     `json:"id"`
	CustomerID CustomerID  `json:"customer_id"`
	Origin     OrderOrigin `json:"origin"`
	Status     OrderStatus `json:"status"`
	// Number is the document number, given when the order completes.
	Number   *string      `json:"number"`
	Currency string       `json:"currency"`
	Total    int64        `json:"total,string"`
	Lines    []OrderLine  `json:"lines"`
	Payment  OrderPayment `json:"payment"`
	// PaymentOptions are the PSPs that can take this order while it is open.
	PaymentOptions []OrderPaymentOption `json:"payment_options"`
	ExpiresAt      time.Time            `json:"expires_at"`
	CompletedAt    *time.Time           `json:"completed_at"`
	CanceledAt     *time.Time           `json:"canceled_at"`
	ExpiredAt      *time.Time           `json:"expired_at"`
	CreatedAt      time.Time            `json:"created_at"`
}

// OrderPayment is an order's payment, as a Stripe PaymentIntent reads.
type OrderPayment struct {
	Status OrderPaymentStatus `json:"status"`
	// NextAction is what the customer completes while Status is
	// requires_action.
	NextAction *NextAction `json:"next_action"`
	// LastPaymentError is why the latest attempt failed.
	LastPaymentError *PaymentFailure `json:"last_payment_error"`
	// PaymentMethodID is the card the latest attempt charged.
	PaymentMethodID *PaymentMethodID `json:"payment_method_id"`
	// PaymentID is the money that moved.
	PaymentID *PaymentID `json:"payment_id"`
}

// OrderLine is one line of an order, and what paying it produced.
type OrderLine struct {
	ID          OrderLineID `json:"id"`
	PriceID     PriceID     `json:"price_id"`
	ProductID   ProductID   `json:"product_id"`
	Description string      `json:"description"`
	// Quantity is null on a recurring line of a price without seats.
	Quantity   *int              `json:"quantity"`
	UnitAmount int64             `json:"unit_amount,string"`
	Amount     int64             `json:"amount,string"`
	Ownership  catalog.Ownership `json:"ownership"`
	// BillingIntervalHours is set on a recurring line, whose subscription
	// renews every interval (for Quantity seats on a per-seat price).
	BillingIntervalHours *int             `json:"billing_interval_hours"`
	AccessDurationHours  *int             `json:"access_duration_hours"`
	SubscriptionID       *SubscriptionID  `json:"subscription_id"`
	ProductAccessID      *ProductAccessID `json:"product_access_id"`
}

// OrderPaymentOption is one PSP that can take an order's lines, and what it
// accepts: saved_card or new_card.
type OrderPaymentOption struct {
	PSPID   PSPID    `json:"psp_id"`
	Rail    string   `json:"rail"`
	Accepts []string `json:"accepts"`
}

// OrderPreview prices lines as an order would freeze them.
type OrderPreview struct {
	Currency       string               `json:"currency"`
	Total          int64                `json:"total,string"`
	Lines          []OrderPreviewLine   `json:"lines"`
	PaymentOptions []OrderPaymentOption `json:"payment_options"`
}

// OrderPreviewLine is one priced line, or why it cannot be bought.
type OrderPreviewLine struct {
	PriceID              PriceID           `json:"price_id"`
	ProductID            ProductID         `json:"product_id"`
	Description          string            `json:"description"`
	Quantity             *int              `json:"quantity"`
	UnitAmount           int64             `json:"unit_amount,string"`
	Amount               int64             `json:"amount,string"`
	Ownership            catalog.Ownership `json:"ownership"`
	BillingIntervalHours *int              `json:"billing_interval_hours"`
	AccessDurationHours  *int              `json:"access_duration_hours"`
	Refusal              *OrderLineRefusal `json:"refusal"`
}

// OrderLineRefusal is why a line cannot be bought. Code is already_owned,
// unavailable, currency_mismatch, quantity_invalid or quantity_not_allowed. An already_owned line
// names its owner (a subscription, product access or an unpaid order) and a
// hint: change (change that subscription instead) or resume (pay that order,
// or resume that subscription).
type OrderLineRefusal struct {
	Code    string  `json:"code"`
	Message string  `json:"message"`
	OwnedBy *string `json:"owned_by"`
	Hint    *string `json:"hint"`
}

// OrderListParams selects orders, newest first; every filter is optional.
// PriceID names orders with a line on that price. A customer's own list
// ignores CustomerID. IDs instead reads 1 to MaxBatchItems named orders in
// one page; unknown ones are absent.
type OrderListParams struct {
	IDs        []OrderID
	CustomerID CustomerID
	PriceID    PriceID
	Status     OrderStatus
	PageRequest
}
