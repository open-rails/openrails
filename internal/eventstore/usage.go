package eventstore

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
)

// UsageOutcome is usage_events.outcome.
type UsageOutcome string

const (
	UsageSucceeded UsageOutcome = "succeeded"
	UsageFailed    UsageOutcome = "failed"
)

// UsageEvent is one settled usage item; its merchant is the context's.
type UsageEvent struct {
	// EventID is the intake identity: a UUIDv5 of (merchant, lease, n), or of
	// the request id.
	EventID     uuid.UUID          `json:"event_id"`
	CustomerID  billing.CustomerID `json:"customer_id"`
	Currency    string             `json:"currency"`
	EventType   string             `json:"event_type"`
	Invoker     string             `json:"invoker"`
	InvokerType string             `json:"invoker_type"`
	Outcome     UsageOutcome       `json:"outcome"`
	Source      string             `json:"source"`
	SourceID    string             `json:"source_id"`
	// LeaseID and N number a synced item; zero for a direct capture.
	LeaseID        uuid.UUID        `json:"lease_id"`
	N              uint64           `json:"n"`
	OccurredAt     time.Time        `json:"occurred_at"`
	AdmittedAt     time.Time        `json:"admitted_at"`
	IngestedAt     time.Time        `json:"ingested_at"`
	PeriodStartsAt time.Time        `json:"period_starts_at"`
	Dimensions     map[string]int64 `json:"dimensions"`
	Metadata       string           `json:"metadata"`
	// Amount is the provisional price limits and the accrual-rate cap count.
	Amount int64 `json:"amount"`
	// Stated is a host-stated amount; Charged and Forgiven split failed usage.
	Stated   *int64        `json:"stated"`
	Charged  int64         `json:"charged"`
	Forgiven int64         `json:"forgiven"`
	Charges  []UsageCharge `json:"charges"`
	Limits   []UsageLimit  `json:"limits"`
}

// UsageCharge is a priced meter cell of an item, under the card print in force.
type UsageCharge struct {
	Meter    string `json:"meter"`
	Cell     string `json:"cell"`
	Print    string `json:"print"`
	Quantity int64  `json:"quantity"`
	Stated   int64  `json:"stated"`
}

// UsageLimit is a limit window that counts an item, located at admission.
type UsageLimit struct {
	Key            string     `json:"key"`
	WindowStartsAt time.Time  `json:"window_starts_at"`
	WindowEndsAt   *time.Time `json:"window_ends_at"`
	Units          int64      `json:"units"`
}

const insertUsage = "INSERT INTO {database}.usage_events (merchant_id, event_id, customer_id, currency, event_type, invoker, " +
	"invoker_type, outcome, source, source_id, lease_id, n, occurred_at, admitted_at, ingested_at, period_starts_at, " +
	"dimensions, metadata, amount, stated, charged, forgiven, `charges.meter`, `charges.cell`, `charges.print`, " +
	"`charges.quantity`, `charges.stated`, `limits.key`, `limits.window_starts_at`, `limits.window_ends_at`, `limits.units`)"

// InsertUsage stores events as one asynchronous insert and returns once
// ClickHouse has written them, rollups included: what it acknowledged reads
// back.
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
			e.IngestedAt.UTC(), e.PeriodStartsAt.UTC(), dimensions, e.Metadata, e.Amount, e.Stated, e.Charged, e.Forgiven,
			c.meter, c.cell, c.print, c.quantity, c.stated, l.key, l.startsAt, l.endsAt, l.units); err != nil {
			return fmt.Errorf("eventstore: usage event %s: %w", e.EventID, err)
		}
	}
	return fail("insert usage", batch.Send())
}

type charges struct {
	meter, cell, print []string
	quantity, stated   []int64
}

func chargeColumns(in []UsageCharge) charges {
	out := charges{meter: []string{}, cell: []string{}, print: []string{}, quantity: []int64{}, stated: []int64{}}
	for _, c := range in {
		out.meter, out.cell, out.print = append(out.meter, c.Meter), append(out.cell, c.Cell), append(out.print, c.Print)
		out.quantity, out.stated = append(out.quantity, c.Quantity), append(out.stated, c.Stated)
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

// usageNamespace names usage events' intake identities (UUIDv5).
var usageNamespace = uuid.MustParse("6f1c5a3e-2b8d-5c4e-9a71-0d3e8b2f4c19")

// LeaseEventID is the identity of a lease's usage item n.
func LeaseEventID(mid billing.MerchantID, lease uuid.UUID, n int64) uuid.UUID {
	return uuid.NewSHA1(usageNamespace, []byte(mid.String()+"/lease/"+lease.String()+"/"+strconv.FormatInt(n, 10)))
}

// RequestEventID is the identity of the capture of an admitted request.
func RequestEventID(mid billing.MerchantID, requestID string) uuid.UUID {
	return uuid.NewSHA1(usageNamespace, []byte(mid.String()+"/request/"+requestID))
}

// UsageKey locates a usage event by its sort key.
type UsageKey struct {
	CustomerID billing.CustomerID
	OccurredAt time.Time
	EventID    uuid.UUID
}

// Existing is what the store holds of a usage event already inserted: a
// failed item's charge and forgiveness, which its resend posts again.
type Existing struct {
	Charged, Forgiven int64
}

// ExistingUsage are keys' events the store holds, read through the sort key:
// a resend's items an earlier attempt already inserted.
func (s *Store) ExistingUsage(ctx context.Context, keys []UsageKey) (map[uuid.UUID]Existing, error) {
	found := map[uuid.UUID]Existing{}
	if len(keys) == 0 {
		return found, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	customers := map[billing.CustomerID]bool{}
	ids := make([]string, 0, len(keys))
	from, to := keys[0].OccurredAt, keys[0].OccurredAt
	for _, k := range keys {
		customers[k.CustomerID] = true
		ids = append(ids, uuidLiteral(k.EventID))
		if k.OccurredAt.Before(from) {
			from = k.OccurredAt
		}
		if k.OccurredAt.After(to) {
			to = k.OccurredAt
		}
	}
	who := make([]string, 0, len(customers))
	for c := range customers {
		who = append(who, uuidLiteral(uuid.UUID(c)))
	}
	query := fmt.Sprintf("SELECT event_id, any(charged), any(forgiven) FROM {database}.usage_events WHERE merchant_id = %s AND customer_id IN (%s) "+
		"AND occurred_at BETWEEN %s AND %s AND event_id IN (%s) GROUP BY event_id",
		uuidLiteral(uuid.UUID(mid)), strings.Join(who, ", "), timeLiteral(from), timeLiteral(to), strings.Join(ids, ", "))
	err = s.query(ctx, "read usage ids", query, func(rows interface{ Scan(...any) error }) error {
		var id uuid.UUID
		var e Existing
		if err := rows.Scan(&id, &e.Charged, &e.Forgiven); err != nil {
			return err
		}
		found[id] = e
		return nil
	})
	return found, err
}

// Literals of values the store formats itself: no caller text reaches a
// query.
func uuidLiteral(id uuid.UUID) string { return "toUUID('" + id.String() + "')" }

func timeLiteral(t time.Time) string {
	return "toDateTime64('" + t.UTC().Format("2006-01-02 15:04:05.000000") + "', 6, 'UTC')"
}
