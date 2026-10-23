package billing

import (
	"encoding/json"
	"time"
)

// NotificationEvent is an immutable billing fact. Applications own its delivery,
// presentation and read state. Unknown kinds remain consumable.
type NotificationEvent struct {
	ID         NotificationID  `json:"id"`
	MerchantID MerchantID      `json:"merchant_id"`
	CustomerID CustomerID      `json:"customer_id"`
	EventType  string          `json:"event_type"`
	Data       json.RawMessage `json:"data"`
	CreatedAt  time.Time       `json:"created_at"`
	// ActionRequested describes the event when emitted, not current debt state.
	ActionRequested bool `json:"action_requested,omitempty"`
	// Optional marks ordinary successful-charge facts consumers skip by default.
	Optional bool `json:"optional,omitempty"`
}

// NotificationRetryStatus records the collection decision when a failure was
// observed. Unknown never promises an automatic retry.
type NotificationRetryStatus string

const (
	NotificationRetryScheduled             NotificationRetryStatus = "scheduled"
	NotificationRetryPaymentMethodRequired NotificationRetryStatus = "payment_method_required"
	NotificationRetryStopped               NotificationRetryStatus = "stopped"
	NotificationRetryUnknown               NotificationRetryStatus = "unknown"
)

// NotificationRecoveryAction identifies the customer action supported by the
// billing outcome. The consuming application supplies its destination.
type NotificationRecoveryAction string

const (
	NotificationRecoveryUpdatePaymentMethod NotificationRecoveryAction = "update_payment_method"
	NotificationRecoveryResubscribe         NotificationRecoveryAction = "resubscribe"
	NotificationRecoveryAuthenticatePayment NotificationRecoveryAction = "authenticate_payment"
	NotificationRecoveryContactSupport      NotificationRecoveryAction = "contact_support"
)

// CustomerNotificationActionRequested classifies what the fact originally asked.
func CustomerNotificationActionRequested(kind string) bool {
	switch kind {
	case "payment_method_failed", "payment_method_update_required", "payment_authentication_required",
		"order_due", "order_overdue", "order_collection_stopped", "account_delinquent",
		"balance_low", "balance_depleted":
		return true
	default:
		return false
	}
}

// CustomerNotificationOptional identifies routine charge confirmations.
func CustomerNotificationOptional(kind string, data NotificationData) bool {
	switch kind {
	case "premium_renewed":
		return !data.DowngradeApplied
	case "one_off_purchase_completed", "usage_charged":
		return true
	default:
		return false
	}
}

// NotificationData is the typed payload every notification event writes:
// each event fills the fields it has and the rest are omitted. Money is an
// exact decimal string of Currency's native unit, ids are typed and
// timestamps are RFC3339 instants — the same rules as every other wire shape,
// so the JSONB round-trip never touches a float.
type NotificationData struct {
	// Reason qualifies premium_ended (user_cancel, admin, expired, refund,
	// chargeback, non_recoverable, access_ended) and
	// order_collection_stopped (schedule_exhausted, non_recoverable).
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// Source names the emitter when it is not the lifecycle itself
	// (admin_manual, converge_notify).
	Source    string     `json:"source,omitempty"`
	ProductID ProductID  `json:"product_id,omitzero"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// AccessEnded distinguishes observed access loss from stopping recurrence.
	// A subscription_canceled fact may retain paid access until EndedAt.
	AccessEnded bool   `json:"access_ended,omitempty"`
	Currency    string `json:"currency,omitempty"`

	// Scheduled reprice / plan change (subscription_reprice_scheduled,
	// subscription_plan_change_scheduled).
	SubscriptionID   SubscriptionID   `json:"subscription_id,omitzero"`
	PriceMigrationID PriceMigrationID `json:"price_migration_id,omitzero"`
	FromPriceID      PriceID          `json:"from_price_id,omitzero"`
	FromProductID    ProductID        `json:"from_product_id,omitzero"`
	FromProductName  string           `json:"from_product_name,omitempty"`
	ToPriceID        PriceID          `json:"to_price_id,omitzero"`
	ToProductID      ProductID        `json:"to_product_id,omitzero"`
	ToProductName    string           `json:"to_product_name,omitempty"`
	OldAmount        *int64           `json:"old_amount,omitempty,string"`
	NewAmount        *int64           `json:"new_amount,omitempty,string"`
	EffectiveAt      *time.Time       `json:"effective_at,omitempty"`

	// Renewal receipt (premium_renewed): the renewed period, and whether it
	// applied a scheduled downgrade.
	PeriodStartsAt   *time.Time `json:"period_starts_at,omitempty"`
	PeriodEndsAt     *time.Time `json:"period_ends_at,omitempty"`
	DowngradeApplied bool       `json:"downgrade_applied,omitempty"`
	NewProduct       string     `json:"new_product,omitempty"`

	// Arrears delinquency (account_delinquent, account_delinquency_cleared).
	OverdueAmount    *int64     `json:"overdue_amount,omitempty,string"`
	OverdueOrders    int        `json:"overdue_orders,omitempty"`
	OverdueStartedAt *time.Time `json:"overdue_started_at,omitempty"`
	FromState        string     `json:"from_state,omitempty"`
	ToState          string     `json:"to_state,omitempty"`

	// A usage order's billing and collection (order_due, order_overdue,
	// payment_method_failed, payment_method_update_required,
	// order_collection_stopped); OrderID names it. CollectionMethod, on
	// order_due, records whether collection is automatic or invoiced.
	AmountDue        *int64                     `json:"amount_due,omitempty,string"`
	DueAt            *time.Time                 `json:"due_at,omitempty"`
	CollectionMethod InvoiceCollectionMethod    `json:"collection_method,omitempty"`
	FailureCode      string                     `json:"failure_code,omitempty"`
	FailureReason    string                     `json:"failure_reason,omitempty"`
	DeclineOutcome   string                     `json:"decline_outcome,omitempty"`
	NextAttemptAt    *time.Time                 `json:"next_attempt_at,omitempty"`
	RetryStatus      NotificationRetryStatus    `json:"retry_status,omitempty"`
	RecoveryAction   NotificationRecoveryAction `json:"recovery_action,omitempty"`

	// A watched balance went low or ran out (balance_low, balance_depleted):
	// what was available and owed in Currency as the crossing was seen, the
	// effective low line, and when capacity reaches zero at the customer's
	// burn rate (absent at rate zero).
	AvailableAmount  *int64     `json:"available_amount,omitempty,string"`
	OwedAmount       *int64     `json:"owed_amount,omitempty,string"`
	LowBalanceAmount *int64     `json:"low_balance_amount,omitempty,string"`
	RunwayEndsAt     *time.Time `json:"runway_ends_at,omitempty"`

	// Rail context on payment_method_failed.
	Rail               string `json:"rail,omitempty"`
	RailSubscriptionID string `json:"rail_subscription_id,omitempty"`
	TransactionID      string `json:"transaction_id,omitempty"`

	// Quantity is the seats a subscription_changed notice's subscription has
	// from EffectiveAt; Amount is what the change charged now.
	Quantity *int `json:"quantity,omitempty"`

	// One-off purchase receipt (one_off_purchase_completed): what was bought,
	// the amount paid, the payment and, for an order, the order and its
	// number. Delivery addresses belong to the consuming application.
	Amount        *int64    `json:"amount,omitempty,string"`
	ProductName   string    `json:"product_name,omitempty"`
	PaymentMethod string    `json:"payment_method,omitempty"`
	PaymentID     PaymentID `json:"payment_id,omitzero"`
	OrderID       OrderID   `json:"order_id,omitzero"`
	OrderNumber   int64     `json:"order_number,omitempty"`
}
