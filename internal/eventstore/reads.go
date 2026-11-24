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
	// Later counts a posting read's usage at or after its cut.
	Later int64
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
	return s.quantities(ctx, sql, false)
}

// PostingQuantities are a customer's deduplicated quantities in a currency of
// the pricing periods from one start on, of the usage that occurred before
// cut, each with how much occurred at or after it (Later): what a posting
// takes.
func (s *Store) PostingQuantities(ctx context.Context, customer billing.CustomerID, currency string, from, cut time.Time) ([]PeriodQuantity, error) {
	mid, err := s.merchant(ctx)
	if err != nil {
		return nil, err
	}
	c := timeLiteral(cut)
	sql := fmt.Sprintf(`SELECT customer_id, currency, period_starts_at, charges.meter, charges.cell, charges.print, sumIf(charges.quantity, occurred_at < %[1]s),
       sumIf(charges.stated, occurred_at < %[1]s), toInt64(countIf(occurred_at < %[1]s)), toInt64(countIf(occurred_at >= %[1]s))
FROM {database}.usage_events FINAL ARRAY JOIN charges
WHERE merchant_id = %[2]s AND customer_id = %[3]s AND currency = %[4]s AND occurred_at >= %[5]s AND period_starts_at >= %[5]s
GROUP BY customer_id, currency, period_starts_at, charges.meter, charges.cell, charges.print`, c, mid, uuidLiteral(uuid.UUID(customer)), stringLiteral(currency), timeLiteral(from))
	return s.quantities(ctx, sql, true)
}

func (s *Store) quantities(ctx context.Context, sql string, later bool) ([]PeriodQuantity, error) {
	var out []PeriodQuantity
	err := s.query(ctx, "read usage quantities", sql, func(rows interface{ Scan(...any) error }) error {
		var q PeriodQuantity
		var customer uuid.UUID
		dest := []any{&customer, &q.Currency, &q.PeriodStartsAt, &q.Meter, &q.Cell, &q.Print, &q.Quantity, &q.Stated, &q.Requests}
		if later {
			dest = append(dest, &q.Later)
		}
		if err := rows.Scan(dest...); err != nil {
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

// UsageDay is one row of usage history: a day's events of one type, invoker
// and outcome (Meter ""), or what they took of one meter cell under one card
// print in one pricing period.
type UsageDay struct {
	CustomerID         billing.CustomerID
	Day                time.Time
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

// DayFilter selects usage history: one customer (all when zero), one
// currency (all when empty), the days in [From, To), and with PeriodsFrom
// only charge rows of periods that start then or later.
type DayFilter struct {
	CustomerID  billing.CustomerID
	Currency    string
	From, To    time.Time
	PeriodsFrom *time.Time
}

func (f DayFilter) where(mid string) string {
	w := []string{"merchant_id = " + mid, "day >= toDate(" + timeLiteral(f.From) + ", 'UTC')", "day < toDate(" + timeLiteral(f.To) + ", 'UTC')"}
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

// UsageDays are the history rows a filter selects, summed by key.
func (s *Store) UsageDays(ctx context.Context, f DayFilter) ([]UsageDay, error) {
	mid, err := s.merchant(ctx)
	if err != nil {
		return nil, err
	}
	sql := `SELECT customer_id, day, currency, event_type, meter, cell, print, period_starts_at, invoker, toString(outcome),
       toInt64(sum(requests)), sum(quantity), sum(stated), sum(charged), sum(forgiven)
FROM {database}.usage_days WHERE ` + f.where(mid) + `
GROUP BY customer_id, day, currency, event_type, meter, cell, print, period_starts_at, invoker, outcome`
	var out []UsageDay
	err = s.query(ctx, "read usage history", sql, func(rows interface{ Scan(...any) error }) error {
		var d UsageDay
		var customer uuid.UUID
		var outcome string
		if err := rows.Scan(&customer, &d.Day, &d.Currency, &d.EventType, &d.Meter, &d.Cell, &d.Print, &d.PeriodStartsAt, &d.Invoker, &outcome,
			&d.Requests, &d.Quantity, &d.Stated, &d.Charged, &d.Forgiven); err != nil {
			return err
		}
		d.CustomerID, d.Outcome = billing.CustomerID(customer), UsageOutcome(outcome)
		d.Day, d.PeriodStartsAt = time.Date(d.Day.Year(), d.Day.Month(), d.Day.Day(), 0, 0, 0, 0, time.UTC), d.PeriodStartsAt.UTC()
		out = append(out, d)
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
	sql := fmt.Sprintf(`SELECT count() > 0 FROM (SELECT 1 FROM {database}.usage_days WHERE merchant_id = %s AND event_type IN (%s) LIMIT 1)`,
		mid, strings.Join(types, ", "))
	var found bool
	err = s.query(ctx, "read usage activity", sql, func(rows interface{ Scan(...any) error }) error { return rows.Scan(&found) })
	return found, err
}

// stringLiteral quotes text for a query.
func stringLiteral(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// LastUsageDays is the day of each event type's latest usage.
func (s *Store) LastUsageDays(ctx context.Context, eventTypes []string) (map[string]time.Time, error) {
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
	sql := fmt.Sprintf(`SELECT event_type, max(day) FROM {database}.usage_days WHERE merchant_id = %s AND event_type IN (%s) GROUP BY event_type`,
		mid, strings.Join(types, ", "))
	err = s.query(ctx, "read usage activity", sql, func(rows interface{ Scan(...any) error }) error {
		var t string
		var day time.Time
		if err := rows.Scan(&t, &day); err != nil {
			return err
		}
		out[t] = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
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
