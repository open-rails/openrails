package billing

import (
	"time"

	"github.com/open-rails/openrails/catalog"
)

// UsageOutcome is whether the work a usage report records delivered.
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

// UsageParams is one report of metered usage with no admission: a gate's
// Record numbers it on its lease and sends it on the next sync. SourceID is
// the merchant's call id.
type UsageParams struct {
	CustomerID CustomerID `json:"customer_id"`
	Invoker    string     `json:"invoker"`
	// InvokerType says whose credential the invoker presented (customer when
	// empty); it decides how a failure is handled.
	InvokerType InvokerType      `json:"invoker_type,omitempty"`
	Currency    catalog.Currency `json:"currency"`
	EventType   string           `json:"event_type"`
	Dimensions  map[string]int64 `json:"dimensions,omitempty"`
	// Amount is the optional native-unit cost a dynamic card charges. Other
	// cards ignore it. A failed report must state its cost and is never rated.
	Amount *int64 `json:"amount,omitempty,string"`
	// Outcome is succeeded when empty.
	Outcome  UsageOutcome   `json:"outcome,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Source   string         `json:"source"`
	SourceID string         `json:"source_id"`
	// OccurredAt places the usage in its pricing period and day; nil is now.
	// It may be up to 35 days in the past and not in the future.
	OccurredAt *time.Time `json:"occurred_at,omitempty"`
}

// UsageCharge is what one meter charged one usage item. Dynamic charges
// take the stated amount; other models price the group of items settled
// together, R(Q+Σq) - R(Q), and share it out by quantity: q is Quantity in
// the meter's unit, Q the customer's quantity of the meter (and Cell)
// earlier in the billing period, and R the rate card's price for a period.
// Cell is the matrix cell it was priced in, null without a matrix.
type UsageCharge struct {
	MeterKey string        `json:"meter_key"`
	Model    catalog.Model `json:"model"`
	Cell     *string       `json:"cell"`
	Quantity int64         `json:"quantity"`
	Amount   int64         `json:"amount,string"`
}

// ErrUsageNotPriced is usage a priced meter cannot price: its matrix
// has no cell for the event's value, or no rate card of the event type is in
// its currency. A catalog gap is refused, never free.
var ErrUsageNotPriced error = newCodedError(CodeUsageNotPriced, ErrInvalid)

// UsageGroupBy is what a usage report groups usage by.
type UsageGroupBy string

const (
	UsageByEventType UsageGroupBy = "event_type"
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

// GetUsageParams selects a usage report: one currency over the days of the
// merchant's accounting zone overlapping [From, To), grouped by GroupBy (event_type when empty), a page
// of at most Limit groups (DefaultUsagePageLimit when zero) after Cursor. A
// zero From or To is the cursor's, or without a cursor the month before now.
// A cursor reads only the report it came from: the same currency, window and
// grouping.
type GetUsageParams struct {
	Currency catalog.Currency `form:"currency"`
	From     time.Time        `form:"from"`
	To       time.Time        `form:"to"`
	GroupBy  UsageGroupBy     `form:"group_by"`
	PageRequest
}

// UsageRow is one group of a usage report: EventCount usage items and Amount
// what they charged. Grouped by meter, Key is the meter key, Cell its matrix
// cell (null without a matrix), EventCount the items it priced, Quantity the
// summed quantity and Amount what it charged; Quantity is null in other
// groupings.
type UsageRow struct {
	Key        string  `json:"key"`
	Cell       *string `json:"cell"`
	EventCount int64   `json:"event_count"`
	Amount     int64   `json:"amount,string"`
	Quantity   *int64  `json:"quantity"`
}

// Usage is one page of a customer's usage over a window: its groups by total
// amount, largest first, then by key. NextCursor reads the next page; nil on
// the last.
type Usage struct {
	CustomerID CustomerID       `json:"customer_id"`
	Currency   catalog.Currency `json:"currency"`
	From       time.Time        `json:"from"`
	To         time.Time        `json:"to"`
	GroupBy    UsageGroupBy     `json:"group_by"`
	Rows       []UsageRow       `json:"rows"`
	NextCursor *string          `json:"next_cursor"`
}

// UsageDeduction is what one posting of a customer's usage was for: the
// pricing period, the instant usage before which it took (Through), Amount
// the period's price less what earlier postings took, and per meter and cell
// what it added: requests, quantity and amount. A volume tier whose price
// fell credits a negative Amount.
type UsageDeduction struct {
	PeriodStartsAt time.Time            `json:"period_starts_at"`
	PeriodEndsAt   time.Time            `json:"period_ends_at"`
	Through        time.Time            `json:"through"`
	Amount         int64                `json:"amount,string"`
	Lines          []UsageDeductionLine `json:"lines"`
}

// UsageDeductionLine is one meter cell of a usage deduction; Cell is null
// without a matrix.
type UsageDeductionLine struct {
	MeterKey string  `json:"meter_key"`
	Cell     *string `json:"cell"`
	Requests int64   `json:"requests"`
	Quantity int64   `json:"quantity"`
	Amount   int64   `json:"amount,string"`
}
