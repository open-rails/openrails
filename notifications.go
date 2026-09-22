package openrails

import (
	"time"

	"github.com/google/uuid"
)

// Notification is one in-app billing notification (GET /v1/me/notifications
// for the customer's own; GET /v1/merchant/repair-alerts for the merchant's
// system_alert rows).
type Notification struct {
	ID         uuid.UUID        `json:"id"`
	CustomerID string           `json:"customer_id"`
	EventType  string           `json:"event_type"`
	Data       NotificationData `json:"data"`
	Seen       bool             `json:"seen"`
	CreatedAt  time.Time        `json:"created_at"`
}

// NotificationData is the typed payload every notification event writes:
// each event fills the fields it has and the rest are omitted. Money is an
// exact decimal string of Currency's native unit, ids are typed and
// timestamps are RFC3339 instants — the same rules as every other wire shape,
// so the JSONB round-trip never touches a float.
type NotificationData struct {
	// Reason qualifies premium_ended (user_cancel, admin, expired, refund,
	// chargeback, non_recoverable, access_ended) and
	// invoice_collection_stopped (schedule_exhausted, non_recoverable).
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// Source names the emitter when it is not the lifecycle itself
	// (admin_manual, fetch_converge, converge_notify).
	Source      string     `json:"source,omitempty"`
	Entitlement string     `json:"entitlement,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	Currency    string     `json:"currency,omitempty"`

	// Scheduled reprice / plan change (subscription_reprice_scheduled,
	// subscription_plan_change_scheduled).
	SubscriptionID SubscriptionID `json:"subscription_id,omitzero"`
	FromPriceID    string         `json:"from_price_id,omitzero"`
	ToPriceID      string         `json:"to_price_id,omitzero"`
	ToProductID    string         `json:"to_product_id,omitzero"`
	ToProductName  string         `json:"to_product_name,omitempty"`
	OldAmount      *int64         `json:"old_amount,omitempty,string"`
	NewAmount      *int64         `json:"new_amount,omitempty,string"`
	EffectiveAt    *time.Time     `json:"effective_at,omitempty"`

	// Renewal that applied a scheduled downgrade (premium_renewed).
	DowngradeApplied bool   `json:"downgrade_applied,omitempty"`
	NewProduct       string `json:"new_product,omitempty"`

	// Arrears delinquency (account_delinquent, account_delinquency_cleared).
	OverdueAmount   *int64     `json:"overdue_amount,omitempty,string"`
	OverdueInvoices int        `json:"overdue_invoices,omitempty"`
	OverdueSince    *time.Time `json:"overdue_since,omitempty"`
	FromState       string     `json:"from_state,omitempty"`
	ToState         string     `json:"to_state,omitempty"`

	// Invoice lifecycle and collection (invoice_issued,
	// payment_method_failed, payment_method_update_required,
	// invoice_collection_stopped).
	InvoiceID      uuid.UUID  `json:"invoice_id,omitzero"`
	InvoiceNumber  string     `json:"invoice_number,omitempty"`
	AmountDue      *int64     `json:"amount_due,omitempty,string"`
	DueAt          *time.Time `json:"due_at,omitempty"`
	FailureCode    string     `json:"failure_code,omitempty"`
	FailureReason  string     `json:"failure_reason,omitempty"`
	DeclineOutcome string     `json:"decline_outcome,omitempty"`
	NextAttemptAt  *time.Time `json:"next_attempt_at,omitempty"`

	// Rail context on payment_method_failed.
	Rail               string `json:"rail,omitempty"`
	RailSubscriptionID string `json:"rail_subscription_id,omitempty"`
	TransactionID      string `json:"transaction_id,omitempty"`

	// One-off purchase receipt (one_off_purchase_completed). UserEmail is the
	// address the purchase was made with, for buyers without a profile email.
	Amount        *int64 `json:"amount,omitempty,string"`
	ProductName   string `json:"product_name,omitempty"`
	PaymentMethod string `json:"payment_method,omitempty"`
	UserEmail     string `json:"user_email,omitempty"`

	// Merchant repair alerts (system_alert): what needs an operator and why.
	Kind               string         `json:"kind,omitempty"`
	Provider           string         `json:"provider,omitempty"`
	Operation          string         `json:"operation,omitempty"`
	AffectedCustomerID string         `json:"affected_customer_id,omitzero"`
	OriginalPaymentID  PaymentID      `json:"original_payment_id,omitzero"`
	Error              string         `json:"error,omitempty"`
	Metadata           map[string]any `json:"metadata,omitempty"`
}
