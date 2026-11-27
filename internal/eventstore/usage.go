package eventstore

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// UsageOutcome is usage_events.outcome.
type UsageOutcome string

const (
	UsageSucceeded UsageOutcome = "succeeded"
	UsageFailed    UsageOutcome = "failed"
)

// UsageEvent is one settled usage item as intake takes it in; its merchant is
// the context's. usage_events keeps what was reported, its identity, its
// pricing; Limits only feed the rollups.
type UsageEvent struct {
	// EventID is the intake identity: LeaseEventID of (merchant, lease, n), or
	// RequestEventID of the request id.
	EventID     uuid.UUID
	CustomerID  billing.CustomerID
	Currency    string
	EventType   string
	Invoker     string
	InvokerType string
	Outcome     UsageOutcome
	Source      string
	SourceID    string
	// LeaseID and N number a synced item; zero for a direct capture.
	LeaseID        uuid.UUID
	N              uint64
	OccurredAt     time.Time
	AdmittedAt     time.Time
	IngestedAt     time.Time
	PeriodStartsAt time.Time
	Dimensions     map[string]int64
	Metadata       string
	// Stated is a host-stated amount; Charged and Forgiven split failed usage.
	Stated   *int64
	Charged  int64
	Forgiven int64
	Charges  []UsageCharge
	Limits   []UsageLimit
}

// UsageCharge is a priced meter cell of an item, under the card print in
// force: Amount is the price intake took it at, never below zero, and
// Discount what a volume tier's fall took off the period's price there, so
// Amount less Discount sums to the period's price.
type UsageCharge struct {
	Meter    string `json:"meter"`
	Cell     string `json:"cell"`
	Print    string `json:"print"`
	Quantity int64  `json:"quantity"`
	Stated   int64  `json:"stated"`
	Amount   int64  `json:"amount"`
	Discount int64  `json:"discount"`
}

// UsageLimit is a limit window that counts an item, located at admission.
type UsageLimit struct {
	Key            string
	WindowStartsAt time.Time
	WindowEndsAt   *time.Time
	Units          int64
}

const insertUsage = "INSERT INTO {database}.usage_intake (merchant_id, event_id, customer_id, currency, event_type, invoker, " +
	"invoker_type, outcome, source, source_id, lease_id, n, occurred_at, admitted_at, ingested_at, period_starts_at, " +
	"dimensions, metadata, stated, charged, forgiven, `charges.meter`, `charges.cell`, `charges.print`, " +
	"`charges.quantity`, `charges.stated`, `charges.amount`, `charges.discount`, `limits.key`, `limits.window_starts_at`, `limits.window_ends_at`, `limits.units`)"

// InsertUsage takes events in as one asynchronous insert into usage_intake
// and returns once ClickHouse has written them, raw and rollups: what it
// acknowledged reads back, so a view answered after the sync counts it.
// Intake inserts only what PostgreSQL settled, once: a retry after an error
// that hid a success inserts twice, which raw events collapse on merge and in
// FINAL reads and the rollups count.
func (s *Store) InsertUsage(ctx context.Context, events []UsageEvent) error {
	if len(events) == 0 {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	batch, err := s.conn.PrepareBatch(clickhouse.Context(ctx, clickhouse.WithAsync(true)), s.resolve(insertUsage))
	if err != nil {
		return fail("prepare usage insert", err)
	}
	defer func() { _ = batch.Abort() }()
	for _, e := range events {
		c := chargeColumns(e.Charges)
		l := limitColumns(e.Limits)
		dimensions := e.Dimensions
		if dimensions == nil {
			dimensions = map[string]int64{}
		}
		if err := batch.Append(uuid.UUID(mid), e.EventID, uuid.UUID(e.CustomerID), e.Currency, e.EventType, e.Invoker,
			e.InvokerType, string(e.Outcome), e.Source, e.SourceID, e.LeaseID, e.N, e.OccurredAt.UTC(), e.AdmittedAt.UTC(),
			e.IngestedAt.UTC(), e.PeriodStartsAt.UTC(), dimensions, e.Metadata, e.Stated, e.Charged, e.Forgiven,
			c.meter, c.cell, c.print, c.quantity, c.stated, c.amount, c.discount, l.key, l.startsAt, l.endsAt, l.units); err != nil {
			return fmt.Errorf("eventstore: usage event %s: %w", e.EventID, err)
		}
	}
	return fail("insert usage", batch.Send())
}

// Ping reports whether the store answers now.
func (s *Store) Ping(ctx context.Context) error {
	return fail("ping", s.conn.Ping(ctx))
}

type charges struct {
	meter, cell, print                 []string
	quantity, stated, amount, discount []int64
}

func chargeColumns(in []UsageCharge) charges {
	out := charges{meter: []string{}, cell: []string{}, print: []string{}, quantity: []int64{}, stated: []int64{}, amount: []int64{}, discount: []int64{}}
	for _, c := range in {
		out.meter, out.cell, out.print = append(out.meter, c.Meter), append(out.cell, c.Cell), append(out.print, c.Print)
		out.quantity, out.stated = append(out.quantity, c.Quantity), append(out.stated, c.Stated)
		out.amount, out.discount = append(out.amount, c.Amount), append(out.discount, c.Discount)
	}
	return out
}

type limits struct {
	key      []string
	startsAt []time.Time
	endsAt   []*time.Time
	units    []int64
}

func limitColumns(in []UsageLimit) limits {
	out := limits{key: []string{}, startsAt: []time.Time{}, endsAt: []*time.Time{}, units: []int64{}}
	for _, l := range in {
		ends := l.WindowEndsAt
		if ends != nil {
			utc := ends.UTC()
			ends = &utc
		}
		out.key, out.startsAt = append(out.key, l.Key), append(out.startsAt, l.WindowStartsAt.UTC())
		out.endsAt, out.units = append(out.endsAt, ends), append(out.units, l.Units)
	}
	return out
}

// usageNamespace names usage events' intake identities.
var usageNamespace = uuid.MustParse("6f1c5a3e-2b8d-5c4e-9a71-0d3e8b2f4c19")

// LeaseEventID is the identity of a lease's usage item n, timed at the lease.
func LeaseEventID(mid billing.MerchantID, lease uuid.UUID, n int64) uuid.UUID {
	return uuidutil.DerivedV7(uuidutil.TimeOf(lease), usageNamespace, mid.String()+"/lease/"+lease.String()+"/"+strconv.FormatInt(n, 10))
}

// RequestEventID is the identity of the capture of an admitted request, timed
// at the request id when it is a UUIDv7.
func RequestEventID(mid billing.MerchantID, requestID string) uuid.UUID {
	parent, err := uuid.Parse(requestID)
	if err != nil {
		parent = uuid.Nil
	}
	return uuidutil.DerivedV7(uuidutil.TimeOf(parent), usageNamespace, mid.String()+"/request/"+requestID)
}

// Literals of values the store formats itself: no caller text reaches a
// query.
func uuidLiteral(id uuid.UUID) string { return "toUUID('" + id.String() + "')" }

func timeLiteral(t time.Time) string {
	return "toDateTime64('" + t.UTC().Format("2006-01-02 15:04:05.000000") + "', 6, 'UTC')"
}
