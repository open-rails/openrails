package billing

import (
	"time"

	"github.com/google/uuid"
)

// InvoiceID names one invoice; it travels as inv_<uuid>.
type InvoiceID uuid.UUID

// InvoicePaymentID names one payment applied to an invoice; it travels as
// invpay_<uuid>.
type InvoicePaymentID uuid.UUID

const (
	InvoiceIDPrefix        = "inv_"
	InvoicePaymentIDPrefix = "invpay_"
)

func ParseInvoiceID(s string) (InvoiceID, error) {
	u, err := parsePrefixedID("invoice", InvoiceIDPrefix, s)
	return InvoiceID(u), err
}

func ParseInvoicePaymentID(s string) (InvoicePaymentID, error) {
	u, err := parsePrefixedID("invoice payment", InvoicePaymentIDPrefix, s)
	return InvoicePaymentID(u), err
}

func (id InvoiceID) UUID() uuid.UUID              { return uuid.UUID(id) }
func (id InvoiceID) IsZero() bool                 { return uuid.UUID(id) == uuid.Nil }
func (id InvoiceID) String() string               { return formatPrefixedID(InvoiceIDPrefix, uuid.UUID(id)) }
func (id InvoiceID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *InvoiceID) UnmarshalText(text []byte) error {
	parsed, err := ParseInvoiceID(string(text))
	*id = parsed
	return err
}

func (id InvoicePaymentID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id InvoicePaymentID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id InvoicePaymentID) String() string {
	return formatPrefixedID(InvoicePaymentIDPrefix, uuid.UUID(id))
}
func (id InvoicePaymentID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *InvoicePaymentID) UnmarshalText(text []byte) error {
	parsed, err := ParseInvoicePaymentID(string(text))
	*id = parsed
	return err
}

// InvoiceStatus is where an invoice stands.
type InvoiceStatus string

const (
	InvoiceDraft         InvoiceStatus = "draft"
	InvoiceOpen          InvoiceStatus = "open"
	InvoicePastDue       InvoiceStatus = "past_due"
	InvoicePaid          InvoiceStatus = "paid"
	InvoiceVoided        InvoiceStatus = "voided"
	InvoiceUncollectible InvoiceStatus = "uncollectible"
)

// InvoiceStatuses lists every invoice status.
func InvoiceStatuses() []InvoiceStatus {
	return []InvoiceStatus{InvoiceDraft, InvoiceOpen, InvoicePastDue, InvoicePaid, InvoiceVoided, InvoiceUncollectible}
}

// InvoiceCollectionMethod is how an invoice is paid: charged to the
// customer's collection card, or sent for the customer to pay.
type InvoiceCollectionMethod string

const (
	CollectChargeAutomatically InvoiceCollectionMethod = "charge_automatically"
	CollectSendInvoice         InvoiceCollectionMethod = "send_invoice"
)

// InvoiceAction is an operation the merchant may take on an invoice now: a
// description of the invoice's state, filtered by the caller's permissions.
type InvoiceAction string

const (
	InvoiceActionVoid            InvoiceAction = "void"
	InvoiceActionUncollectible   InvoiceAction = "mark_uncollectible"
	InvoiceActionRecordPayment   InvoiceAction = "record_payment"
	InvoiceActionRetryCollection InvoiceAction = "retry_collection"
)

// Invoice is one period's statement and, in arrears billing, the receivable
// payments are applied to. Amounts are native units of Currency.
type Invoice struct {
	ID             InvoiceID         `json:"id"`
	CustomerID     CustomerID        `json:"customer_id"`
	Currency       string            `json:"currency"`
	InvoiceNumber  *string           `json:"invoice_number"`
	PeriodFrom     time.Time         `json:"period_from"`
	PeriodTo       time.Time         `json:"period_to"`
	UsageTotal     int64             `json:"usage_total,string"`
	DepositsTotal  int64             `json:"deposits_total,string"`
	OwedAccrued    int64             `json:"owed_accrued,string"`
	OwedPaid       int64             `json:"owed_paid,string"`
	ClosingBalance int64             `json:"closing_balance,string"`
	SubtotalAmount int64             `json:"subtotal_amount,string"`
	TotalAmount    int64             `json:"total_amount,string"`
	AmountPaid     int64             `json:"amount_paid,string"`
	AmountDue      int64             `json:"amount_due,string"`
	LineItems      []InvoiceLineItem `json:"line_items"`
	MoneyMovements AmountMap         `json:"money_movements"`
	PONumber       *string           `json:"po_number"`
	// Tax is the host-defined tax document fields from the invoice profile.
	Tax              map[string]any          `json:"tax"`
	BillingContacts  []InvoiceContact        `json:"billing_contacts"`
	Memo             *string                 `json:"memo"`
	Status           InvoiceStatus           `json:"status"`
	CollectionMethod InvoiceCollectionMethod `json:"collection_method"`
	IssuedAt         *time.Time              `json:"issued_at"`
	DueAt            *time.Time              `json:"due_at"`
	PaidAt           *time.Time              `json:"paid_at"`
	VoidedAt         *time.Time              `json:"voided_at"`
	UncollectibleAt  *time.Time              `json:"uncollectible_at"`
	FinalizedAt      *time.Time              `json:"finalized_at"`
	// ExternalInvoiceID is the invoice's id at a provider that mirrors it.
	ExternalInvoiceID         *string    `json:"external_invoice_id"`
	CollectionFailureCount    int32      `json:"collection_failure_count"`
	CollectionFailedAt        *time.Time `json:"collection_failed_at"`
	NextCollectionAttemptAt   *time.Time `json:"next_collection_attempt_at"`
	LastCollectionFailureCode *string    `json:"last_collection_failure_code"`
	// Recovery is whether the customer can pay the invoice now; read with a
	// single invoice, null in lists.
	Recovery *PaymentRecovery `json:"recovery"`
	// AvailableActions are the merchant operations the caller may take now;
	// empty for the customer.
	AvailableActions []InvoiceAction `json:"available_actions"`
	CreatedAt        time.Time       `json:"created_at"`
}

// InvoiceLineItem is one statement line: a usage rollup by event type.
type InvoiceLineItem struct {
	EventType  string           `json:"event_type"`
	Amount     int64            `json:"amount,string"`
	Count      int64            `json:"count"`
	Dimensions map[string]int64 `json:"dimensions"`
}

// InvoiceContact is one billing contact on an invoice document.
type InvoiceContact struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// InvoicePaymentStatus is where a payment applied to an invoice stands.
type InvoicePaymentStatus string

const (
	InvoicePaymentAttempted InvoicePaymentStatus = "attempted"
	InvoicePaymentSettled   InvoicePaymentStatus = "settled"
	InvoicePaymentFailed    InvoicePaymentStatus = "failed"
)

// InvoicePayment is one payment applied to an invoice: a collection charge
// (Rail and PaymentMethodID set) or money the merchant recorded.
type InvoicePayment struct {
	ID              InvoicePaymentID     `json:"id"`
	InvoiceID       InvoiceID            `json:"invoice_id"`
	Currency        string               `json:"currency"`
	Amount          int64                `json:"amount,string"`
	Status          InvoicePaymentStatus `json:"status"`
	PaymentMethodID *PaymentMethodID     `json:"payment_method_id"`
	Rail            *string              `json:"rail"`
	// TransactionID is the provider's id of the charge.
	TransactionID *string    `json:"transaction_id"`
	FailureCode   *string    `json:"failure_code"`
	FailureReason *string    `json:"failure_reason"`
	AttemptedAt   time.Time  `json:"attempted_at"`
	SettledAt     *time.Time `json:"settled_at"`
}

// ListInvoicesParams selects invoices, newest period first; every filter is
// optional. PeriodFrom and PeriodTo bound the invoice's period start.
type ListInvoicesParams struct {
	CustomerID CustomerID
	Currency   string
	Status     InvoiceStatus
	PeriodFrom *time.Time
	PeriodTo   *time.Time
	Page       PageRequest
}

// RetryInvoiceCollectionParams charges an open invoice to one of the
// customer's cards. A retry with the same IdempotencyKey answers the first
// attempt.
type RetryInvoiceCollectionParams struct {
	PaymentMethodID PaymentMethodID `json:"payment_method_id"`
	IdempotencyKey  string          `json:"-"`
}

// InvoiceCollection is the outcome of a collection charge: the invoice, the
// charge applied to it, and whether this answers an earlier request.
type InvoiceCollection struct {
	Invoice  Invoice        `json:"invoice"`
	Payment  InvoicePayment `json:"payment"`
	Replayed bool           `json:"replayed"`
}

// PayInvoiceParams is the customer paying an invoice now with one of its
// cards.
type PayInvoiceParams struct {
	PaymentMethodID PaymentMethodID `json:"payment_method_id"`
	IdempotencyKey  string          `json:"-"`
}

// InvoicePayNow is the outcome of a customer paying an invoice: the charge
// and its operation, unresolved while the provider (or 3-D Secure) decides.
type InvoicePayNow struct {
	Invoice   Invoice          `json:"invoice"`
	Payment   InvoicePayment   `json:"payment"`
	Operation PaymentOperation `json:"operation"`
	Replayed  bool             `json:"replayed"`
}

// CreateInvoicePaymentParams records money received outside collection.
// Reference is the remittance's identity: recording it again with the same
// amount answers the first record, with another amount is refused.
type CreateInvoicePaymentParams struct {
	Amount    int64  `json:"amount,string"`
	Reference string `json:"reference"`
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

// InvoiceProfile is a customer's invoice terms and document fields, copied
// onto each invoice at finalization.
type InvoiceProfile struct {
	NetTermsDays     int                     `json:"net_terms_days"`
	CollectionMethod InvoiceCollectionMethod `json:"collection_method"`
	PONumber         string                  `json:"po_number"`
	Tax              map[string]any          `json:"tax"`
	BillingContacts  []InvoiceContact        `json:"billing_contacts"`
	Memo             string                  `json:"memo"`
}

// SetInvoiceProfileParams replaces a customer's invoice profile. IfAbsent
// only creates it: an existing profile is answered unchanged.
type SetInvoiceProfileParams struct {
	InvoiceProfile
	IfAbsent bool `json:"-"`
}
