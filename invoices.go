package openrails

import (
	"github.com/google/uuid"
	"time"
)

// InvoiceAdminAction describes an available operation, not permission to perform it.
type InvoiceAdminAction string

const (
	InvoiceAdminVoid            InvoiceAdminAction = "void"
	InvoiceAdminUncollectible   InvoiceAdminAction = "mark_uncollectible"
	InvoiceAdminRecordPayment   InvoiceAdminAction = "record_payment"
	InvoiceAdminRetryCollection InvoiceAdminAction = "retry_collection"
)

// Invoice DTOs are the shared Go and HTTP contract. Timestamps are RFC3339
// instants. Monetary amounts use the currency precision given by UnitDecimals.
type InvoiceLineItemDTO struct {
	EventType  string           `json:"event_type"`
	Amount     int64            `json:"amount,string"`
	Count      int64            `json:"count"`
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
}

type InvoiceDTO struct {
	Recovery                  *PaymentRecovery     `json:"recovery,omitempty"`
	ID                        uuid.UUID            `json:"id"`
	Currency                  string               `json:"currency"`
	InvoiceNumber             *string              `json:"invoice_number,omitempty"`
	PeriodFrom                time.Time            `json:"period_from"`
	PeriodTo                  time.Time            `json:"period_to"`
	UsageTotal                int64                `json:"usage_total,string"`
	DepositsTotal             int64                `json:"deposits_total,string"`
	OwedAccrued               int64                `json:"owed_accrued,string"`
	OwedPaid                  int64                `json:"owed_paid,string"`
	ClosingBalance            int64                `json:"closing_balance,string"`
	SubtotalAmount            int64                `json:"subtotal_amount,string"`
	TotalAmount               int64                `json:"total_amount,string"`
	AmountPaid                int64                `json:"amount_paid,string"`
	AmountDue                 int64                `json:"amount_due,string"`
	LineItems                 []InvoiceLineItemDTO `json:"line_items"`
	MoneyMovements            AmountMap            `json:"money_movements,omitempty"`
	PONumber                  *string              `json:"po_number,omitempty"`
	Tax                       map[string]any       `json:"tax,omitempty"`
	BillingContacts           []InvoiceContactDTO  `json:"billing_contacts,omitempty"`
	Memo                      *string              `json:"memo,omitempty"`
	Status                    string               `json:"status"`
	CollectionMethod          string               `json:"collection_method"`
	IssuedAt                  *time.Time           `json:"issued_at,omitempty"`
	DueAt                     *time.Time           `json:"due_at,omitempty"`
	PaidAt                    *time.Time           `json:"paid_at,omitempty"`
	VoidedAt                  *time.Time           `json:"voided_at,omitempty"`
	UncollectibleAt           *time.Time           `json:"uncollectible_at,omitempty"`
	FinalizedAt               *time.Time           `json:"finalized_at,omitempty"`
	ExternalInvoiceID         *string              `json:"external_invoice_id,omitempty"`
	CollectionFailureCount    int32                `json:"collection_failure_count"`
	CollectionFailedAt        *time.Time           `json:"collection_failed_at,omitempty"`
	NextCollectionAttemptAt   *time.Time           `json:"next_collection_attempt_at,omitempty"`
	LastCollectionFailureCode *string              `json:"last_collection_failure_code,omitempty"`
	// CollectionIntentID names the live collection operation (`openrails
	// intents`); no competing collection or admin mutation runs while set.
	CollectionIntentID *uuid.UUID `json:"collection_intent_id,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
}

type InvoicePaymentAttemptDTO struct {
	ID              uuid.UUID        `json:"id"`
	InvoiceID       uuid.UUID        `json:"invoice_id"`
	Currency        string           `json:"currency"`
	Amount          int64            `json:"amount,string"`
	Status          string           `json:"status"`
	PaymentMethodID *PaymentMethodID `json:"payment_method_id,omitempty"`
	Rail            *string          `json:"rail,omitempty"`
	RailPaymentID   *string          `json:"rail_payment_id,omitempty"`
	FailureCode     *string          `json:"failure_code,omitempty"`
	FailureReason   *string          `json:"failure_reason,omitempty"`
	AttemptedAt     time.Time        `json:"attempted_at"`
	SettledAt       *time.Time       `json:"settled_at,omitempty"`
}

type InvoiceCollectionRetryRequest struct {
	InvoiceID       uuid.UUID       `json:"invoice_id"`
	IdempotencyKey  string          `json:"-"`
	PaymentMethodID PaymentMethodID `json:"payment_method_id"`
}

type InvoiceCollectionRetryResult struct {
	Invoice  InvoiceDTO               `json:"invoice"`
	Attempt  InvoicePaymentAttemptDTO `json:"attempt"`
	Replayed bool                     `json:"replayed"`
}

type InvoiceContactDTO struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// Invoice operation refusals carry these StatusError.Code values.
const (
	CodeInvoiceActionNotAllowed         = "invoice_action_not_allowed"
	CodeInvoiceNotRetryable             = "invoice_not_retryable"
	CodeInvoiceRetryInProgress          = "invoice_retry_in_progress"
	CodeInvoiceRetryOutcomeUnknown      = "invoice_retry_outcome_unknown"
	CodeInvoiceRetryIdempotencyConflict = "invoice_retry_idempotency_conflict"
	CodeInvoicePaymentReferenceUsed     = "invoice_payment_reference_used"
	CodeInvoicePaymentExceedsDue        = "invoice_payment_exceeds_due"
	CodeInvoicePaymentInvalid           = "invoice_payment_invalid"
	CodeCollectionPaymentMethodRequired = "collection_payment_method_required"
	CodeCollectionPaymentMethodInvalid  = "collection_payment_method_invalid"
)

type InvoiceProfileDTO struct {
	NetTermsDays     int                 `json:"net_terms_days"`
	CollectionMethod string              `json:"collection_method"`
	PONumber         string              `json:"po_number,omitempty"`
	Tax              map[string]any      `json:"tax,omitempty"`
	BillingContacts  []InvoiceContactDTO `json:"billing_contacts,omitempty"`
	Memo             string              `json:"memo,omitempty"`
}

type MerchantInvoiceDTO struct {
	InvoiceDTO
	UnitDecimals     int                  `json:"unit_decimals"`
	CustomerID       CustomerID           `json:"customer_id"`
	AvailableActions []InvoiceAdminAction `json:"available_actions"`
}

type MerchantInvoiceFilter struct {
	CustomerID CustomerID
	Currency   *string
	Status     *string
	PeriodFrom *time.Time
	PeriodTo   *time.Time
}

// RecordInvoicePaymentRequest records money received outside automatic collection.
// Reference is the immutable idempotency identity of that remittance.
type RecordInvoicePaymentRequest struct {
	Amount    int64  `json:"amount,string"`
	Reference string `json:"reference"`
}
