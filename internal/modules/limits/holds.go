package limits

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/eventstore"
)

// Lease timing. None is a setting: the bound holds for any value, and only
// speed depends on them.
const (
	// SyncInterval is how often an active gate syncs its lease.
	SyncInterval = time.Second
	// LeaseLifetime is how long a lease stays live without a sync.
	LeaseLifetime = 10 * time.Second
	// SyncTimeout bounds one sync's transaction.
	SyncTimeout = 5 * time.Second
)

// LeaseLiveSince is the oldest last sync of a lease readers count live at
// now.
func LeaseLiveSince(now time.Time) time.Time { return now.Add(-LeaseLifetime) }

// SpendKeyPrefix names a billing policy's spend window among limit windows:
// spend: and its hashed identity.
const SpendKeyPrefix = "spend:"

// Claim is what a request asks of one limit: Cost units in Window, against
// Capacity (the products' sum, an override, or a spend window's limit,
// before adjustments). Opens is a session or first-use window a use opens.
// Ungranted is a limit no product or override gives the customer: usage
// still counts in a window row a grant or an adjustment made, and nothing
// enforces it.
type Claim struct {
	Key       string
	Unit      string
	Reset     catalog.LimitReset
	Window    Window
	Opens     bool
	Capacity  int64
	Cost      int64
	Overrun   catalog.Overrun
	Ungranted bool
	// Stored: the window has its row (Windows sets it).
	Stored bool
}

// Standing is where a limit stands in a claim's window: its capacity with
// adjustments, the units usage took (a rate limit's level now) and what live
// gates' in-flight reports set aside.
type Standing struct {
	Capacity int64
	Settled  int64
	Held     int64
}

// Remaining is Capacity - Settled - Held, saturating.
func (s Standing) Remaining() int64 { return subSat(subSat(s.Capacity, s.Settled), s.Held) }

// Holder reads where one customer's limits stand. What usage took of a
// window is the event store's (Events, else the one bound to Q's
// transaction), and Loaded, when set, is the customer's rows already read.
// ExceptLease leaves one lease's in-flight report out of what it reads as
// held.
type Holder struct {
	Q           *gen.Queries
	Events      *eventstore.Store
	Loaded      []eventstore.LimitRow
	MerchantID  uuid.UUID
	CustomerID  uuid.UUID
	Now         time.Time
	ExceptLease *uuid.UUID
}

// Reach records the first time a claim's window is found at capacity: on
// its row, which it adds for a window that has none (a rolling limit's
// bucket, a rate or lifetime limit's one row).
func (h Holder) Reach(ctx context.Context, c Claim, s Standing, row *gen.GetLimitWindowForUpdateRow) error {
	if !Reached(c, s, row) {
		return nil
	}
	if row == nil {
		if err := h.ensure(ctx, c.Key, c.Window, c.Capacity); err != nil {
			return err
		}
	}
	return h.Q.ReachLimitWindow(ctx, gen.ReachLimitWindowParams{Capacity: c.Capacity, ReachedAt: h.Now.UTC(),
		MerchantID: h.MerchantID, CustomerID: h.CustomerID, LimitKey: c.Key, WindowStartsAt: c.Window.StartsAt})
}

// Reached reports a claim's window found at capacity whose row (nil: none)
// has not recorded it: what Reach records.
func Reached(c Claim, s Standing, row *gen.GetLimitWindowForUpdateRow) bool {
	total := addSat(s.Settled, s.Held)
	return !c.Ungranted && total > 0 && total >= s.Capacity && (row == nil || row.ReachedAt == nil)
}

// ResetsAt is when a refused claim's limit resets.
func (h Holder) ResetsAt(ctx context.Context, c Claim, s Standing) *time.Time {
	h.Now = h.Now.UTC()
	return h.resetsAt(ctx, c, s)
}

// Held is what live gates report in flight of a limit: only in its current
// window (inFlight).
func (h Holder) Held(ctx context.Context, key string, inFlight bool) (int64, error) {
	return h.Q.SumWindowHeld(ctx, gen.SumWindowHeldParams{
		MerchantID: h.MerchantID, CustomerID: h.CustomerID, LimitKey: key, LeaseLiveSince: LeaseLiveSince(h.Now),
		InFlight: inFlight, ExceptLease: h.ExceptLease,
	})
}

// Standing is where a claim's limit stands in its window, read without
// locking it; Row is the window's row.
func (h Holder) Standing(ctx context.Context, c Claim) (Standing, *gen.GetLimitWindowForUpdateRow, error) {
	h.Now = h.Now.UTC()
	row, err := h.read(ctx, c.Key, c.Window.StartsAt)
	if err != nil {
		return Standing{}, nil, err
	}
	s, err := h.standing(ctx, c, row)
	return s, row, err
}

// standing is a claim's limit's standing in its window given the window's
// row (nil: none). A rolling limit counts its buckets from the window's
// start; a rate limit's level is read from its recent buckets. What is held
// is read before the event store, so an item is counted twice at worst, and
// not at all only between its sync's commit, which ends the in-flight report
// that held it, and its insert.
func (h Holder) standing(ctx context.Context, c Claim, row *gen.GetLimitWindowForUpdateRow) (Standing, error) {
	s := Standing{Capacity: c.Capacity}
	w, from := c.Window, c.Window.StartsAt
	if c.Reset.Kind == catalog.ResetRolling {
		from = RollingFrom(c.Reset.WindowSeconds, h.Now)
	}
	current := !h.Now.Before(w.StartsAt) && (w.EndsAt == nil || h.Now.Before(*w.EndsAt)) || c.Reset.Kind == catalog.ResetRolling
	held, err := h.Held(ctx, c.Key, current)
	if err != nil {
		return s, err
	}
	s.Held = held
	switch c.Reset.Kind {
	case catalog.ResetRolling:
		adjusted, err := h.Q.ListLimitWindowsFrom(ctx, gen.ListLimitWindowsFromParams{MerchantID: h.MerchantID, CustomerID: h.CustomerID, LimitKey: c.Key, WindowStartsAt: from})
		if err != nil {
			return s, err
		}
		for _, b := range adjusted {
			s.Capacity = addSat(s.Capacity, b.Adjusted)
		}
		buckets, err := h.Units(ctx, c.Key, from, h.Now)
		if err != nil {
			return s, err
		}
		for _, b := range buckets {
			s.Settled = addSat(s.Settled, b.Units)
		}
	case catalog.ResetRate:
		buckets, err := h.Units(ctx, c.Key, RateFrom(c.Reset.WindowSeconds, h.Now), h.Now)
		if err != nil {
			return s, err
		}
		s.Settled = RateLevelOf(buckets, c.Capacity, c.Reset.WindowSeconds, h.Now)
	default:
		if row != nil {
			s.Capacity = addSat(s.Capacity, row.Adjusted)
		}
		if !c.Opens {
			units, err := h.Units(ctx, c.Key, w.StartsAt, w.StartsAt)
			if err != nil {
				return s, err
			}
			for _, u := range units {
				s.Settled = addSat(s.Settled, u.Units)
			}
		}
	}
	return s, nil
}

// Units is what usage took of one limit in its windows (or buckets) that
// start within [from, to], from the event store.
func (h Holder) Units(ctx context.Context, key string, from, to time.Time) ([]eventstore.LimitRow, error) {
	if h.Loaded != nil {
		var out []eventstore.LimitRow
		for _, r := range h.Loaded {
			if r.Key == key && !r.WindowStartsAt.Before(from) && !r.WindowStartsAt.After(to) {
				out = append(out, r)
			}
		}
		return out, nil
	}
	store := h.Events
	if store == nil {
		store = db.EventStoreOf(h.Q)
	}
	if store == nil {
		return nil, eventstore.Required("a usage limit")
	}
	return store.LimitUnits(ctx, billing.CustomerID(h.CustomerID), key, from, to)
}

// resetsAt is when a refused claim's limit resets: its window's end, a
// rolling limit's oldest counted bucket leaving it, a rate limit drained,
// never for a lifetime limit.
func (h Holder) resetsAt(ctx context.Context, c Claim, s Standing) *time.Time {
	switch c.Reset.Kind {
	case catalog.ResetLifetime:
		return nil
	case catalog.ResetRate:
		return RateRefilledAt(addSat(s.Settled, s.Held), s.Capacity, c.Reset.WindowSeconds, h.Now)
	case catalog.ResetRolling:
		if at := h.rollingResetsAt(ctx, c.Key, c.Reset); at != nil {
			return at
		}
		return ptr(c.Window.StartsAt.Add(BucketWidth(c.Reset.WindowSeconds) + time.Duration(c.Reset.WindowSeconds)*time.Second))
	}
	return c.Window.EndsAt
}

// rollingResetsAt is when a rolling limit's oldest bucket with usage leaves
// its window; nil when none has usage.
func (h Holder) rollingResetsAt(ctx context.Context, key string, reset catalog.LimitReset) *time.Time {
	buckets, err := h.Units(ctx, key, RollingFrom(reset.WindowSeconds, h.Now), h.Now)
	if err != nil {
		return nil
	}
	for _, b := range buckets {
		if b.Units != 0 {
			return ptr(b.WindowStartsAt.Add(BucketWidth(reset.WindowSeconds) + time.Duration(reset.WindowSeconds)*time.Second))
		}
	}
	return nil
}

func (h Holder) ensure(ctx context.Context, key string, w Window, capacity int64) error {
	return h.Q.EnsureLimitWindow(ctx, gen.EnsureLimitWindowParams{MerchantID: h.MerchantID, CustomerID: h.CustomerID, LimitKey: key,
		WindowStartsAt: w.StartsAt, WindowEndsAt: w.EndsAt, Capacity: capacity, AsOf: h.Now})
}

// read is the window row at start without locking it.
func (h Holder) read(ctx context.Context, key string, start time.Time) (*gen.GetLimitWindowForUpdateRow, error) {
	row, err := h.Q.GetLimitWindow(ctx, gen.GetLimitWindowParams{MerchantID: h.MerchantID, CustomerID: h.CustomerID, LimitKey: key, WindowStartsAt: start})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &gen.GetLimitWindowForUpdateRow{LimitKey: row.LimitKey, WindowStartsAt: row.WindowStartsAt, WindowEndsAt: row.WindowEndsAt,
		Adjusted: row.Adjusted, ReachedAt: row.ReachedAt, UpdatedAt: row.UpdatedAt}, nil
}

func addSat(a, b int64) int64 {
	switch {
	case b > 0 && a > maxInt64-b:
		return maxInt64
	case b < 0 && a < -maxInt64-1-b:
		return -maxInt64 - 1
	}
	return a + b
}

func subSat(a, b int64) int64 {
	if b == -maxInt64-1 {
		return addSat(addSat(a, maxInt64), 1)
	}
	return addSat(a, -b)
}
