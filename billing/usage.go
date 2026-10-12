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
	// Amount is the host-priced cost in native units. Zero records an event
	// the rate cards of its event type's meters price when it is recorded
	// (UsageEvent.Charges); a failed event's Amount is what the failure cost,
	// and is never rated.
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

// UsageEvent is one recorded usage event. Amount is what it charged: the
// host's price, or the sum of its Charges when its meters' rate cards priced
// it. A failed event's ForgivenAmount is what its grace absorbed, so Amount +
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
	// Charges are what each priced meter charged the event when it was
	// recorded; empty for an event no priced meter consumes and for a
	// host-priced event.
	Charges []UsageCharge `json:"charges"`
	// BalanceTransactionID is the ledger posting a priced event made: its
	// first charge's when meters priced it.
	BalanceTransactionID *BalanceTransactionID `json:"balance_transaction_id"`
	OccurredAt           time.Time             `json:"occurred_at"`
	CreatedAt            time.Time             `json:"created_at"`
	Replayed             bool                  `json:"replayed"`
}

// UsageCharge is what one meter charged one usage event: R(Q+q) - R(Q), where
// q is Quantity in the meter's unit, Q the customer's quantity of the meter
// (and Cell) earlier in the event's billing period, and R the rate card's
// price for a period. Cell is the matrix cell it was priced in, null without
// a matrix.
type UsageCharge struct {
	MeterKey string  `json:"meter_key"`
	Cell     *string `json:"cell"`
	Quantity int64   `json:"quantity"`
	Amount   int64   `json:"amount,string"`
}

// ErrUsageNotPriced is a usage event a priced meter cannot price: its matrix
// has no cell for the event's value, or no rate card of the event type is in
// its currency. A catalog gap is refused, never free.
var ErrUsageNotPriced error = newCodedError(CodeUsageNotPriced, ErrInvalid)

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
	// UsageByMeter groups what each meter, and matrix cell, charged.
	UsageByMeter UsageGroupBy = "meter"
)

// DefaultUsagePageLimit and MaxUsagePageLimit bound a usage report's page of
// groups.
const (
	DefaultUsagePageLimit = 100
	MaxUsagePageLimit     = 1000
)

// GetUsageParams selects a usage report: one currency over [From, To), grouped
// by GroupBy (event_type when empty), a page of at most Limit groups
// (DefaultUsagePageLimit when zero) after Cursor. A zero From or To is the
// cursor's, or without a cursor the month before now. A cursor reads only the
// report it came from: the same currency, window and grouping.
type GetUsageParams struct {
	Currency string       `form:"currency"`
	From     time.Time    `form:"from"`
	To       time.Time    `form:"to"`
	GroupBy  UsageGroupBy `form:"group_by"`
	PageRequest
}

// UsageRow is one group of a usage report. Dimensions are the summed event
// dimensions, reported when grouping by event_type. Grouped by meter, Key is
// the meter key, Cell its matrix cell (null without a matrix), Quantity the
// summed quantity it priced and Amount what it charged; both are null in
// other groupings.
type UsageRow struct {
	Key        string           `json:"key"`
	Cell       *string          `json:"cell"`
	EventCount int64            `json:"event_count"`
	Amount     int64            `json:"amount,string"`
	Quantity   *int64           `json:"quantity"`
	Dimensions map[string]int64 `json:"dimensions"`
}

// Usage is one page of a customer's usage over a window: its groups by total
// amount, largest first, then by key. NextCursor reads the next page; nil on
// the last.
type Usage struct {
	CustomerID CustomerID   `json:"customer_id"`
	Currency   string       `json:"currency"`
	From       time.Time    `json:"from"`
	To         time.Time    `json:"to"`
	GroupBy    UsageGroupBy `json:"group_by"`
	Rows       []UsageRow   `json:"rows"`
	NextCursor *string      `json:"next_cursor"`
}
