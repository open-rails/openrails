package billing

import (
	"time"

	"github.com/google/uuid"
)

// InvokerType says whose credential an invoker presents.
type InvokerType string

const (
	// InvokerTypeCustomer is the paying customer's own credential: failed usage
	// draws the customer's grace, then is charged.
	InvokerTypeCustomer InvokerType = "customer"
	// InvokerTypeDelegated is another party acting for the customer: admission
	// refuses it, and its failed usage meets flat cutoffs.
	InvokerTypeDelegated InvokerType = "delegated"
)

// AdmitParams asks to admit one request against a customer's money: payer
// capacity, delegated spend windows and failed-usage cutoffs. An allowed admit
// with a nonzero EstimatedAmount holds that much until it is captured,
// released or reaches ExpiresAt.
type AdmitParams struct {
	// RequestID identifies the admission: a retry with the same terms answers
	// the same verdict, one with changed terms is ErrIdempotencyKeyReused.
	RequestID   string      `json:"request_id"`
	CustomerID  CustomerID  `json:"customer_id"`
	Invoker     string      `json:"invoker"`
	InvokerType InvokerType `json:"invoker_type"`
	// TrustLevel selects money policy; empty uses the customer's stored level.
	TrustLevel string `json:"trust_level,omitempty"`
	// Resource is attribution only.
	Resource string `json:"resource,omitempty"`
	Currency string `json:"currency"`
	// EstimatedAmount is a host-stated hold. Usage instead states the usage
	// the request expects, which the meters' rate cards price into the hold;
	// the two are exclusive, and with neither the admission holds nothing.
	EstimatedAmount int64          `json:"estimated_amount,string"`
	Usage           *UsageEstimate `json:"usage,omitempty"`
	// AccrualRateDeltaPerHour is the ongoing rate this request adds, in native
	// units per hour; an accrual_rate_cap policy gates on it.
	AccrualRateDeltaPerHour int64  `json:"accrual_rate_delta_per_hour,omitempty,string"`
	Source                  string `json:"source,omitempty"`
	// ExpiresAt is the admitted work's deadline, required when EstimatedAmount
	// places a hold. A hold lives at most 30 days past its admission.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Roles are the invoker's role UUIDs; role-scoped spend delegations gate
	// on them.
	Roles []uuid.UUID `json:"roles,omitempty"`
}

// UsageEstimate is the usage an admission expects, e.g. {requests: 1} for a
// call or {input_tokens: <prompt>, output_tokens: <max_tokens>} for an LLM
// request. Its event type's rate cards price it at the customer's current
// period quantities, without advancing them, and are pinned with the
// admission: its capture is priced by them.
type UsageEstimate struct {
	// Amount is what a dynamic card charges for this estimate.
	Amount     *int64           `json:"amount,omitempty,string"`
	EventType  string           `json:"event_type"`
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
	Metadata   map[string]any   `json:"metadata,omitempty"`
}

// MaxAdmissionBatchItems bounds one Admit, CaptureAdmissions,
// ReleaseAdmissions or ExtendAdmissions call.
const MaxAdmissionBatchItems = 1000

// AdmitBatchParams is the body of POST /v1/admin/admissions.
type AdmitBatchParams struct {
	Items []AdmitParams `json:"items"`
}

// AdmissionState is where an admitted request's hold stands.
type AdmissionState string

const (
	AdmissionOpen     AdmissionState = "open"
	AdmissionCaptured AdmissionState = "captured"
	AdmissionReleased AdmissionState = "released"
	// AdmissionExpired is an open hold past its deadline.
	AdmissionExpired AdmissionState = "expired"
)

// AdmissionBlock names the gate that refused an admission.
type AdmissionBlock string

const (
	AdmissionBlockedByMoney  AdmissionBlock = "money"
	AdmissionBlockedByBudget AdmissionBlock = "budget"
	AdmissionBlockedByAbuse  AdmissionBlock = "abuse"
)

// Admission is the verdict on one request and, when it was allowed, its hold.
type Admission struct {
	RequestID  string     `json:"request_id"`
	CustomerID CustomerID `json:"customer_id"`
	Allowed    bool       `json:"allowed"`
	// BlockedBy and DenyCode name a refusal; RetryAfterSeconds is when a
	// window refusal clears. All three are null when allowed.
	BlockedBy         *AdmissionBlock `json:"blocked_by"`
	DenyCode          *string         `json:"deny_code"`
	RetryAfterSeconds *int64          `json:"retry_after_seconds"`
	Currency          string          `json:"currency"`
	EstimatedAmount   int64           `json:"estimated_amount,string"`
	// StartCapacityAmount is what the customer could spend when the request
	// was admitted, before its own hold.
	StartCapacityAmount int64 `json:"start_capacity_amount,string"`
	// State, ExpiresAt and CapturedAmount describe the hold; null when refused.
	State          *AdmissionState `json:"state"`
	ExpiresAt      *time.Time      `json:"expires_at"`
	CapturedAmount *int64          `json:"captured_amount,string"`
	// Replayed: this request id was already admitted; nothing new was held.
	Replayed bool `json:"replayed"`
}

// Active reports an allowed admission whose hold is still open.
func (a *Admission) Active() bool {
	return a != nil && a.Allowed && a.State != nil && *a.State == AdmissionOpen
}

// AdmissionVerdict is one item of an admission batch. Status is the HTTP
// status admitting the item alone would have answered: 200 with Admission, or
// a refusal with Error.
type AdmissionVerdict struct {
	Status    int           `json:"status"`
	Admission *Admission    `json:"admission"`
	Error     *ErrorDetails `json:"error"`
}

// Allowed reports whether this item holds a live admission.
func (v AdmissionVerdict) Allowed() bool {
	return v.Status == 200 && v.Admission.Active()
}

// AdmitBatchResult answers POST /v1/admin/admissions: one verdict per
// item, in order.
type AdmitBatchResult struct {
	Items []AdmissionVerdict `json:"items"`
}

// CaptureUsage is the usage a capture records: its quantities (Dimensions,
// Metadata) and its idempotency coordinate, Source ("admit" when empty) and
// SourceID (the request id when empty).
type CaptureUsage struct {
	EventType  string           `json:"event_type"`
	Resource   string           `json:"resource,omitempty"`
	Metadata   map[string]any   `json:"metadata,omitempty"`
	Source     string           `json:"source,omitempty"`
	SourceID   string           `json:"source_id,omitempty"`
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
}

// CaptureAdmissionParams settles one admitted request. Usage is required and
// priced by the admission's pinned cards (or the cards in force when none were
// pinned). Its event type must be the admitted one. Dynamic cards charge Amount;
// other cards price the quantities. Above the hold the rest is owed, never
// clamped. An exact retry answers the first receipt; changed terms are refused.
type CaptureAdmissionParams struct {
	RequestID string       `json:"request_id"`
	Usage     CaptureUsage `json:"usage" binding:"required"`
	Amount    *int64       `json:"amount,omitempty,string"`
}

// CaptureAdmissionBatchParams settles 1 to MaxAdmissionBatchItems admitted
// requests.
type CaptureAdmissionBatchParams struct {
	Items []CaptureAdmissionParams `json:"items"`
}

// CaptureReceipt is the result of settling one admitted request: Amount is
// what it charged, the sum of Charges when its usage was priced by rate
// cards. UsageEventID is the usage event it recorded; BalanceTransactionID
// the ledger posting it made (its first charge's), null when it charged
// nothing.
type CaptureReceipt struct {
	RequestID            string                `json:"request_id"`
	CustomerID           CustomerID            `json:"customer_id"`
	Currency             string                `json:"currency"`
	EstimatedAmount      int64                 `json:"estimated_amount,string"`
	Amount               int64                 `json:"amount,string"`
	Charges              []UsageCharge         `json:"charges"`
	UsageEventID         *UsageEventID         `json:"usage_event_id"`
	BalanceTransactionID *BalanceTransactionID `json:"balance_transaction_id"`
	Replayed             bool                  `json:"replayed"`
}

// CaptureResult is one item of a capture batch: Status is what capturing
// that item alone answers, with Receipt or Error.
type CaptureResult struct {
	Status  int             `json:"status"`
	Receipt *CaptureReceipt `json:"receipt"`
	Error   *ErrorDetails   `json:"error"`
}

// Err is the item's refusal as the error capturing it alone would return,
// nil when it was captured.
func (r CaptureResult) Err() error {
	if r.Error == nil {
		return nil
	}
	return &StatusError{Status: r.Status, ErrorDetails: *r.Error}
}

// CaptureBatchResult is one result per item, in request order.
type CaptureBatchResult struct {
	Items []CaptureResult `json:"items"`
}

// ReleaseAdmissionBatchParams releases the holds of 1 to
// MaxAdmissionBatchItems admitted requests.
type ReleaseAdmissionBatchParams struct {
	RequestIDs []string `json:"request_ids"`
}

// ExtendAdmissionParams moves an open hold's deadline later, to at most 30
// days past its admission.
type ExtendAdmissionParams struct {
	RequestID string    `json:"request_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ExtendAdmissionBatchParams extends 1 to MaxAdmissionBatchItems holds.
type ExtendAdmissionBatchParams struct {
	Items []ExtendAdmissionParams `json:"items"`
}

// AdmissionResult is one item of a release or extend batch: Status is what
// the operation on that item alone answers, with Admission or Error.
type AdmissionResult struct {
	Status    int           `json:"status"`
	Admission *Admission    `json:"admission"`
	Error     *ErrorDetails `json:"error"`
}

// Err is the item's refusal as the error the operation alone would return,
// nil when it succeeded.
func (r AdmissionResult) Err() error {
	if r.Error == nil {
		return nil
	}
	return &StatusError{Status: r.Status, ErrorDetails: *r.Error}
}

// AdmissionBatchResult is one result per item, in request order.
type AdmissionBatchResult struct {
	Items []AdmissionResult `json:"items"`
}
