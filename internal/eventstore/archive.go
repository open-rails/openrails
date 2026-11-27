package eventstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/merchant"
)

// A merchant archive carries its usage as the event store holds it, one
// archive table per store table: the raw events (FINAL, as long as their TTL
// keeps them) and every rollup's rows. Nothing is derived again: a restore
// inserts each table's rows into that table, the events straight into
// usage_events.

// Archive tables, in archive order.
const (
	ArchiveEvents  = "usage_events"
	ArchiveHistory = "usage_history"
	ArchiveLimits  = "limit_usage"
)

// archiveTime is PostgreSQL's text form of a UTC instant, as the archive
// carries every instant.
const archiveTime = "2006-01-02 15:04:05.999999-07"

// A column's kind: how it is scanned from the store, written to the archive
// and appended back.
type kind int

const (
	kindUUID kind = iota
	kindTime
	kindNullTime
	kindText
	kindInt
)

// rollup is an archived rollup table: its columns after merchant_id and the
// select that reads one merchant's rows of them, summed by key.
type rollup struct {
	table   string
	columns []string
	kinds   []kind
	query   string
}

var rollups = []rollup{
	{ArchiveHistory, []string{"customer_id", "bucket_at", "currency", "event_type", "meter", "cell",
		"requests", "quantity", "stated", "amount", "discount", "failed_requests", "failed_amount", "forgiven"},
		[]kind{kindUUID, kindTime, kindText, kindText, kindText, kindText, kindInt, kindInt, kindInt, kindInt, kindInt, kindInt, kindInt, kindInt},
		`SELECT customer_id, bucket_at, currency, event_type, meter, cell, sum(requests) AS r, sum(quantity) AS q, sum(stated) AS st,
       sum(amount) AS a, sum(discount) AS d, sum(failed_requests) AS fr, sum(failed_amount) AS fa, sum(forgiven) AS f
FROM {database}.usage_history WHERE merchant_id = %s
GROUP BY customer_id, bucket_at, currency, event_type, meter, cell
HAVING r != 0 OR q != 0 OR st != 0 OR a != 0 OR d != 0 OR fr != 0 OR fa != 0 OR f != 0
ORDER BY customer_id, bucket_at, currency, event_type, meter, cell`},
	{ArchiveLimits, []string{"customer_id", "limit_key", "window_starts_at", "window_ends_at", "units"},
		[]kind{kindUUID, kindText, kindTime, kindNullTime, kindInt},
		`SELECT customer_id, limit_key, window_starts_at, any(window_ends_at), sum(units) AS u
FROM {database}.limit_usage WHERE merchant_id = %s
GROUP BY customer_id, limit_key, window_starts_at HAVING u != 0 ORDER BY customer_id, limit_key, window_starts_at`},
}

// archivedEvent is a usage_events row as the archive carries it.
type archivedEvent struct {
	EventID        uuid.UUID        `json:"event_id"`
	CustomerID     uuid.UUID        `json:"customer_id"`
	Currency       string           `json:"currency"`
	EventType      string           `json:"event_type"`
	Invoker        string           `json:"invoker"`
	InvokerType    string           `json:"invoker_type"`
	Outcome        string           `json:"outcome"`
	Source         string           `json:"source"`
	SourceID       string           `json:"source_id"`
	LeaseID        uuid.UUID        `json:"lease_id"`
	N              uint64           `json:"n"`
	OccurredAt     time.Time        `json:"occurred_at"`
	AdmittedAt     time.Time        `json:"admitted_at"`
	IngestedAt     time.Time        `json:"ingested_at"`
	PeriodStartsAt time.Time        `json:"period_starts_at"`
	Dimensions     map[string]int64 `json:"dimensions"`
	Metadata       string           `json:"metadata"`
	Stated         *int64           `json:"stated"`
	Charged        int64            `json:"charged"`
	Forgiven       int64            `json:"forgiven"`
	Charges        []UsageCharge    `json:"charges"`
}

const eventColumns = "event_id, customer_id, currency, event_type, invoker, invoker_type, outcome, source, source_id, lease_id, n, occurred_at, admitted_at, " +
	"ingested_at, period_starts_at, dimensions, metadata, stated, charged, forgiven, `charges.meter`, `charges.cell`, `charges.print`, `charges.quantity`, `charges.stated`, " +
	"`charges.amount`, `charges.discount`"

// ExportUsage streams the context's merchant's usage to emit, table by
// table in archive order.
func (s *Store) ExportUsage(ctx context.Context, emit func(table string, values []*string) error) error {
	mid, err := s.merchant(ctx)
	if err != nil {
		return err
	}
	rows, err := s.conn.Query(ctx, s.resolve(`SELECT event_id, customer_id, currency, event_type, invoker, invoker_type, toString(outcome), source, source_id,
       lease_id, n, occurred_at, admitted_at, ingested_at, period_starts_at, dimensions, metadata, stated, charged, forgiven,
       charges.meter, charges.cell, charges.print, charges.quantity, charges.stated, charges.amount, charges.discount
FROM {database}.usage_events FINAL WHERE merchant_id = `+mid+` ORDER BY customer_id, occurred_at, event_id`))
	if err != nil {
		return fail("export usage", err)
	}
	for rows.Next() {
		var e archivedEvent
		var meter, cell, print []string
		var quantity, stated, amount, discount []int64
		if err := rows.Scan(&e.EventID, &e.CustomerID, &e.Currency, &e.EventType, &e.Invoker, &e.InvokerType, &e.Outcome, &e.Source, &e.SourceID,
			&e.LeaseID, &e.N, &e.OccurredAt, &e.AdmittedAt, &e.IngestedAt, &e.PeriodStartsAt, &e.Dimensions, &e.Metadata, &e.Stated,
			&e.Charged, &e.Forgiven, &meter, &cell, &print, &quantity, &stated, &amount, &discount); err != nil {
			rows.Close()
			return fail("export usage", err)
		}
		for i := range meter {
			e.Charges = append(e.Charges, UsageCharge{Meter: meter[i], Cell: cell[i], Print: print[i], Quantity: quantity[i], Stated: stated[i],
				Amount: amount[i], Discount: discount[i]})
		}
		raw, err := json.Marshal(e)
		if err != nil {
			rows.Close()
			return err
		}
		text := string(raw)
		if err := emit(ArchiveEvents, []*string{&text}); err != nil {
			rows.Close()
			return err
		}
	}
	rows.Close()
	if err := fail("export usage", rows.Err()); err != nil {
		return err
	}
	for _, r := range rollups {
		if err := s.exportRollup(ctx, mid, r, emit); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) exportRollup(ctx context.Context, mid string, r rollup, emit func(table string, values []*string) error) error {
	rows, err := s.conn.Query(ctx, s.resolve(fmt.Sprintf(r.query, mid)))
	if err != nil {
		return fail("export "+r.table, err)
	}
	defer rows.Close()
	for rows.Next() {
		dest := make([]any, len(r.kinds))
		for i, k := range r.kinds {
			switch k {
			case kindUUID:
				dest[i] = new(uuid.UUID)
			case kindTime:
				dest[i] = new(time.Time)
			case kindNullTime:
				dest[i] = new(*time.Time)
			case kindText:
				dest[i] = new(string)
			case kindInt:
				dest[i] = new(int64)
			}
		}
		if err := rows.Scan(dest...); err != nil {
			return fail("export "+r.table, err)
		}
		values := make([]*string, len(dest))
		for i, d := range dest {
			var v string
			switch d := d.(type) {
			case *uuid.UUID:
				v = d.String()
			case *time.Time:
				v = d.UTC().Format(archiveTime)
			case **time.Time:
				if *d == nil {
					continue
				}
				v = (*d).UTC().Format(archiveTime)
			case *string:
				v = *d
			case *int64:
				v = strconv.FormatInt(*d, 10)
			}
			values[i] = &v
		}
		if err := emit(r.table, values); err != nil {
			return err
		}
	}
	return fail("export "+r.table, rows.Err())
}

// ImportUsage inserts one table's archived rows for the context's merchant.
func (s *Store) ImportUsage(ctx context.Context, table string, rows [][]*string) error {
	if len(rows) == 0 {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	merchant := uuid.UUID(mid)
	if table == ArchiveEvents {
		batch, err := s.conn.PrepareBatch(ctx, s.resolve("INSERT INTO {database}.usage_events (merchant_id, "+eventColumns+")"))
		if err != nil {
			return fail("restore usage", err)
		}
		defer func() { _ = batch.Abort() }()
		for _, r := range rows {
			var e archivedEvent
			if len(r) != 1 || r[0] == nil || json.Unmarshal([]byte(*r[0]), &e) != nil {
				return fmt.Errorf("eventstore: invalid archived usage event")
			}
			c := chargeColumns(e.Charges)
			dimensions := e.Dimensions
			if dimensions == nil {
				dimensions = map[string]int64{}
			}
			if err := batch.Append(merchant, e.EventID, e.CustomerID, e.Currency, e.EventType, e.Invoker, e.InvokerType, e.Outcome, e.Source, e.SourceID,
				e.LeaseID, e.N, e.OccurredAt.UTC(), e.AdmittedAt.UTC(), e.IngestedAt.UTC(), e.PeriodStartsAt.UTC(), dimensions, e.Metadata, e.Stated,
				e.Charged, e.Forgiven, c.meter, c.cell, c.print, c.quantity, c.stated, c.amount, c.discount); err != nil {
				return fail("restore usage", err)
			}
		}
		return fail("restore usage", batch.Send())
	}
	for _, t := range rollups {
		if t.table != table {
			continue
		}
		cols := "merchant_id"
		for _, c := range t.columns {
			cols += ", " + c
		}
		batch, err := s.conn.PrepareBatch(ctx, s.resolve("INSERT INTO {database}."+t.table+" ("+cols+")"))
		if err != nil {
			return fail("restore "+t.table, err)
		}
		defer func() { _ = batch.Abort() }()
		for _, r := range rows {
			values, err := archived(t, r)
			if err != nil {
				return err
			}
			if err := batch.Append(append([]any{merchant}, values...)...); err != nil {
				return fail("restore "+t.table, err)
			}
		}
		return fail("restore "+t.table, batch.Send())
	}
	return fmt.Errorf("eventstore: no archive table %q", table)
}

// archived parses an archived rollup row into the values its insert takes.
func archived(t rollup, r []*string) ([]any, error) {
	if len(r) != len(t.kinds) {
		return nil, fmt.Errorf("eventstore: archived %s row has %d values, want %d", t.table, len(r), len(t.kinds))
	}
	out := make([]any, len(r))
	for i, k := range t.kinds {
		if r[i] == nil {
			if k != kindNullTime {
				return nil, fmt.Errorf("eventstore: archived %s row lacks %s", t.table, t.columns[i])
			}
			out[i] = (*time.Time)(nil)
			continue
		}
		v := *r[i]
		var err error
		switch k {
		case kindUUID:
			out[i], err = uuid.Parse(v)
		case kindTime, kindNullTime:
			var at time.Time
			at, err = time.Parse(archiveTime, v)
			at = at.UTC()
			if k == kindNullTime {
				out[i] = &at
			} else {
				out[i] = at
			}
		case kindText:
			out[i] = v
		case kindInt:
			out[i], err = strconv.ParseInt(v, 10, 64)
		}
		if err != nil {
			return nil, fmt.Errorf("eventstore: archived %s.%s: %w", t.table, t.columns[i], err)
		}
	}
	return out, nil
}
