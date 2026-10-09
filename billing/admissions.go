package billing

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// InvokerType says whose credential an invoker presents.
type InvokerType string

const (
	// InvokerTypeCustomer is the paying customer's own credential: wasted spend
	// draws the customer's grace, then is charged.
	InvokerTypeCustomer InvokerType = "customer"
	// InvokerTypeDelegated spends the customer's balance under a spend
	// delegation: flat wasted-spend cutoffs apply.
	InvokerTypeDelegated InvokerType = "delegated"
)

// AdmitParams asks to admit one request against a customer's money: payer
// capacity, delegated spend windows and wasted-spend cutoffs. An allowed admit
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
	Resource        string `json:"resource,omitempty"`
	Currency        string `json:"currency"`
	EstimatedAmount int64  `json:"estimated_amount,string"`
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

// MaxAdmissionBatchItems bounds one Admit, ReleaseAdmissions or
// ExtendAdmissions call.
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

// CaptureUsage records a usage event with a capture when EventType is set.
// The first capture fixes these terms.
type CaptureUsage struct {
	EventType  string           `json:"event_type"`
	Resource   string           `json:"resource,omitempty"`
	Metadata   map[string]any   `json:"metadata,omitempty"`
	Source     string           `json:"source,omitempty"`
	SourceID   string           `json:"source_id,omitempty"`
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
}

// CaptureAdmissionParams settles an admitted request at Amount; zero completes it at
// no cost. Amount is required. A retry that changes the terms is
// ErrIdempotencyKeyReused.
type CaptureAdmissionParams struct {
	Amount int64         `json:"amount,string"`
	Usage  *CaptureUsage `json:"usage,omitempty"`
}

// UnmarshalJSON refuses a capture that does not name its amount, so a
// missing field is never read as a free completion.
func (p *CaptureAdmissionParams) UnmarshalJSON(raw []byte) error {
	var in struct {
		Amount *int64        `json:"amount,string"`
		Usage  *CaptureUsage `json:"usage"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		return err
	}
	if in.Amount == nil {
		return errors.New("amount is required")
	}
	p.Amount, p.Usage = *in.Amount, in.Usage
	return nil
}

// CaptureReceipt is the result of settling one admitted request.
// BalanceTransactionID is null for a zero-cost capture.
type CaptureReceipt struct {
	RequestID            string                `json:"request_id"`
	CustomerID           CustomerID            `json:"customer_id"`
	Currency             string                `json:"currency"`
	Amount               int64                 `json:"amount,string"`
	BalanceTransactionID *BalanceTransactionID `json:"balance_transaction_id"`
	Replayed             bool                  `json:"replayed"`
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

// ReportWastedSpendParams reports spend a customer's invoker wasted (failed or
// abusive work). Source and SourceID identify the report: a replay records
// nothing new, one with a changed Amount is ErrIdempotencyKeyReused.
type ReportWastedSpendParams struct {
	CustomerID  CustomerID  `json:"customer_id"`
	Invoker     string      `json:"invoker"`
	InvokerType InvokerType `json:"invoker_type"`
	Currency    string      `json:"currency"`
	Amount      int64       `json:"amount,string"`
	Source      string      `json:"source"`
	SourceID    string      `json:"source_id"`
	Reason      string      `json:"reason,omitempty"`
}

// ReportWastedSpendBatchParams reports 1 to MaxBatchItems wasted spends.
type ReportWastedSpendBatchParams struct {
	Items []ReportWastedSpendParams `json:"items"`
}

// WastedSpendResult is one report's outcome: Status is what reporting it
// alone answers, with Report or Error.
type WastedSpendResult struct {
	Status int                `json:"status"`
	Report *WastedSpendReport `json:"report"`
	Error  *ErrorDetails      `json:"error"`
}

// Err is the item's refusal as the error reporting it alone would return, nil
// when it was handled.
func (r WastedSpendResult) Err() error {
	if r.Error == nil {
		return nil
	}
	return &StatusError{Status: r.Status, ErrorDetails: *r.Error}
}

// ReportWastedSpendBatchResult is one result per item, in request order.
type ReportWastedSpendBatchResult struct {
	Items []WastedSpendResult `json:"items"`
}

// WastedSpendAction is what OpenRails did with a wasted-spend report.
type WastedSpendAction string

const (
	// WastedSpendIgnored: a zero amount; nothing to record.
	WastedSpendIgnored WastedSpendAction = "ignored"
	// WastedSpendForgiven: absorbed by the customer's grace window.
	WastedSpendForgiven WastedSpendAction = "forgiven"
	// WastedSpendCharged: the customer's grace was exhausted and the
	// overage charged.
	WastedSpendCharged WastedSpendAction = "charged"
	// WastedSpendInvokerCutoffTracked: counted toward a delegated invoker's
	// cutoff; nothing charged.
	WastedSpendInvokerCutoffTracked WastedSpendAction = "invoker_cutoff_tracked"
	// WastedSpendDuplicate: a replay of a report already handled. The money
	// never moves twice.
	WastedSpendDuplicate WastedSpendAction = "duplicate"
)

// WastedSpendReport is how OpenRails handled one wasted-spend report. The
// Policy* amounts are in the policy's currency, null when no policy applied.
type WastedSpendReport struct {
	Currency             string            `json:"currency"`
	PolicyCurrency       *string           `json:"policy_currency"`
	RecordedAmount       int64             `json:"recorded_amount,string"`
	PolicyRecordedAmount *int64            `json:"policy_recorded_amount,string"`
	ForgivenAmount       int64             `json:"forgiven_amount,string"`
	PolicyForgivenAmount *int64            `json:"policy_forgiven_amount,string"`
	ChargedAmount        int64             `json:"charged_amount,string"`
	PolicyChargedAmount  *int64            `json:"policy_charged_amount,string"`
	Action               WastedSpendAction `json:"action"`
}
