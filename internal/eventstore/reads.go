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
	"github.com/open-rails/openrails/internal/shared/calendarutil"
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

// HistoryBucket is the length of a usage history bucket: a UTC quarter
// hour. Every zone's offset is a whole number of quarter hours, so its hours
// and days are whole buckets.
const HistoryBucket = 15 * time.Minute

// EventsKept is how long raw usage events are kept (usage_events' TTL):
// what reads by invoker reach back to.
const EventsKept = 90 * 24 * time.Hour

// UsageBucket is one row of usage history: a bucket's events of one type
// (Meter "": Requests, the failed ones' FailedRequests, FailedAmount and
// Forgiven, and Stated), or what they took of one meter cell (Requests,
// Quantity, Stated, and Amount at intake's price less Discount, a volume
// tier's fall). BucketAt is the bucket's start, or read in a zone the first
// instant of the zone's day (calendarutil.DayStart). Read from raw events by
// invoker (UsageEvents), a row is one invoker's events of one type with
// their charges summed in.
type UsageBucket struct {
	CustomerID                   billing.CustomerID
	BucketAt                     time.Time
	Currency, EventType          string
	Meter, Cell                  string
	Invoker                      string
	Requests, Quantity, Stated   int64
	Amount, Discount             int64
	FailedRequests, FailedAmount int64
	Forgiven                     int64
}

// Cost is what a row's usage cost: its charges at intake's price, less the
// volume discount, and what failed usage charged.
func (b UsageBucket) Cost() int64 { return b.Amount - b.Discount + b.FailedAmount }

// HistoryFilter selects usage: one customer (all when zero), one currency
// (all when empty), the buckets that start in [From, To). With a Zone,
// ClickHouse groups the buckets into the zone's dates as they are read, so a
// zone change regroups all history.
type HistoryFilter struct {
	CustomerID billing.CustomerID
	Currency   string
	From, To   time.Time
	Zone       *time.Location
}

func (f HistoryFilter) where(mid, at string) string {
	w := []string{"merchant_id = " + mid, at + " >= " + timeLiteral(f.From), at + " < " + timeLiteral(f.To)}
	if !f.CustomerID.IsZero() {
		w = append(w, "customer_id = "+uuidLiteral(uuid.UUID(f.CustomerID)))
	}
	if f.Currency != "" {
		w = append(w, "currency = "+stringLiteral(f.Currency))
	}
	return strings.Join(w, " AND ")
}

// bucket is the expression a filter groups an instant column by: its quarter
// hour, or with a zone its date there at UTC midnight (placed at its DayStart
// as read, as PostgreSQL resolves a midnight a clock change skips or
// repeats).
func (f HistoryFilter) bucket(at string) string {
	if f.Zone == nil {
		return "toStartOfFifteenMinutes(" + at + ")"
	}
	return "toDateTime(toDate(" + at + ", " + stringLiteral(f.Zone.String()) + "), 'UTC')"
}

// dayStart places a row read by zone at its day's first instant.
func (f HistoryFilter) dayStart(at time.Time) time.Time {
	at = at.UTC()
	if f.Zone == nil {
		return at
	}
	return calendarutil.DayStart(at.Year(), at.Month(), at.Day(), f.Zone).UTC()
}

// UsageHistory are the history rows a filter selects, summed by key: by
// bucket, or by the filter's zone's days.
func (s *Store) UsageHistory(ctx context.Context, f HistoryFilter) ([]UsageBucket, error) {
	mid, err := s.merchant(ctx)
	if err != nil {
		return nil, err
	}
	sql := `SELECT customer_id, ` + f.bucket("bucket_at") + ` AS b, currency, event_type, meter, cell, sum(requests), sum(quantity), sum(stated),
       sum(amount), sum(discount), sum(failed_requests), sum(failed_amount), sum(forgiven)
FROM {database}.usage_history WHERE ` + f.where(mid, "bucket_at") + `
GROUP BY customer_id, b, currency, event_type, meter, cell`
	var out []UsageBucket
	err = s.query(ctx, "read usage history", sql, func(rows interface{ Scan(...any) error }) error {
		var b UsageBucket
		var customer uuid.UUID
		if err := rows.Scan(&customer, &b.BucketAt, &b.Currency, &b.EventType, &b.Meter, &b.Cell, &b.Requests, &b.Quantity, &b.Stated,
			&b.Amount, &b.Discount, &b.FailedRequests, &b.FailedAmount, &b.Forgiven); err != nil {
			return err
		}
		b.CustomerID, b.BucketAt = billing.CustomerID(customer), f.dayStart(b.BucketAt)
		out = append(out, b)
		return nil
	})
	return out, err
}

// UsageEvents are the raw usage events a filter selects (by occurred_at),
// deduplicated, summed by invoker and event type in history's buckets or the
// zone's days: what reads by invoker use, back EventsKept.
func (s *Store) UsageEvents(ctx context.Context, f HistoryFilter) ([]UsageBucket, error) {
	mid, err := s.merchant(ctx)
	if err != nil {
		return nil, err
	}
	at := "toDateTime(occurred_at, 'UTC')"
	sql := `SELECT customer_id, ` + f.bucket(at) + ` AS b, currency, event_type, invoker, toInt64(count()), toInt64(sum(ifNull(stated, 0))),
       sum(arraySum(charges.amount)), sum(arraySum(charges.discount)), toInt64(countIf(outcome = 'failed')), sum(charged), sum(forgiven)
FROM {database}.usage_events FINAL WHERE ` + f.where(mid, "occurred_at") + `
GROUP BY customer_id, b, currency, event_type, invoker`
	var out []UsageBucket
	err = s.query(ctx, "read usage events", sql, func(rows interface{ Scan(...any) error }) error {
		var b UsageBucket
		var customer uuid.UUID
		if err := rows.Scan(&customer, &b.BucketAt, &b.Currency, &b.EventType, &b.Invoker, &b.Requests, &b.Stated,
			&b.Amount, &b.Discount, &b.FailedRequests, &b.FailedAmount, &b.Forgiven); err != nil {
			return err
		}
		b.CustomerID, b.BucketAt = billing.CustomerID(customer), f.dayStart(b.BucketAt)
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
