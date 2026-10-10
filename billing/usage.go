package billing

import (
	"time"

	"github.com/google/uuid"
)

// UsageEventID names one recorded usage event.
type UsageEventID uuid.UUID

const UsageEventIDPrefix = "uev_"

func ParseUsageEventID(s string) (UsageEventID, error) {
	u, err := parsePrefixedID("usage event", UsageEventIDPrefix, s)
	return UsageEventID(u), err
}
func (id UsageEventID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id UsageEventID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id UsageEventID) String() string  { return formatPrefixedID(UsageEventIDPrefix, uuid.UUID(id)) }
func (id UsageEventID) MarshalText() ([]byte, error) {
	return []byte(id.String()), nil
}
func (id *UsageEventID) UnmarshalText(text []byte) error {
	parsed, err := ParseUsageEventID(string(text))
	*id = parsed
	return err
}

// UsageOutcome is whether the work a usage event records delivered.
type UsageOutcome string

const (
	UsageSucceeded UsageOutcome = "succeeded"
	// UsageFailed is work that cost the platform and did not deliver. The
	// customer's own failures are forgiven up to the grace windows of its
	// billing policy (bad_spend_windows) and charged past them; with no window
	// nothing is charged. A delegated invoker's failures are never charged and
	// count toward its cutoff (delegated_invoker_wasted_spend_limits), past
	// which admission refuses it failure_rate_limited.
	UsageFailed UsageOutcome = "failed"
)

// RecordUsageParams records one metered usage event for a customer. Source and
// SourceID identify the event within its EventType, whatever its outcome: a
// retry with the same Amount and Outcome records nothing new, one with a
// different Amount or Outcome is ErrIdempotencyKeyReused.
type RecordUsageParams struct {
	CustomerID CustomerID `json:"customer_id"`
	Invoker    string     `json:"invoker"`
	// InvokerType says whose credential the invoker presented (customer when
	// empty); it decides how a failure is handled.
	InvokerType InvokerType      `json:"invoker_type,omitempty"`
	Currency    string           `json:"currency"`
	EventType   string           `json:"event_type"`
	Dimensions  map[string]int64 `json:"dimensions,omitempty"`
	// Amount is the host-priced cost in native units. Zero records a
	// metered-only event that the catalog's rate cards price; a failed event's
	// Amount is what the failure cost, and is never catalog-rated.
	Amount int64 `json:"amount,string"`
	// Outcome is succeeded when empty.
	Outcome  UsageOutcome   `json:"outcome,omitempty"`
	Resource string         `json:"resource,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Source   string         `json:"source"`
	SourceID string         `json:"source_id"`
	// OccurredAt places the event in its rating window; nil is now. It may be
	// up to 35 days in the past and not in the future, and the event's Source
	// and SourceID are honoured as its idempotency key for those 35 days.
	OccurredAt *time.Time `json:"occurred_at,omitempty"`
}

// UsageEvent is one recorded usage event. Amount is what it charged; a failed
// event's ForgivenAmount is what its grace absorbed, so Amount +
// ForgivenAmount is the cost the host reported. Replayed: the event was
// already recorded and this call metered nothing.
type UsageEvent struct {
	ID             UsageEventID     `json:"id"`
	CustomerID     CustomerID       `json:"customer_id"`
	Invoker        string           `json:"invoker"`
	Currency       string           `json:"currency"`
	EventType      string           `json:"event_type"`
	Dimensions     map[string]int64 `json:"dimensions"`
	Outcome        UsageOutcome     `json:"outcome"`
	Amount         int64            `json:"amount,string"`
	ForgivenAmount int64            `json:"forgiven_amount,string"`
	Resource       *string          `json:"resource"`
	Metadata       map[string]any   `json:"metadata"`
	Source         string           `json:"source"`
	SourceID       string           `json:"source_id"`
	// BalanceTransactionID is the ledger debit a priced event made.
	BalanceTransactionID *BalanceTransactionID `json:"balance_transaction_id"`
	OccurredAt           time.Time             `json:"occurred_at"`
	CreatedAt            time.Time             `json:"created_at"`
	Replayed             bool                  `json:"replayed"`
}

// MaxUsageBatchItems bounds one RecordUsage call.
const MaxUsageBatchItems = 1000

// RecordUsageBatchParams records 1 to MaxUsageBatchItems usage events.
type RecordUsageBatchParams struct {
	Items []RecordUsageParams `json:"items"`
}

// UsageEventResult is one item's outcome: Status is what recording it alone
// answers (201 recorded, 200 replayed, or the refusal's status), with Event
// or Error.
type UsageEventResult struct {
	Status int           `json:"status"`
	Event  *UsageEvent   `json:"event"`
	Error  *ErrorDetails `json:"error"`
}

// Err is the item's refusal as the error recording it alone would return,
// nil when it was recorded or replayed.
func (r UsageEventResult) Err() error {
	if r.Error == nil {
		return nil
	}
	return &StatusError{Status: r.Status, ErrorDetails: *r.Error}
}

// RecordUsageBatchResult is one result per item, in request order.
type RecordUsageBatchResult struct {
	Items []UsageEventResult `json:"items"`
}

// UsageGroupBy is what a usage report groups events by.
type UsageGroupBy string

const (
	UsageByEventType UsageGroupBy = "event_type"
	UsageByResource  UsageGroupBy = "resource"
	UsageByInvoker   UsageGroupBy = "invoker"
)

// GetUsageParams selects a usage report: one currency over [From, To), grouped
// by GroupBy (event_type when empty).
type GetUsageParams struct {
	Currency string       `form:"currency"`
	From     time.Time    `form:"from"`
	To       time.Time    `form:"to"`
	GroupBy  UsageGroupBy `form:"group_by"`
}

// UsageRow is one group of a usage report. Dimensions are the summed event
// dimensions, reported when grouping by event_type.
type UsageRow struct {
	Key        string           `json:"key"`
	EventCount int64            `json:"event_count"`
	Amount     int64            `json:"amount,string"`
	Dimensions map[string]int64 `json:"dimensions"`
}

// Usage is a customer's usage over a window.
type Usage struct {
	CustomerID CustomerID   `json:"customer_id"`
	Currency   string       `json:"currency"`
	From       time.Time    `json:"from"`
	To         time.Time    `json:"to"`
	GroupBy    UsageGroupBy `json:"group_by"`
	Rows       []UsageRow   `json:"rows"`
}
