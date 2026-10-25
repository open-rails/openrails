package billing

// OrderPaymentAttempt is the retained receipt of one purchase payment request.
// Its original POST /me/orders/{id}/pay Idempotency-Key selects the attempt;
// create-order-with-payment keys and recurring/usage collection keys are separate.
// The latest order payment
// does not. Reading it neither executes nor retries a charge. A missing key
// returns not found even when a different request paid the order.
type OrderPaymentAttempt struct {
	OrderID    OrderID        `json:"order_id"`
	CustomerID CustomerID     `json:"customer_id"`
	AttemptID  OrderAttemptID `json:"attempt_id"`
	// State is the attempt's own state: created, processing, requires_action,
	// succeeded, failed, expired or canceled. Nonterminal states are not proof
	// that a provider did or did not debit money.
	State     string     `json:"state"`
	PaymentID *PaymentID `json:"payment_id,omitempty"`
	// PaymentMethodID is the succeeded operation's retained method. Before
	// success an intended UUID is not proof of an attached card. Omitted for
	// purchase-scoped instruments and attempts without a retained card.
	PaymentMethodID *PaymentMethodID `json:"payment_method_id,omitempty"`
	PSPID           PSPID            `json:"psp_id"`
	Amount          int64            `json:"amount,string"`
	Currency        string           `json:"currency"`
}
