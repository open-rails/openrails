package eventstore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// ErrUnavailable is a read or write the event store could not serve: what
// needs it answers 503 and is retried.
var ErrUnavailable = apperr.New(http.StatusServiceUnavailable, billing.CodeServiceUnavailable, "the event store is unavailable; retry")

// fail classifies a store error: anything but a ClickHouse exception (a
// refused query) is the store being out of reach.
func fail(what string, err error) error {
	if err == nil {
		return nil
	}
	var ex *clickhouse.Exception
	if errors.As(err, &ex) || errors.Is(err, context.Canceled) {
		return fmt.Errorf("eventstore: %s: %w", what, err)
	}
	return errors.Join(ErrUnavailable, fmt.Errorf("eventstore: %s: %w", what, err))
}

// query runs a read on the context's merchant, scanning each row.
func (s *Store) query(ctx context.Context, what, sql string, scan func(rows interface{ Scan(...any) error }) error) error {
	rows, err := s.conn.Query(ctx, s.resolve(sql))
	if err != nil {
		return fail(what, err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return fail(what, err)
		}
	}
	return fail(what, rows.Err())
}

func (s *Store) merchant(ctx context.Context) (string, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	return uuidLiteral(uuid.UUID(mid)), nil
}

// LimitRow is what usage took of one limit window, or of one bucket of a
// rolling or rate limit.
type LimitRow struct {
	CustomerID     billing.CustomerID
	Key            string
	WindowStartsAt time.Time
	Units          int64
}

// LimitUsage are the customers' limit rows of windows that end after since or
// never: every current window, and the buckets back to since.
func (s *Store) LimitUsage(ctx context.Context, customers []billing.CustomerID, since time.Time) ([]LimitRow, error) {
	if len(customers) == 0 {
		return nil, nil
	}
	mid, err := s.merchant(ctx)
	if err != nil {
		return nil, err
	}
	who := make([]string, 0, len(customers))
	for _, c := range customers {
		who = append(who, uuidLiteral(uuid.UUID(c)))
	}
	slices.Sort(who)
	who = slices.Compact(who)
	sql := fmt.Sprintf(`SELECT customer_id, limit_key, window_starts_at, sum(units) FROM {database}.limit_usage
WHERE merchant_id = %s AND customer_id IN (%s) AND (window_ends_at IS NULL OR window_ends_at > %s)
GROUP BY customer_id, limit_key, window_starts_at`, mid, strings.Join(who, ", "), timeLiteral(since))
	return s.limitRows(ctx, sql)
}

// LimitUnits are one customer's rows of one limit whose windows start within
// [from, to].
func (s *Store) LimitUnits(ctx context.Context, customer billing.CustomerID, key string, from, to time.Time) ([]LimitRow, error) {
	mid, err := s.merchant(ctx)
	if err != nil {
		return nil, err
	}
	sql := fmt.Sprintf(`SELECT customer_id, limit_key, window_starts_at, sum(units) FROM {database}.limit_usage
WHERE merchant_id = %s AND customer_id = %s AND limit_key = %s AND window_starts_at BETWEEN %s AND %s
GROUP BY customer_id, limit_key, window_starts_at`, mid, uuidLiteral(uuid.UUID(customer)), stringLiteral(key), timeLiteral(from), timeLiteral(to))
	return s.limitRows(ctx, sql)
}

func (s *Store) limitRows(ctx context.Context, sql string) ([]LimitRow, error) {
	var out []LimitRow
	err := s.query(ctx, "read limit usage", sql, func(rows interface{ Scan(...any) error }) error {
		var r LimitRow
		var customer uuid.UUID
		if err := rows.Scan(&customer, &r.Key, &r.WindowStartsAt, &r.Units); err != nil {
			return err
		}
		r.CustomerID, r.WindowStartsAt = billing.CustomerID(customer), r.WindowStartsAt.UTC()
		out = append(out, r)
		return nil
	})
	slices.SortFunc(out, func(a, b LimitRow) int {
		if c := strings.Compare(a.Key, b.Key); c != 0 {
			return c
		}
		return a.WindowStartsAt.Compare(b.WindowStartsAt)
	})
	return out, err
}

// PeriodQuantity is a customer's quantity of one meter cell under one card
// print in a pricing period.
type PeriodQuantity struct {
	CustomerID         billing.CustomerID
	Currency           string
	PeriodStartsAt     time.Time
	Meter, Cell, Print string
	Quantity, Stated   int64
	Requests           int64
}

// PeriodQuantities are customers' quantities of the pricing periods from one
// start on, from the rollup: what a balance and an item's price position
// read. A currency narrows them; "" reads every one.
func (s *Store) PeriodQuantities(ctx context.Context, customers []billing.CustomerID, currency string, from time.Time) ([]PeriodQuantity, error) {
	if len(customers) == 0 {
		return nil, nil
	}
	mid, err := s.merchant(ctx)
	if err != nil {
		return nil, err
	}
	who := make([]string, 0, len(customers))
	for _, c := range customers {
		who = append(who, uuidLiteral(uuid.UUID(c)))
	}
	slices.Sort(who)
	who = slices.Compact(who)
	narrow := ""
	if currency != "" {
		narrow = " AND currency = " + stringLiteral(currency)
	}
	sql := fmt.Sprintf(`SELECT customer_id, currency, period_starts_at, meter, cell, print, sum(quantity), sum(stated), toInt64(sum(requests))
FROM {database}.usage_quantities
WHERE merchant_id = %s AND customer_id IN (%s)%s AND period_starts_at >= %s
GROUP BY customer_id, currency, period_starts_at, meter, cell, print`, mid, strings.Join(who, ", "), narrow, timeLiteral(from))
	return s.quantities(ctx, sql)
}

// Quantities are usage's quantity, stated amounts and requests.
type Quantities struct {
	Quantity, Stated, Requests int64
}

// PostingQuantity is what a posting reads of a customer's usage of one meter
// cell under one card print in a pricing period, before its cut: Read is the
// usage an earlier posting may have taken (it occurred before that posting's
// cut and was ingested by its watermark), Fresh the rest. Later counts the
// usage at or after the cut, and IngestedThrough is the latest ingestion
// before it.
type PostingQuantity struct {
	PeriodStartsAt     time.Time
	Meter, Cell, Print string
	Read, Fresh        Quantities
	Later              int64
	IngestedThrough    time.Time
}

// PostingWatermark is how far earlier postings read a customer's usage: what
// occurred before Cut and was ingested by Ingested. Zero is no posting.
type PostingWatermark struct {
	Cut, Ingested time.Time
}

// PostingQuantities are a customer's deduplicated quantities in a currency of
// the pricing periods from one start on, split at an earlier posting's
// watermark: what a posting cut at cut takes.
func (s *Store) PostingQuantities(ctx context.Context, customer billing.CustomerID, currency string, from, cut time.Time, prior PostingWatermark) ([]PostingQuantity, error) {
	mid, err := s.merchant(ctx)
	if err != nil {
		return nil, err
	}
	before := "occurred_at < " + timeLiteral(cut)
	read := "0"
	if !prior.Cut.IsZero() && !prior.Ingested.IsZero() {
		read = "occurred_at < " + timeLiteral(prior.Cut) + " AND ingested_at <= " + timeLiteral(prior.Ingested)
	}
	sql := fmt.Sprintf(`SELECT period_starts_at, charges.meter, charges.cell, charges.print,
       sumIf(charges.quantity, %[1]s AND %[2]s), sumIf(charges.stated, %[1]s AND %[2]s), toInt64(countIf(%[1]s AND %[2]s)),
       sumIf(charges.quantity, %[1]s AND NOT (%[2]s)), sumIf(charges.stated, %[1]s AND NOT (%[2]s)), toInt64(countIf(%[1]s AND NOT (%[2]s))),
       toInt64(countIf(NOT (%[1]s))), maxIf(ingested_at, %[1]s)
FROM {database}.usage_events FINAL ARRAY JOIN charges
WHERE merchant_id = %[3]s AND customer_id = %[4]s AND currency = %[5]s AND occurred_at >= %[6]s AND period_starts_at >= %[6]s
GROUP BY period_starts_at, charges.meter, charges.cell, charges.print`, before, read, mid, uuidLiteral(uuid.UUID(customer)), stringLiteral(currency), timeLiteral(from))
	var out []PostingQuantity
	err = s.query(ctx, "read usage quantities", sql, func(rows interface{ Scan(...any) error }) error {
		var q PostingQuantity
		if err := rows.Scan(&q.PeriodStartsAt, &q.Meter, &q.Cell, &q.Print, &q.Read.Quantity, &q.Read.Stated, &q.Read.Requests,
			&q.Fresh.Quantity, &q.Fresh.Stated, &q.Fresh.Requests, &q.Later, &q.IngestedThrough); err != nil {
			return err
		}
		q.PeriodStartsAt, q.IngestedThrough = q.PeriodStartsAt.UTC(), q.IngestedThrough.UTC()
		out = append(out, q)
		return nil
	})
	return out, err
}

// PrintSource is a card print's usage: an event type it priced, and when its
// first event was ingested.
type PrintSource struct {
	EventType  string
	IngestedAt time.Time
}

// PrintSources are where a customer's usage in a currency from one period
// start on names prints.
func (s *Store) PrintSources(ctx context.Context, customer billing.CustomerID, currency string, from time.Time, prints []string) (map[string]PrintSource, error) {
	out := map[string]PrintSource{}
	if len(prints) == 0 {
		return out, nil
	}
	mid, err := s.merchant(ctx)
	if err != nil {
		return nil, err
	}
	named := make([]string, 0, len(prints))
	for _, p := range prints {
		named = append(named, stringLiteral(p))
	}
	sql := fmt.Sprintf(`SELECT charges.print, any(event_type), min(ingested_at)
FROM {database}.usage_events ARRAY JOIN charges
WHERE merchant_id = %s AND customer_id = %s AND currency = %s AND occurred_at >= %s AND period_starts_at >= %s AND charges.print IN (%s)
GROUP BY charges.print`, mid, uuidLiteral(uuid.UUID(customer)), stringLiteral(currency), timeLiteral(from), timeLiteral(from), strings.Join(named, ", "))
	err = s.query(ctx, "read usage prints", sql, func(rows interface{ Scan(...any) error }) error {
		var print string
		var src PrintSource
		if err := rows.Scan(&print, &src.EventType, &src.IngestedAt); err != nil {
			return err
		}
		src.IngestedAt = src.IngestedAt.UTC()
		out[print] = src
		return nil
	})
	return out, err
}

func (s *Store) quantities(ctx context.Context, sql string) ([]PeriodQuantity, error) {
	var out []PeriodQuantity
	err := s.query(ctx, "read usage quantities", sql, func(rows interface{ Scan(...any) error }) error {
		var q PeriodQuantity
		var customer uuid.UUID
		if err := rows.Scan(&customer, &q.Currency, &q.PeriodStartsAt, &q.Meter, &q.Cell, &q.Print, &q.Quantity, &q.Stated, &q.Requests); err != nil {
			return err
		}
		q.CustomerID, q.PeriodStartsAt = billing.CustomerID(customer), q.PeriodStartsAt.UTC()
		out = append(out, q)
		return nil
	})
	return out, err
}

// MinuteAmount is what a customer's usage charged in a currency, at its
// provisional prices, in the minutes from since's up to until's (no bound
// when zero).
func (s *Store) MinuteAmount(ctx context.Context, customer billing.CustomerID, currency string, since, until time.Time) (int64, error) {
	mid, err := s.merchant(ctx)
	if err != nil {
		return 0, err
	}
	bound := ""
	if !until.IsZero() {
		bound = " AND minute < toStartOfMinute(" + timeLiteral(until) + ")"
	}
	sql := fmt.Sprintf(`SELECT sum(amount) FROM {database}.usage_minutes
WHERE merchant_id = %s AND customer_id = %s AND currency = %s AND minute >= toStartOfMinute(%s)%s`,
		mid, uuidLiteral(uuid.UUID(customer)), stringLiteral(currency), timeLiteral(since), bound)
	var total int64
	err = s.query(ctx, "read usage minutes", sql, func(rows interface{ Scan(...any) error }) error { return rows.Scan(&total) })
	return total, err
}

// MinuteAmounts are MinuteAmount for many customers since one instant, with
// no upper bound, in one read; a customer with none is absent.
func (s *Store) MinuteAmounts(ctx context.Context, customers []billing.CustomerID, currency string, since time.Time) (map[billing.CustomerID]int64, error) {
	out := map[billing.CustomerID]int64{}
	if len(customers) == 0 {
		return out, nil
	}
	mid, err := s.merchant(ctx)
	if err != nil {
		return nil, err
	}
	sql := fmt.Sprintf(`SELECT customer_id, sum(amount) FROM {database}.usage_minutes
WHERE merchant_id = %s AND customer_id IN (%s) AND currency = %s AND minute >= toStartOfMinute(%s)
GROUP BY customer_id`, mid, customerList(customers), stringLiteral(currency), timeLiteral(since))
	err = s.query(ctx, "read usage minutes", sql, func(rows interface{ Scan(...any) error }) error {
		var customer uuid.UUID
		var total int64
		if err := rows.Scan(&customer, &total); err != nil {
			return err
		}
		out[billing.CustomerID(customer)] = total
		return nil
	})
	return out, err
}

// customerList is customers as a deduplicated IN list.
func customerList(customers []billing.CustomerID) string {
	who := make([]string, 0, len(customers))
	for _, c := range customers {
		who = append(who, uuidLiteral(uuid.UUID(c)))
	}
	slices.Sort(who)
	return strings.Join(slices.Compact(who), ", ")
}

// HistoryBucket is the length of a usage history bucket: every zone's
// offset is a multiple of it, so buckets fold into any zone's days.
const HistoryBucket = 15 * time.Minute

// UsageBucket is one row of usage history: a 15-minute UTC bucket's events of
// one type, invoker and outcome (Meter ""), or what they took of one meter
// cell under one card print in one pricing period.
type UsageBucket struct {
	CustomerID         billing.CustomerID
	BucketAt           time.Time
	Currency           string
	EventType          string
	Meter, Cell, Print string
	PeriodStartsAt     time.Time
	Invoker            string
	Outcome            UsageOutcome
	Requests           int64
	Quantity, Stated   int64
	Charged, Forgiven  int64
}

// HistoryFilter selects usage history: one customer (all when zero), one
// currency (all when empty), the buckets that start in [From, To), and with
// PeriodsFrom only charge rows of periods that start then or later.
type HistoryFilter struct {
	CustomerID  billing.CustomerID
	Currency    string
	From, To    time.Time
	PeriodsFrom *time.Time
}

func (f HistoryFilter) where(mid string) string {
	w := []string{"merchant_id = " + mid, "bucket_at >= " + timeLiteral(f.From), "bucket_at < " + timeLiteral(f.To)}
	if !f.CustomerID.IsZero() {
		w = append(w, "customer_id = "+uuidLiteral(uuid.UUID(f.CustomerID)))
	}
	if f.Currency != "" {
		w = append(w, "currency = "+stringLiteral(f.Currency))
	}
	if f.PeriodsFrom != nil {
		w = append(w, "meter != ''", "period_starts_at >= "+timeLiteral(*f.PeriodsFrom))
	}
	return strings.Join(w, " AND ")
}

// UsageHistory are the history rows a filter selects, summed by key.
func (s *Store) UsageHistory(ctx context.Context, f HistoryFilter) ([]UsageBucket, error) {
	mid, err := s.merchant(ctx)
	if err != nil {
		return nil, err
	}
	sql := `SELECT customer_id, bucket_at, currency, event_type, meter, cell, print, period_starts_at, invoker, toString(outcome),
       toInt64(sum(requests)), sum(quantity), sum(stated), sum(charged), sum(forgiven)
FROM {database}.usage_history WHERE ` + f.where(mid) + `
GROUP BY customer_id, bucket_at, currency, event_type, meter, cell, print, period_starts_at, invoker, outcome`
	var out []UsageBucket
	err = s.query(ctx, "read usage history", sql, func(rows interface{ Scan(...any) error }) error {
		var b UsageBucket
		var customer uuid.UUID
		var outcome string
		if err := rows.Scan(&customer, &b.BucketAt, &b.Currency, &b.EventType, &b.Meter, &b.Cell, &b.Print, &b.PeriodStartsAt, &b.Invoker, &outcome,
			&b.Requests, &b.Quantity, &b.Stated, &b.Charged, &b.Forgiven); err != nil {
			return err
		}
		b.CustomerID, b.Outcome = billing.CustomerID(customer), UsageOutcome(outcome)
		b.BucketAt, b.PeriodStartsAt = b.BucketAt.UTC(), b.PeriodStartsAt.UTC()
		out = append(out, b)
		return nil
	})
	return out, err
}

// UsageExists reports whether the merchant has usage of any of eventTypes:
// a meter that read it may not change what it means.
func (s *Store) UsageExists(ctx context.Context, eventTypes []string) (bool, error) {
	mid, err := s.merchant(ctx)
	if err != nil {
		return false, err
	}
	types := make([]string, 0, len(eventTypes))
	for _, t := range eventTypes {
		types = append(types, stringLiteral(t))
	}
	if len(types) == 0 {
		return false, nil
	}
	sql := fmt.Sprintf(`SELECT count() > 0 FROM (SELECT 1 FROM {database}.usage_history WHERE merchant_id = %s AND event_type IN (%s) LIMIT 1)`,
		mid, strings.Join(types, ", "))
	var found bool
	err = s.query(ctx, "read usage activity", sql, func(rows interface{ Scan(...any) error }) error { return rows.Scan(&found) })
	return found, err
}

// stringLiteral quotes text for a query.
func stringLiteral(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// LastUsage is the start of the bucket of each event type's latest usage.
func (s *Store) LastUsage(ctx context.Context, eventTypes []string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	if len(eventTypes) == 0 {
		return out, nil
	}
	mid, err := s.merchant(ctx)
	if err != nil {
		return nil, err
	}
	types := make([]string, 0, len(eventTypes))
	for _, t := range eventTypes {
		types = append(types, stringLiteral(t))
	}
	sql := fmt.Sprintf(`SELECT event_type, max(bucket_at) FROM {database}.usage_history WHERE merchant_id = %s AND event_type IN (%s) GROUP BY event_type`,
		mid, strings.Join(types, ", "))
	err = s.query(ctx, "read usage activity", sql, func(rows interface{ Scan(...any) error }) error {
		var t string
		var at time.Time
		if err := rows.Scan(&t, &at); err != nil {
			return err
		}
		out[t] = at.UTC()
		return nil
	})
	return out, err
}

// LimitUsed reports whether usage of the merchant has counted toward a limit.
func (s *Store) LimitUsed(ctx context.Context, key string) (bool, error) {
	mid, err := s.merchant(ctx)
	if err != nil {
		return false, err
	}
	var used bool
	err = s.query(ctx, "read limit usage", "SELECT count() > 0 FROM (SELECT 1 FROM {database}.limit_usage WHERE merchant_id = "+mid+
		" AND limit_key = "+stringLiteral(key)+" LIMIT 1)", func(rows interface{ Scan(...any) error }) error { return rows.Scan(&used) })
	return used, err
}
