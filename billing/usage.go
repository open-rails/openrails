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

// GetUsageParams selects a usage report: one currency over the days of
// Timezone (an IANA zone; the merchant's accounting zone when empty)
// overlapping [From, To), grouped by GroupBy (event_type when empty; by
// invoker the window reaches back 90 days, CodeUsageEventsExpired), a page
// of at most Limit groups (DefaultUsagePageLimit when zero) after Cursor. A
// zero From or To is the cursor's, or without a cursor the month before now.
// A cursor reads only the report it came from: the same currency, zone,
// window and grouping.
type GetUsageParams struct {
	Currency catalog.Currency `form:"currency"`
	Timezone string           `form:"timezone"`
	From     time.Time        `form:"from"`
	To       time.Time        `form:"to"`
	GroupBy  UsageGroupBy     `form:"group_by"`
	PageRequest
}

// UsageRow is one group of a usage report: EventCount usage items and Amount
// what they cost at their rate cards when taken, failed usage's charges
// included; in a merchant's own unit that includes any part a posting
// forgave. Grouped by meter, Key is the meter key, Cell its
// matrix cell (null without a matrix), EventCount the items it priced,
// Quantity the summed quantity and Amount what it cost; Quantity is null in
// other groupings.
type UsageRow struct {
	Key        string  `json:"key"`
	Cell       *string `json:"cell"`
	EventCount int64   `json:"event_count"`
	Amount     int64   `json:"amount,string"`
	Quantity   *int64  `json:"quantity"`
}

// Usage is one page of a customer's usage over a window, on Timezone's days:
// its groups by total amount, largest first, then by key. Amounts are the
// prices usage was taken at; Discount is what volume tiers took off the
// window's usage as its quantity grew, so every group's amount less it is
// what posts. NextCursor reads the next page; nil on the last.
type Usage struct {
	CustomerID CustomerID       `json:"customer_id"`
	Currency   catalog.Currency `json:"currency"`
	Timezone   string           `json:"timezone"`
	From       time.Time        `json:"from"`
	To         time.Time        `json:"to"`
	GroupBy    UsageGroupBy     `json:"group_by"`
	Rows       []UsageRow       `json:"rows"`
	Discount   int64            `json:"discount,string"`
	NextCursor *string          `json:"next_cursor"`
}

// UsageInterval is the length of a usage series' buckets on its zone's
// calendar.
type UsageInterval string

const (
	// UsageByHour counts hours from each day's first instant: a day of 23 or
	// 25 hours has as many.
	UsageByHour  UsageInterval = "hour"
	UsageByDay   UsageInterval = "day"
	UsageByWeek  UsageInterval = "week" // from Monday
	UsageByMonth UsageInterval = "month"
)

// MaxUsageBuckets is the most buckets a usage series answers: a month by
// hour fits.
const MaxUsageBuckets = 1000

// GetUsageSeriesParams selects a usage series: one currency over the
// buckets of Interval on Timezone's calendar (an IANA zone; the merchant's
// accounting zone when empty) that overlap [From, To), each grouped by
// GroupBy (event_type when empty; by invoker the window reaches back 90
// days). A zero From or To is the month before now.
// More than MaxUsageBuckets buckets is refused, CodeTooManyBuckets.
type GetUsageSeriesParams struct {
	Currency catalog.Currency `form:"currency"`
	Interval UsageInterval    `form:"interval"`
	Timezone string           `form:"timezone"`
	From     time.Time        `form:"from"`
	To       time.Time        `form:"to"`
	GroupBy  UsageGroupBy     `form:"group_by"`
}

// UsageSeries is a customer's usage over time: one bucket per Interval of
// Timezone's calendar from From to To (the window snapped out to whole
// buckets), empty ones included, each with its groups as a usage report
// orders them. The buckets sum to the report of the same window.
type UsageSeries struct {
	CustomerID CustomerID       `json:"customer_id"`
	Currency   catalog.Currency `json:"currency"`
	Interval   UsageInterval    `json:"interval"`
	Timezone   string           `json:"timezone"`
	From       time.Time        `json:"from"`
	To         time.Time        `json:"to"`
	GroupBy    UsageGroupBy     `json:"group_by"`
	Buckets    []UsageBucket    `json:"buckets"`
}

// UsageBucket is one bucket of a usage series: StartsAt is its local start
// as a UTC instant; it ends where the next starts, the last at the series'
// To. Discount is what volume tiers took off its usage, as on Usage.
type UsageBucket struct {
	StartsAt time.Time  `json:"starts_at"`
	Rows     []UsageRow `json:"rows"`
	Discount int64      `json:"discount,string"`
}

// UsageReceipt is what one posting of a customer's metered spend in a
// currency took: the usage that occurred before Through, per pricing period
// and meter cell the period's price less what earlier postings took, and
// what each running charge accrued since its last posting. Amount sums them;
// a period whose volume price fell has negative lines.
type UsageReceipt struct {
	Through time.Time            `json:"through"`
	Amount  int64                `json:"amount,string"`
	Lines   []UsageReceiptLine   `json:"lines"`
	Charges []UsageReceiptCharge `json:"charges"`
}

// UsageReceiptLine is what a receipt added for one meter cell in a pricing
// period: requests, quantity and amount. Cell is null without a matrix.
type UsageReceiptLine struct {
	PeriodStartsAt time.Time `json:"period_starts_at"`
	PeriodEndsAt   time.Time `json:"period_ends_at"`
	MeterKey       string    `json:"meter_key"`
	Cell           *string   `json:"cell"`
	Requests       int64     `json:"requests"`
	Quantity       int64     `json:"quantity"`
	Amount         int64     `json:"amount,string"`
}

// UsageReceiptCharge is what a running charge accrued from StartsAt, its last
// posting, to EndsAt.
type UsageReceiptCharge struct {
	RunningChargeID RunningChargeID `json:"running_charge_id"`
	Reference       string          `json:"reference"`
	Resource        *string         `json:"resource"`
	StartsAt        time.Time       `json:"starts_at"`
	EndsAt          time.Time       `json:"ends_at"`
	Amount          int64           `json:"amount,string"`
}
