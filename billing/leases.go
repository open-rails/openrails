package billing

import (
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/catalog"
)

// LeaseID names one gate's lease. The client picks the UUID: a gate when it
// starts, a batch job once for its whole stream.
type LeaseID uuid.UUID

const leaseIDPrefix = "lse_"

func ParseLeaseID(s string) (LeaseID, error) {
	u, err := parsePrefixedID("lease", leaseIDPrefix, s)
	return LeaseID(u), err
}
func (id LeaseID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id LeaseID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id LeaseID) String() string  { return formatPrefixedID(leaseIDPrefix, uuid.UUID(id)) }
func (id LeaseID) MarshalText() ([]byte, error) {
	return []byte(id.String()), nil
}
func (id *LeaseID) UnmarshalText(text []byte) error {
	parsed, err := ParseLeaseID(string(text))
	*id = parsed
	return err
}

// Bounds of one SyncLeases call.
const (
	MaxSyncUsageItems = 1000
	MaxSyncViews      = 1000
	MaxSyncInFlight   = 10_000
)

// SyncLeasesParams is one sync of a lease: the usage it settles, what its
// gate has in flight beyond it, and the views it asks for. The first sync
// under a new LeaseID creates the lease (its id must be a UUIDv7 no older
// than the replay horizon); every later one renews it, until one with Close:
// that sync settles its usage, clears its in-flight report and ends it.
// Usage against a closed lease still settles. A process resending an earlier
// lease's usage sends Close.
type SyncLeasesParams struct {
	LeaseID LeaseID      `json:"lease_id"`
	Usage   []LeaseUsage `json:"usage"`
	// InFlight replaces the lease's last report; null or empty clears it.
	InFlight []InFlight   `json:"in_flight"`
	Views    []ViewParams `json:"views"`
	Close    bool         `json:"close,omitempty"`
}

// InFlight is what a gate has had in flight for one customer's limit across
// at least one sync: tickets open since before its previous sync, and usage
// a previous sync left unsettled. Other leases' views count
// it as held while the lease is live.
type InFlight struct {
	CustomerID CustomerID `json:"customer_id"`
	// Limit is balance, a declared limit or a spend window's key; Unit is
	// its unit (balance's currency).
	Limit  string `json:"limit"`
	Unit   string `json:"unit"`
	Amount int64  `json:"amount,string"`
}

// LeaseUsage is one usage item of a lease, numbered once per lease and sent
// in order. N decides duplicates: a resent item answers its receipt, a
// changed payload under a settled N is ErrIdempotencyKeyReused and a gap is
// refused usage_out_of_order. SourceID is the merchant's call id; Amount the
// stated amount a dynamic card charges. TrustLevel, Roles and Resource are
// the invoker attributes billing policy resolves its spend windows from.
// AdmittedAt places the usage in the limit windows current when its request
// was admitted; null, in those current when it settles.
type LeaseUsage struct {
	N           int64            `json:"n"`
	CustomerID  CustomerID       `json:"customer_id"`
	Invoker     string           `json:"invoker"`
	InvokerType InvokerType      `json:"invoker_type"`
	TrustLevel  *string          `json:"trust_level"`
	Roles       []uuid.UUID      `json:"roles"`
	Resource    *string          `json:"resource"`
	Currency    catalog.Currency `json:"currency"`
	Source      string           `json:"source"`
	SourceID    string           `json:"source_id"`
	Outcome     UsageOutcome     `json:"outcome"`
	EventType   string           `json:"event_type"`
	Dimensions  map[string]int64 `json:"dimensions"`
	Metadata    map[string]any   `json:"metadata"`
	Amount      *int64           `json:"amount,string"`
	OccurredAt  time.Time        `json:"occurred_at"`
	AdmittedAt  *time.Time       `json:"admitted_at"`
}

// LeaseUsageRef names one usage item of a lease.
type LeaseUsageRef struct {
	LeaseID LeaseID `json:"lease_id"`
	N       int64   `json:"n"`
}

// OutboxItem is one usage item of a lease, as a gate's outbox stores it.
type OutboxItem struct {
	LeaseID LeaseID    `json:"lease_id"`
	Usage   LeaseUsage `json:"usage"`
}

// ViewParams names a customer and the invoker attributes of its requests, as
// AdmitParams names them, whose view a sync answers. EventType is null for
// requests that state an amount.
type ViewParams struct {
	CustomerID  CustomerID       `json:"customer_id"`
	Invoker     string           `json:"invoker"`
	InvokerType InvokerType      `json:"invoker_type"`
	TrustLevel  *string          `json:"trust_level"`
	Roles       []uuid.UUID      `json:"roles"`
	Resource    *string          `json:"resource"`
	Currency    catalog.Currency `json:"currency"`
	EventType   *string          `json:"event_type"`
}

// SyncLeasesResult answers a sync: the lease, what its usage settled, a view
// of each customer and currency it named (read after its usage settled), the
// limit set of each ViewParams in order (null when the sync ran out of time
// for it: ask again, or when RefusedViews names it), the pricing of each
// currency and event type named, and how many of the merchant's leases are
// live.
type SyncLeasesResult struct {
	Lease     Lease            `json:"lease"`
	Usage     LeaseUsageResult `json:"usage"`
	Views     []View           `json:"views"`
	LimitSets []*LimitSet      `json:"limit_sets"`
	// RefusedViews are the ViewParams refused alone, as Admit would refuse
	// their requests; the rest of the sync stands.
	RefusedViews []ViewRefusal `json:"refused_views"`
	Pricing      []ViewPricing `json:"pricing"`
	Gates        int           `json:"gates"`
}

// ViewRefusal is one refused ViewParams, by its index in Views: Status is
// what it alone answers, with Error.
type ViewRefusal struct {
	Index  int           `json:"index"`
	Status int           `json:"status"`
	Error  *ErrorDetails `json:"error"`
}

// Lease is a gate's lease: it is live until ExpiresAt unless its gate syncs
// again. A closed lease's ExpiresAt is when it closed.
type Lease struct {
	ID        LeaseID   `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// LeaseUsageResult is the lease's highest settled item and the items the
// sync refused, which posted nothing. A sync settles a prefix of its items:
// one past ThroughN that it did not refuse is unsettled, to send again.
type LeaseUsageResult struct {
	ThroughN int64               `json:"through_n"`
	Refused  []LeaseUsageRefusal `json:"refused"`
}

// LeaseUsageRefusal is one refused usage item: Status is what it alone
// answers, with Error.
type LeaseUsageRefusal struct {
	N      int64         `json:"n"`
	Status int           `json:"status"`
	Error  *ErrorDetails `json:"error"`
}

// View is where one customer stands on every limit its requests in Currency
// may claim, as of AsOf: balance, each declared limit it is granted in the
// currency or a named unit, and the spend windows of the limit sets the sync
// resolved. It reserves nothing.
type View struct {
	CustomerID CustomerID       `json:"customer_id"`
	Currency   catalog.Currency `json:"currency"`
	AsOf       time.Time        `json:"as_of"`
	Limits     []ViewLimit      `json:"limits"`
}

// ViewLimit is one limit of a view in its current window. Held is what other
// live leases' in-flight reports set aside; Remaining is Capacity − Used −
// Held (for balance, what the customer may spend before any credit line). A
// session not yet open has no window.
type ViewLimit struct {
	Limit          string              `json:"limit"`
	Unit           string              `json:"unit"`
	Reset          *catalog.LimitReset `json:"reset"`
	Overrun        catalog.Overrun     `json:"overrun"`
	Capacity       int64               `json:"capacity,string"`
	Used           int64               `json:"used,string"`
	Held           int64               `json:"held,string"`
	Remaining      int64               `json:"remaining,string"`
	WindowStartsAt *time.Time          `json:"window_starts_at"`
	WindowEndsAt   *time.Time          `json:"window_ends_at"`
	ResetsAt       *time.Time          `json:"resets_at"`
	// Next is the window after WindowEndsAt; a null StartsAt opens at the
	// first use after it (a session).
	Next *ViewWindow `json:"next"`
	// DrainPerHour is what running charges take from balance each hour.
	DrainPerHour int64 `json:"drain_per_hour,string"`
}

// ViewWindow is a limit's next window and what it will hold.
type ViewWindow struct {
	StartsAt  *time.Time `json:"starts_at"`
	EndsAt    *time.Time `json:"ends_at"`
	Remaining int64      `json:"remaining,string"`
}

// LimitSet is what a request with a ViewParams' attributes claims after
// policy resolution: the keys of its view's limits, in the order a refusal
// names them (balance, spend windows, declared limits), the capacity its
// policy gives each spend window (another policy may cap the same window
// otherwise), the credit line it gives balance and the code balance refuses
// with, or a Refusal whatever the request costs.
type LimitSet struct {
	CustomerID      CustomerID       `json:"customer_id"`
	Currency        catalog.Currency `json:"currency"`
	Limits          []string         `json:"limits"`
	Capacities      []LimitCapacity  `json:"capacities"`
	CreditLine      int64            `json:"credit_line,string"`
	BalanceDenyCode DenyCode         `json:"balance_deny_code"`
	Refusal         *Refusal         `json:"refusal"`
}

// LimitCapacity is the capacity a limit set's policy gives one spend window.
type LimitCapacity struct {
	Limit    string `json:"limit"`
	Capacity int64  `json:"capacity,string"`
}

// ViewPricing is how a gate costs a request of EventType in Currency: how
// each named-unit limit counts it, and the money rate cards in force with
// their price bounds. The server prices settlement exactly.
type ViewPricing struct {
	Currency    catalog.Currency `json:"currency"`
	EventType   string           `json:"event_type"`
	Counts      []ViewCount      `json:"counts"`
	RateCards   []ViewRateCard   `json:"rate_cards"`
	PriceBounds []PriceBound     `json:"price_bounds"`
}

// ViewCount is how one named-unit limit counts a meter: each matching
// request adds quantity × Weight ÷ Per of its unit.
type ViewCount struct {
	Limit         string              `json:"limit"`
	Meter         string              `json:"meter"`
	EventType     string              `json:"event_type"`
	ValueProperty string              `json:"value_property"`
	Aggregation   catalog.Aggregation `json:"aggregation"`
	GroupBy       map[string]string   `json:"group_by"`
	Filter        map[string][]string `json:"filter"`
	Weight        int64               `json:"weight"`
	Per           int64               `json:"per"`
}

// ViewRateCard is a meter as the catalog declares it and the money rate
// card that prices it.
type ViewRateCard struct {
	Meter         string              `json:"meter"`
	EventType     string              `json:"event_type"`
	ValueProperty string              `json:"value_property"`
	Aggregation   catalog.Aggregation `json:"aggregation"`
	GroupBy       map[string]string   `json:"group_by"`
	Filter        map[string][]string `json:"filter"`
	Allowance     *catalog.Allowance  `json:"allowance"`
	Price         catalog.RatePrice   `json:"price"`
}

// PriceBound is the highest marginal price of one meter cell: UnitAmount
// native units per DivideBy units. Cell is null without a matrix.
type PriceBound struct {
	Meter      string  `json:"meter"`
	Cell       *string `json:"cell"`
	UnitAmount int64   `json:"unit_amount,string"`
	DivideBy   int64   `json:"divide_by,string"`
}
