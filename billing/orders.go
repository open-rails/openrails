package billing

import (
	"time"

	"github.com/open-rails/openrails/catalog"
)

// OrderStatus is where an order stands, as Stripe's Order: open (it takes
// payment or awaits the customer's action), processing (the provider has its
// payment), complete (paid and fulfilled), canceled or expired (a purchase
// closed unpaid), void (a renewal its subscription's end left unowed, or a
// usage order the merchant voided) or uncollectible (a usage order the
// merchant wrote off; collecting it reopens it).
type OrderStatus string

const (
	OrderOpen       OrderStatus = "open"
	OrderProcessing OrderStatus = "processing"
	OrderComplete   OrderStatus = "complete"
	OrderCanceled   OrderStatus = "canceled"
	OrderExpired    OrderStatus = "expired"
	OrderVoid       OrderStatus = "void"
	// OrderUncollectible is a usage order written off.
	OrderUncollectible OrderStatus = "uncollectible"
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
	// OrderOriginSubscription is a renewal of an engine-owned subscription:
	// one per period, due when it starts. Its automatic collection and
	// retries are its attempts; a failed one leaves it open for the customer
	// to pay.
	OrderOriginSubscription OrderOrigin = "subscription"
	// OrderOriginUsage is owed usage billed in arrears: one per customer,
	// currency and period (or, past the customer's threshold, the period so
	// far), one line per accrual source, due after the customer's net terms.
	// A funded balance and remittances may pay part of it.
	OrderOriginUsage OrderOrigin = "usage"
)

// MaxOrderLines bounds one order.
const MaxOrderLines = 100

// OrderLineParams is one line to buy: a price and how many. Quantity is
// seats on a per-seat price (omitted: its minimum), units of a consumable
// product (omitted: 1) and 1 on another one-time price; any other recurring
// price takes none (quantity_not_allowed). Amount is what the customer
// chose on a price whose customer picks it (a deposit, a top-up), within
// its bounds; a fixed price takes none.
type OrderLineParams struct {
	PriceID  PriceID `json:"price_id"`
	Quantity *int    `json:"quantity,omitempty"`
	Amount   *int64  `json:"amount,omitempty,string"`
}

// OrderPaymentParams names how to pay: exactly one of a saved card
// (PaymentMethodID), a card just entered (Token), a Solana wallet
// (TokenSymbol, the token it pays in) or the PSP's hosted page (ReturnURL).
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
	// TokenSymbol pays from a Solana wallet in this token (one of GET
	// /v1/me/config's payment.solana.tokens): the order answers requires_action
	// with a solana_pay next_action, a Solana Pay transaction request the
	// wallet completes.
	TokenSymbol string `json:"token_symbol,omitempty"`
	// ReturnURL pays on the PSP's hosted page (Stripe Checkout, an option
	// that accepts redirect): the order answers requires_action with a
	// redirect_to_url next action, and the customer comes back to ReturnURL.
	ReturnURL string `json:"return_url,omitempty"`
}

// PreviewMyOrderParams prices lines for the signed-in customer without
// creating an order: Lines, or the customer's cart (Cart true).
type PreviewMyOrderParams struct {
	Lines []OrderLineParams `json:"lines,omitempty"`
	Cart  bool              `json:"cart,omitempty"`
}

// CreateMyOrderParams creates the signed-in customer's order and, with
// Payment, pays it in the same call. ExpectedTotal is required with Payment:
// a different total refuses the order with order_total_changed. Reusable
// keeps a new card for one-click buys where that needs the customer's opt-in
// (billing or card country in the EEA or the UK); elsewhere every new card is
// kept. Without Payment the order is open for POST /v1/me/orders/{id}/pay,
// and asking again for the same Lines answers the customer's open order of
// them instead of a second one. Cart true buys the customer's cart instead of
// Lines: the order takes the cart's lines in the transaction that creates it.
type CreateMyOrderParams struct {
	Lines         []OrderLineParams   `json:"lines,omitempty"`
	Cart          bool                `json:"cart,omitempty"`
	ExpectedTotal *int64              `json:"expected_total,omitempty,string"`
	Payment       *OrderPaymentParams `json:"payment,omitempty"`
	Reusable      *bool               `json:"reusable,omitempty"`
	// PayURL asks, instead of Payment, for the order's embedded pay URL:
	// Order.PayURL on the payment origin, fresh on every answer (the last
	// one stops working).
	PayURL *MyOrderPayURLParams `json:"pay_url,omitempty"`
}

// PreviewOrderParams prices lines for one customer, as a merchant order
// would freeze them.
type PreviewOrderParams struct {
	CustomerID CustomerID        `json:"customer_id"`
	Lines      []OrderLineParams `json:"lines"`
}

// CreateOrderParams creates a merchant order: an unpaid order for one
// customer, which they pay on the merchant's own site
// (POST /v1/me/orders/{id}/pay), or, with PayURL, at its pay URL. It never
// charges. ExpiresAt defaults to 30 days, or to the pay URL's expiry with
// PayURL, and is at most 120 days away. IdempotencyKey is required: a retry
// with it answers the same order.
type CreateOrderParams struct {
	CustomerID CustomerID        `json:"customer_id"`
	Lines      []OrderLineParams `json:"lines"`
	ExpiresAt  *time.Time        `json:"expires_at,omitempty"`
	// PayURL asks for the order's pay URL (Order.PayURL), where the
	// customer pays. The deployment must serve a payment origin.
	PayURL         *OrderPayURLParams `json:"pay_url,omitempty"`
	IdempotencyKey string             `json:"-"`
}

// PayOrderParams pays an open order; Reusable as in CreateMyOrderParams.
type PayOrderParams struct {
	Payment       OrderPaymentParams `json:"payment"`
	ExpectedTotal int64              `json:"expected_total,string"`
	Reusable      *bool              `json:"reusable,omitempty"`
}

// ConfirmOrderParams confirms an order awaiting its customer. Signature is
// the transaction a Solana wallet sent for it: OpenRails reads it at once.
type ConfirmOrderParams struct {
	Signature *string `json:"signature,omitempty"`
}

// OrderCollectionOwner names the authority allowed to collect an order.
// Provider and NMI schedule orders cannot be paid through generic order APIs.
type OrderCollectionOwner string

const (
	OrderCollectionEngine      OrderCollectionOwner = "engine"
	OrderCollectionNMISchedule OrderCollectionOwner = "nmi_schedule"
	OrderCollectionProvider    OrderCollectionOwner = "provider"
)

// Order is one bill with frozen lines and total. Its collection owner decides
// how it is paid; its id is not a credential.
type Order struct {
	ID         OrderID     `json:"id"`
	CustomerID CustomerID  `json:"customer_id"`
	Origin     OrderOrigin `json:"origin"`
	Status     OrderStatus `json:"status"`
	// CollectionOwner is frozen when the bill is accepted. Only engine
	// orders accept generic pay, confirm and remittance commands.
	CollectionOwner OrderCollectionOwner `json:"collection_owner"`
	// CollectionPSPID is the account responsible for collection, when known.
	CollectionPSPID *PSPID `json:"collection_psp_id"`
	// ProviderBills are verified provider document references and settlement
	// facts. They never authorize local collection.
	ProviderBills []OrderProviderBill `json:"provider_bills"`
	// Number is the document number, given when the order completes.
	Number   *string      `json:"number"`
	Currency string       `json:"currency"`
	Total    int64        `json:"total,string"`
	Lines    []OrderLine  `json:"lines"`
	Payment  OrderPayment `json:"payment"`
	// PaymentOptions are the PSPs that can take this order while it is open.
	PaymentOptions []OrderPaymentOption `json:"payment_options"`
	// PayURL is where the customer pays the order on the payment origin,
	// /pay/<order>#<secret>; the secret stays in the fragment and is stored
	// hashed. Creation and a re-ask while the order is open answer it;
	// other reads are null.
	PayURL *string `json:"pay_url"`
	// PayURLExpiresAt is when the pay URL stops working; null without one.
	PayURLExpiresAt *time.Time `json:"pay_url_expires_at"`
	// Embedded: the merchant's page frames the pay URL.
	Embedded bool `json:"embedded"`
	// SuccessURL and CancelURL are where a merchant order's pay URL returns
	// and its back link; a customer's embedded one has neither.
	SuccessURL          *string `json:"success_url"`
	CancelURL           *string `json:"cancel_url"`
	SavedPaymentMethods bool    `json:"saved_payment_methods"`
	// Renewal is the subscription period a renewal order (origin
	// subscription) bills; null on any other.
	Renewal *OrderRenewal `json:"renewal"`
	// Usage is a usage order's period, collection and document fields; null
	// on any other.
	Usage *OrderUsage `json:"usage"`
	// ExpiresAt is when an unpaid purchase expires; a renewal has none.
	ExpiresAt   *time.Time `json:"expires_at"`
	CompletedAt *time.Time `json:"completed_at"`
	CanceledAt  *time.Time `json:"canceled_at"`
	ExpiredAt   *time.Time `json:"expired_at"`
	VoidedAt    *time.Time `json:"voided_at"`
	// UncollectibleAt is when a usage order was written off.
	UncollectibleAt *time.Time `json:"uncollectible_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

// OrderProviderBill is a verified provider document attached to an order.
// SettlementSource distinguishes gateway cash, credit, out_of_band or mixed
// settlement. A null source/settled_at means settlement is not yet verified.
type OrderProviderBill struct {
	PSPID            PSPID      `json:"psp_id"`
	Kind             string     `json:"kind"`
	Reference        string     `json:"reference"`
	SettlementSource *string    `json:"settlement_source"`
	NonCashAmount    int64      `json:"non_cash_amount,string"`
	SettledAt        *time.Time `json:"settled_at"`
}

// OrderRenewal is the period of a subscription a renewal order bills. It is
// due when the period starts; open past DueAt, it is overdue.
type OrderRenewal struct {
	SubscriptionID SubscriptionID `json:"subscription_id"`
	PeriodStartsAt time.Time      `json:"period_starts_at"`
	PeriodEndsAt   time.Time      `json:"period_ends_at"`
	DueAt          time.Time      `json:"due_at"`
}

// OrderUsage is what a usage order bills and how it is collected: the
// period of its charges, when it is due, how much of its total is paid
// (a funded balance and remittances pay part of it), and the document fields
// of the customer's invoice profile when it was billed.
type OrderUsage struct {
	PeriodStartsAt time.Time `json:"period_starts_at"`
	PeriodEndsAt   time.Time `json:"period_ends_at"`
	DueAt          time.Time `json:"due_at"`
	AmountPaid     int64     `json:"amount_paid,string"`
	AmountDue      int64     `json:"amount_due,string"`
	// CollectionMethod is charge_automatically (the currency's collection
	// card is charged) or send_invoice (the customer remits).
	CollectionMethod        InvoiceCollectionMethod `json:"collection_method"`
	NextCollectionAttemptAt *time.Time              `json:"next_collection_attempt_at"`
	PONumber                *string                 `json:"po_number"`
	Memo                    *string                 `json:"memo"`
	Tax                     map[string]any          `json:"tax"`
	BillingContacts         []InvoiceContact        `json:"billing_contacts"`
	// Delinquent: the order is overdue and its customer's grace has run out,
	// so their new usage in its currency is refused.
	Delinquent bool `json:"delinquent"`
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
	ID OrderLineID `json:"id"`
	// PriceID, ProductID and Ownership are what a purchased line sold; null
	// on a usage line, whose Source names its accrual source instead.
	PriceID     *PriceID   `json:"price_id"`
	ProductID   *ProductID `json:"product_id"`
	Source      *string    `json:"source"`
	Description string     `json:"description"`
	// Quantity is null on a recurring line of a price without seats.
	Quantity   *int               `json:"quantity"`
	UnitAmount int64              `json:"unit_amount,string"`
	Amount     int64              `json:"amount,string"`
	Ownership  *catalog.Ownership `json:"ownership"`
	// BillingIntervalHours is set on a recurring line, whose subscription
	// renews every interval (for Quantity seats on a per-seat price).
	BillingIntervalHours *int             `json:"billing_interval_hours"`
	AccessDurationHours  *int             `json:"access_duration_hours"`
	SubscriptionID       *SubscriptionID  `json:"subscription_id"`
	ProductAccessID      *ProductAccessID `json:"product_access_id"`
}

// OrderPaymentOption is one PSP that can take an order's lines, and what it
// accepts: saved_card and new_card on a card PSP; redirect (its hosted page)
// on Stripe for one-time lines and on CCBill for one subscription;
// solana_pay on Solana. FailedAttempts counts the order's failed payments
// through the PSP; 0 before the order exists.
type OrderPaymentOption struct {
	PSPID          PSPID    `json:"psp_id"`
	Rail           string   `json:"rail"`
	Accepts        []string `json:"accepts"`
	FailedAttempts int      `json:"failed_attempts"`
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
// entitlement_required, unavailable, currency_mismatch, quantity_invalid,
// quantity_not_allowed, amount_not_allowed, amount_required or amount_invalid.
// An already_owned line names its owner (a subscription, product access or an
// unpaid order) and a hint: change (change that subscription instead) or
// resume (pay that order, or resume that subscription). An
// entitlement_required line lists the product's required entitlements the
// customer lacks in Missing.
type OrderLineRefusal struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	OwnedBy *string  `json:"owned_by"`
	Hint    *string  `json:"hint"`
	Missing []string `json:"missing,omitempty"`
}

// OrderListParams selects orders, newest first; every filter is optional.
// PriceID names orders with a line on that price; SubscriptionID a
// subscription's renewals; Overdue the open orders past their due date. A
// customer's own list ignores CustomerID. IDs instead reads 1 to
// MaxBatchItems named orders in one page; unknown ones are absent.
type OrderListParams struct {
	IDs            []OrderID
	CustomerID     CustomerID
	PriceID        PriceID
	SubscriptionID SubscriptionID
	Status         OrderStatus
	Origin         OrderOrigin
	Overdue        bool
	PageRequest
}
