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

// UsageEventParams records one metered usage event for a customer. Source and
// SourceID identify the event within its EventType: a retry with the same
// Amount records nothing new, one with a different Amount is
// ErrIdempotencyKeyReused.
type UsageEventParams struct {
	CustomerID CustomerID       `json:"customer_id"`
	Invoker    string           `json:"invoker"`
	Currency   string           `json:"currency"`
	EventType  string           `json:"event_type"`
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
	// Amount is the host-priced cost in native units. Zero records a
	// metered-only event that the catalog's rate cards price.
	Amount   int64          `json:"amount,string"`
	Resource string         `json:"resource,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Source   string         `json:"source"`
	SourceID string         `json:"source_id"`
	// OccurredAt places the event in its rating window; nil is now. It may be
	// up to 35 days in the past and not in the future, and the event's Source
	// and SourceID are honoured as its idempotency key for those 35 days.
	OccurredAt *time.Time `json:"occurred_at,omitempty"`
}

// UsageEvent is one recorded usage event. Replayed: the event was already
// recorded and this call metered nothing.
type UsageEvent struct {
	ID         UsageEventID     `json:"id"`
	CustomerID CustomerID       `json:"customer_id"`
	Invoker    string           `json:"invoker"`
	Currency   string           `json:"currency"`
	EventType  string           `json:"event_type"`
	Dimensions map[string]int64 `json:"dimensions"`
	Amount     int64            `json:"amount,string"`
	Resource   *string          `json:"resource"`
	Metadata   map[string]any   `json:"metadata"`
	Source     string           `json:"source"`
	SourceID   string           `json:"source_id"`
	// CreditTransactionID is the ledger debit a priced event made.
	CreditTransactionID *CreditTransactionID `json:"credit_transaction_id"`
	OccurredAt          time.Time            `json:"occurred_at"`
	CreatedAt           time.Time            `json:"created_at"`
	Replayed            bool                 `json:"replayed"`
}

// UsageGroupBy is what a usage report groups events by.
type UsageGroupBy string

const (
	UsageByEventType UsageGroupBy = "event_type"
	UsageByResource  UsageGroupBy = "resource"
	UsageByInvoker   UsageGroupBy = "invoker"
	// UsageByFunction and UsageByTier group by the function_name and
	// availability_tier keys of an event's metadata.
	UsageByFunction UsageGroupBy = "function"
	UsageByTier     UsageGroupBy = "tier"
)

// UsageParams selects a usage report: one currency over [From, To), grouped
// by GroupBy (event_type when empty).
type UsageParams struct {
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
