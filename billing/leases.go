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
	MaxSyncWants      = 1000
	MaxSyncReturns    = 1000
	MaxSyncCommitted  = 10_000
	MaxSyncApplied    = 10_000
)

// SyncLeasesParams is one gate's sync: the usage it settles, the holds it
// wants and the holds it gives back. The first sync under a new LeaseID
// creates the lease (its id must be a UUIDv7 no older than the replay
// horizon); every later one renews it while it is live, until one with Close:
// that sync settles its usage, returns every hold of the lease and ends it. A
// closed or dead lease answers its wants lease_not_held; usage against it
// still settles. A process resending an earlier lease's usage sends Close.
type SyncLeasesParams struct {
	LeaseID LeaseID      `json:"lease_id"`
	Usage   []LeaseUsage `json:"usage"`
	Wants   []LeaseWant  `json:"wants"`
	Returns []HoldReturn `json:"returns"`
	Close   bool         `json:"close,omitempty"`
	// Committed is what the gate's open tickets have taken of its holds, as
	// the sync was built: every hold with open tickets, none for the rest.
	// A want asks for its estimate beyond its hold's commitment.
	Committed []HoldCommitment `json:"committed"`
	// Applied is the latest hold version the gate has applied, for each hold
	// whose answer it received since its last answered sync. A smaller size
	// is released only once the gate has applied the version that carried
	// it, and never below what that sync's commitment holds, so capacity a
	// ticket took is never given to another gate, even after a lost reply.
	Applied []HoldApplied `json:"applied"`
}

// HoldApplied is the hold version a gate has applied.
type HoldApplied struct {
	CustomerID CustomerID `json:"customer_id"`
	LimitSet   string     `json:"limit_set"`
	Version    int64      `json:"version"`
}

// HoldCommitment is what open tickets have taken of one hold.
type HoldCommitment struct {
	CustomerID CustomerID        `json:"customer_id"`
	LimitSet   string            `json:"limit_set"`
	Limits     []LimitCommitment `json:"limits"`
}

// LimitCommitment is what open tickets have taken of one limit of a hold,
// in the window the hold set it aside in (null for balance and lifetime
// limits).
type LimitCommitment struct {
	Limit          string     `json:"limit"`
	WindowStartsAt *time.Time `json:"window_starts_at"`
	Committed      int64      `json:"committed,string"`
}

// LeaseUsage is one usage item of a lease, numbered once per lease and sent
// in order. N decides duplicates: a resent item answers its receipt, a
// changed payload under a settled N is ErrIdempotencyKeyReused and a gap is
// refused usage_out_of_order. LimitSet is null for usage with no hold.
// SourceID is the merchant's call id; Amount the stated amount a dynamic
// card charges.
type LeaseUsage struct {
	N           int64            `json:"n"`
	CustomerID  CustomerID       `json:"customer_id"`
	LimitSet    *string          `json:"limit_set"`
	Invoker     string           `json:"invoker"`
	InvokerType InvokerType      `json:"invoker_type"`
	Currency    string           `json:"currency"`
	Source      string           `json:"source"`
	SourceID    string           `json:"source_id"`
	Outcome     UsageOutcome     `json:"outcome"`
	EventType   string           `json:"event_type"`
	Dimensions  map[string]int64 `json:"dimensions"`
	Metadata    map[string]any   `json:"metadata"`
	Amount      *int64           `json:"amount,string"`
	OccurredAt  time.Time        `json:"occurred_at"`
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

// LeaseWant asks for a hold for one customer and the invoker attributes of
// a request, as AdmitParams names them, with the request's usage estimate.
type LeaseWant struct {
	CustomerID  CustomerID     `json:"customer_id"`
	Invoker     string         `json:"invoker"`
	InvokerType InvokerType    `json:"invoker_type"`
	TrustLevel  *string        `json:"trust_level"`
	Roles       []uuid.UUID    `json:"roles"`
	Resource    *string        `json:"resource"`
	Currency    string         `json:"currency"`
	Usage       *UsageEstimate `json:"usage"`
	// EstimatedAmount is a host-stated money estimate, for a want with no
	// Usage, as AdmitParams states one.
	EstimatedAmount *int64 `json:"estimated_amount,string"`
}

// HoldReturn gives a lease's hold for a customer and limit set back.
type HoldReturn struct {
	CustomerID CustomerID `json:"customer_id"`
	LimitSet   string     `json:"limit_set"`
}

// SyncLeasesResult answers a sync: the lease, what its usage settled, one
// answer per want in order, every hold the sync created, topped up, shrank or
// took back, and display values of the customers' limits, which never add
// spendable capacity.
type SyncLeasesResult struct {
	Lease  Lease             `json:"lease"`
	Usage  LeaseUsageResult  `json:"usage"`
	Wants  []LeaseWantResult `json:"wants"`
	Holds  []Hold            `json:"holds"`
	Limits []CustomerLimit   `json:"limits"`
}

// Lease is a gate's lease: it lives until ExpiresAt unless the gate syncs
// again, and its holds live and die with it. A closed lease's ExpiresAt is
// when it closed.
type Lease struct {
	ID        LeaseID   `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
	// Live is false when the lease had already died or closed before this
	// sync, which renewed nothing: the gate starts a new lease and uses the
	// old one only to report usage.
	Live bool `json:"live"`
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

// LeaseWantResult answers one want: its limit set, and either the hold (in
// Holds) or a Refusal. Error is a want that could not be decided.
type LeaseWantResult struct {
	Status   int           `json:"status"`
	LimitSet *string       `json:"limit_set"`
	Error    *ErrorDetails `json:"error"`
	Refusal  *Refusal      `json:"refusal"`
}

// Hold is capacity set aside for one customer and limit set, in each limit's
// window. RateCards count usage toward it and PriceBounds bound its money
// price. Safe is money, the hold's balance size, that its gate may spend
// unreserved while OpenRails is unreachable; null unless the merchant sets
// usage_gate_safe_spend. It never covers unit limits.
type Hold struct {
	CustomerID  CustomerID     `json:"customer_id"`
	LimitSet    string         `json:"limit_set"`
	Currency    string         `json:"currency"`
	Limits      []HoldLimit    `json:"limits"`
	RateCards   []HoldRateCard `json:"rate_cards"`
	PriceBounds []PriceBound   `json:"price_bounds"`
	Safe        *int64         `json:"safe,string"`
	// Version increases each time the server answers this hold a new size;
	// the gate reports the latest it applied in SyncLeasesParams.Applied.
	Version int64 `json:"version"`
}

// HoldLimit is what a hold sets aside of one limit, in the window it was
// issued in; balance and lifetime limits have no window.
type HoldLimit struct {
	Limit          string          `json:"limit"`
	Unit           string          `json:"unit"`
	Held           int64           `json:"held,string"`
	Overrun        catalog.Overrun `json:"overrun"`
	WindowStartsAt *time.Time      `json:"window_starts_at"`
	WindowEndsAt   *time.Time      `json:"window_ends_at"`
}

// HoldRateCard is how a gate counts usage toward a hold: a meter as the
// catalog declares it and the rate card that prices it. A card with a Limit
// counts toward that unit limit, priced in its unit; one without prices
// money, pinned with the hold and bounded by its PriceBounds.
type HoldRateCard struct {
	Limit         *string             `json:"limit"`
	Meter         string              `json:"meter"`
	EventType     string              `json:"event_type"`
	ValueProperty string              `json:"value_property"`
	Aggregation   catalog.Aggregation `json:"aggregation"`
	GroupBy       map[string]string   `json:"group_by"`
	Filter        map[string][]string `json:"filter"`
	Allowance     *catalog.Allowance  `json:"allowance"`
	Price         catalog.RatePrice   `json:"price"`
}

// PriceBound is the highest marginal price of one meter cell while the hold
// lives: UnitAmount native units per DivideBy units. Cell is null without a
// matrix.
type PriceBound struct {
	Meter      string  `json:"meter"`
	Cell       *string `json:"cell"`
	UnitAmount int64   `json:"unit_amount,string"`
	DivideBy   int64   `json:"divide_by,string"`
}
