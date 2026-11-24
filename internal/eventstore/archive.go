package eventstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
)

// A merchant archive carries its usage in three tables of text values: the
// raw events (FINAL, as long as their TTL keeps them), then the usage
// history and limit usage the expired events no longer rebuild. A restore
// inserts the events, whose views rebuild the rollups, then those remainders.

// Archive tables.
const (
	ArchiveEvents  = "usage_events"
	ArchiveHistory = "usage_history"
	ArchiveLimits  = "limit_usage"
)

// ArchiveColumns are each archive table's columns.
var ArchiveColumns = map[string][]string{
	ArchiveEvents: {"event"},
	ArchiveHistory: {"customer_id", "bucket_at", "currency", "event_type", "meter", "cell", "print", "period_starts_at", "invoker", "outcome",
		"requests", "quantity", "stated", "charged", "forgiven"},
	ArchiveLimits: {"customer_id", "limit_key", "window_starts_at", "window_ends_at", "units"},
}

// archiveTime is PostgreSQL's text form of a UTC instant, as the archive
// carries every instant.
const archiveTime = "2006-01-02 15:04:05.999999-07"

type bucketKey struct {
	customer                                billing.CustomerID
	bucket                                  time.Time
	currency, eventType, meter, cell, print string
	period                                  time.Time
	invoker                                 string
	outcome                                 UsageOutcome
}

type bucketTotal struct{ requests, quantity, stated, charged, forgiven int64 }

type limitKey struct {
	customer billing.CustomerID
	key      string
	start    time.Time
}

type limitTotal struct {
	ends  *time.Time
	units int64
}

// ExportUsage streams the context's merchant's usage to emit, table by
// table.
func (s *Store) ExportUsage(ctx context.Context, emit func(table string, values []*string) error) error {
	mid, err := s.merchant(ctx)
	if err != nil {
		return err
	}
	buckets := map[bucketKey]bucketTotal{}
	limits := map[limitKey]limitTotal{}
	rows, err := s.conn.Query(ctx, s.resolve(`SELECT event_id, customer_id, currency, event_type, invoker, invoker_type, toString(outcome), source, source_id,
       lease_id, n, occurred_at, admitted_at, ingested_at, period_starts_at, dimensions, metadata, amount, stated, charged, forgiven,
       charges.meter, charges.cell, charges.print, charges.quantity, charges.stated,
       limits.key, limits.window_starts_at, limits.window_ends_at, limits.units
FROM {database}.usage_events FINAL WHERE merchant_id = `+mid+` ORDER BY customer_id, occurred_at, event_id`))
	if err != nil {
		return fail("export usage", err)
	}
	for rows.Next() {
		var e UsageEvent
		var customer uuid.UUID
		var outcome string
		var meter, cell, print, keys []string
		var quantity, stated, units []int64
		var starts []time.Time
		var ends []*time.Time
		if err := rows.Scan(&e.EventID, &customer, &e.Currency, &e.EventType, &e.Invoker, &e.InvokerType, &outcome, &e.Source, &e.SourceID,
			&e.LeaseID, &e.N, &e.OccurredAt, &e.AdmittedAt, &e.IngestedAt, &e.PeriodStartsAt, &e.Dimensions, &e.Metadata, &e.Amount, &e.Stated,
			&e.Charged, &e.Forgiven, &meter, &cell, &print, &quantity, &stated, &keys, &starts, &ends, &units); err != nil {
			rows.Close()
			return fail("export usage", err)
		}
		e.CustomerID, e.Outcome = billing.CustomerID(customer), UsageOutcome(outcome)
		for i := range meter {
			e.Charges = append(e.Charges, UsageCharge{Meter: meter[i], Cell: cell[i], Print: print[i], Quantity: quantity[i], Stated: stated[i]})
		}
		for i := range keys {
			e.Limits = append(e.Limits, UsageLimit{Key: keys[i], WindowStartsAt: starts[i].UTC(), WindowEndsAt: ends[i], Units: units[i]})
		}
		rebuilt(e, buckets, limits)
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
	history, err := s.UsageHistory(ctx, HistoryFilter{From: time.Unix(0, 0).UTC(), To: time.Now().UTC().AddDate(1, 0, 0)})
	if err != nil {
		return err
	}
	for _, d := range history {
		k := bucketKey{d.CustomerID, d.BucketAt, d.Currency, d.EventType, d.Meter, d.Cell, d.Print, d.PeriodStartsAt, d.Invoker, d.Outcome}
		have := buckets[k]
		left := bucketTotal{d.Requests - have.requests, d.Quantity - have.quantity, d.Stated - have.stated, d.Charged - have.charged, d.Forgiven - have.forgiven}
		if left == (bucketTotal{}) {
			continue
		}
		values := []string{d.CustomerID.String(), d.BucketAt.UTC().Format(archiveTime), d.Currency, d.EventType, d.Meter, d.Cell, d.Print,
			d.PeriodStartsAt.UTC().Format(archiveTime), d.Invoker, string(d.Outcome), strconv.FormatInt(left.requests, 10),
			strconv.FormatInt(left.quantity, 10), strconv.FormatInt(left.stated, 10), strconv.FormatInt(left.charged, 10), strconv.FormatInt(left.forgiven, 10)}
		if err := emit(ArchiveHistory, texts(values)); err != nil {
			return err
		}
	}
	var stored []struct {
		key   limitKey
		total limitTotal
	}
	err = s.query(ctx, "export limit usage", `SELECT customer_id, limit_key, window_starts_at, any(window_ends_at), sum(units) FROM {database}.limit_usage
WHERE merchant_id = `+mid+` GROUP BY customer_id, limit_key, window_starts_at ORDER BY customer_id, limit_key, window_starts_at`,
		func(rows interface{ Scan(...any) error }) error {
			var customer uuid.UUID
			var k limitKey
			var t limitTotal
			if err := rows.Scan(&customer, &k.key, &k.start, &t.ends, &t.units); err != nil {
				return err
			}
			k.customer, k.start = billing.CustomerID(customer), k.start.UTC()
			stored = append(stored, struct {
				key   limitKey
				total limitTotal
			}{k, t})
			return nil
		})
	if err != nil {
		return err
	}
	for _, r := range stored {
		left := r.total.units - limits[r.key].units
		if left == 0 {
			continue
		}
		var ends *string
		if r.total.ends != nil {
			v := r.total.ends.UTC().Format(archiveTime)
			ends = &v
		}
		start, units := r.key.start.Format(archiveTime), strconv.FormatInt(left, 10)
		customer, key := r.key.customer.String(), r.key.key
		if err := emit(ArchiveLimits, []*string{&customer, &key, &start, ends, &units}); err != nil {
			return err
		}
	}
	return nil
}

// rebuilt adds what an event's views write to the rollups.
func rebuilt(e UsageEvent, buckets map[bucketKey]bucketTotal, limits map[limitKey]limitTotal) {
	base := bucketKey{e.CustomerID, e.OccurredAt.UTC().Truncate(HistoryBucket), e.Currency, e.EventType, "", "", "", e.PeriodStartsAt.UTC(), e.Invoker, e.Outcome}
	t := buckets[base]
	t.requests++
	if e.Stated != nil {
		t.stated += *e.Stated
	}
	t.charged += e.Charged
	t.forgiven += e.Forgiven
	buckets[base] = t
	for _, c := range e.Charges {
		k := base
		k.meter, k.cell, k.print = c.Meter, c.Cell, c.Print
		t := buckets[k]
		t.requests++
		t.quantity += c.Quantity
		t.stated += c.Stated
		buckets[k] = t
	}
	for _, l := range e.Limits {
		k := limitKey{e.CustomerID, l.Key, l.WindowStartsAt.UTC()}
		t := limits[k]
		t.units += l.Units
		limits[k] = t
	}
}

func texts(values []string) []*string {
	out := make([]*string, len(values))
	for i := range values {
		out[i] = &values[i]
	}
	return out
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
	switch table {
	case ArchiveEvents:
		events := make([]UsageEvent, 0, len(rows))
		for _, r := range rows {
			var e UsageEvent
			if len(r) != 1 || r[0] == nil || json.Unmarshal([]byte(*r[0]), &e) != nil {
				return fmt.Errorf("eventstore: invalid archived usage event")
			}
			events = append(events, e)
		}
		return s.InsertUsage(ctx, events)
	case ArchiveHistory:
		batch, err := s.conn.PrepareBatch(ctx, s.resolve(`INSERT INTO {database}.usage_history (merchant_id, customer_id, bucket_at, currency, event_type, meter, cell,
    print, period_starts_at, invoker, outcome, requests, quantity, stated, charged, forgiven)`))
		if err != nil {
			return fail("restore usage history", err)
		}
		defer func() { _ = batch.Abort() }()
		for _, r := range rows {
			v, err := archived(r, 15)
			if err != nil {
				return err
			}
			customer, err := uuid.Parse(v[0])
			if err != nil {
				return err
			}
			bucket, err := time.Parse(archiveTime, v[1])
			if err != nil {
				return err
			}
			period, err := time.Parse(archiveTime, v[7])
			if err != nil {
				return err
			}
			n := make([]int64, 5)
			for i := range n {
				if n[i], err = strconv.ParseInt(v[10+i], 10, 64); err != nil {
					return err
				}
			}
			if err := batch.Append(merchant, customer, bucket.UTC(), v[2], v[3], v[4], v[5], v[6], period.UTC(), v[8], v[9], uint64(max(n[0], 0)), n[1], n[2], n[3], n[4]); err != nil {
				return fail("restore usage history", err)
			}
		}
		return fail("restore usage history", batch.Send())
	case ArchiveLimits:
		batch, err := s.conn.PrepareBatch(ctx, s.resolve(`INSERT INTO {database}.limit_usage (merchant_id, customer_id, limit_key, window_starts_at, window_ends_at, units)`))
		if err != nil {
			return fail("restore limit usage", err)
		}
		defer func() { _ = batch.Abort() }()
		for _, r := range rows {
			if len(r) != 5 || r[0] == nil || r[1] == nil || r[2] == nil || r[4] == nil {
				return fmt.Errorf("eventstore: invalid archived limit usage")
			}
			customer, err := uuid.Parse(*r[0])
			if err != nil {
				return err
			}
			start, err := time.Parse(archiveTime, *r[2])
			if err != nil {
				return err
			}
			var ends *time.Time
			if r[3] != nil {
				t, err := time.Parse(archiveTime, *r[3])
				if err != nil {
					return err
				}
				ends = &t
			}
			units, err := strconv.ParseInt(*r[4], 10, 64)
			if err != nil {
				return err
			}
			if err := batch.Append(merchant, customer, *r[1], start.UTC(), ends, units); err != nil {
				return fail("restore limit usage", err)
			}
		}
		return fail("restore limit usage", batch.Send())
	}
	return fmt.Errorf("eventstore: no archive table %q", table)
}

func archived(r []*string, width int) ([]string, error) {
	if len(r) != width {
		return nil, fmt.Errorf("eventstore: archived row has %d values, want %d", len(r), width)
	}
	out := make([]string, width)
	for i, v := range r {
		if v == nil {
			return nil, fmt.Errorf("eventstore: archived row lacks a value")
		}
		out[i] = *v
	}
	return out, nil
}
