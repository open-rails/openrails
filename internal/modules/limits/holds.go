package limits

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/retention"
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

// HeldSince is the oldest hold row that still holds at now: no hold lives
// past the hold lifetime, a request's deadline included.
func HeldSince(now time.Time) time.Time { return now.Add(-retention.AdmissionMaxHold) }

// SpendKeyPrefix names a billing policy's spend window among limit windows:
// spend: and its hashed identity.
const SpendKeyPrefix = "spend:"

// Claim is what one hold asks of one limit: Cost units in Window, against
// Capacity (the products' sum, an override, or a spend window's limit,
// before adjustments). Opens is a session or first-use window the hold opens.
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
}

// Standing is where a limit stands in a claim's window: its capacity with
// adjustments, the units usage took (a rate limit's level now) and what live
// holds and gates' in-flight reports set aside.
type Standing struct {
	Capacity int64
	Settled  int64
	Held     int64
}

// Remaining is Capacity - Settled - Held, saturating.
func (s Standing) Remaining() int64 { return subSat(subSat(s.Capacity, s.Settled), s.Held) }

// Taken is a hold's outcome: the first claim refused (-1 when all fit) and
// when that limit resets, or what the hold sets aside of each claim. Every
// claim's standing is read either way.
type Taken struct {
	Refused   int
	ResetsAt  *time.Time
	Held      []int64
	Standings []Standing
}

// Holder takes one customer's claims and reads where its limits stand. Its
// caller holds the customer's spend lock to take; window rows are locked in
// key order. What usage took of a window is the event store's (Events, else
// the one bound to Q's transaction), and Loaded, when set, is the customer's
// rows already read. ExceptLease leaves one lease's in-flight report out of
// what it reads as held.
type Holder struct {
	Q           *gen.Queries
	Events      *eventstore.Store
	Loaded      []eventstore.LimitRow
	MerchantID  uuid.UUID
	CustomerID  uuid.UUID
	Now         time.Time
	ExceptLease *uuid.UUID
}

// Take checks every claim and, when all fit, sets aside what each takes:
// all or nothing. require_estimate fits when settled + held + cost is at
// most capacity; admit_while_positive while any capacity remains, holding
// at most what remains. A claim that opens a session or first-use window
// opens it, and one that takes its window to capacity records reached_at. A
// refusal opens no window and holds nothing.
func (h Holder) Take(ctx context.Context, claims []Claim) (Taken, error) {
	h.Now = h.Now.UTC()
	out := Taken{Refused: -1, Held: make([]int64, len(claims)), Standings: make([]Standing, len(claims))}
	rows := make([]*gen.GetLimitWindowForUpdateRow, len(claims))
	standings := out.Standings
	for _, i := range lockOrder(claims) {
		c := claims[i]
		row, err := h.lock(ctx, c.Key, c.Window.StartsAt)
		if err != nil {
			return out, err
		}
		rows[i] = row
		if standings[i], err = h.standing(ctx, c, row); err != nil {
			return out, err
		}
	}
	for i, c := range claims {
		s := standings[i]
		if !Fits(c, s, 0) {
			out.Refused, out.ResetsAt = i, h.resetsAt(ctx, c, s)
			return out, nil
		}
		out.Held[i] = min(c.Cost, max(s.Remaining(), 0))
	}
	for _, i := range lockOrder(claims) {
		c, s, row := claims[i], standings[i], rows[i]
		if c.Opens && row == nil {
			if err := h.ensure(ctx, c.Key, c.Window, c.Capacity); err != nil {
				return out, err
			}
		}
		s.Held = addSat(s.Held, out.Held[i])
		if err := h.Reach(ctx, c, s, row); err != nil {
			return out, err
		}
	}
	return out, nil
}

// Reach records the first time a claim's window is found at capacity: on
// its row, which it adds for a window that has none (a rolling limit's
// bucket, a rate or lifetime limit's one row).
func (h Holder) Reach(ctx context.Context, c Claim, s Standing, row *gen.GetLimitWindowForUpdateRow) error {
	total := addSat(s.Settled, s.Held)
	if c.Ungranted || total <= 0 || total < s.Capacity || row != nil && row.ReachedAt != nil {
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

// ResetsAt is when a refused claim's limit resets.
func (h Holder) ResetsAt(ctx context.Context, c Claim, s Standing) *time.Time {
	h.Now = h.Now.UTC()
	return h.resetsAt(ctx, c, s)
}

// Fits reports whether c fits with extra more capacity than s leaves, by
// its overrun.
func Fits(c Claim, s Standing, extra int64) bool {
	remaining := addSat(s.Remaining(), extra)
	if c.Overrun == catalog.OverrunAdmitWhilePositive {
		return remaining > 0
	}
	return c.Cost <= remaining
}

// Held is what live holds set aside of a limit from window from through to,
// and in the current window (inFlight) what live gates report in flight.
func (h Holder) Held(ctx context.Context, key string, from, to time.Time, countsExpired, inFlight bool) (int64, error) {
	return h.Q.SumWindowHeld(ctx, gen.SumWindowHeldParams{
		MerchantID: h.MerchantID, CustomerID: h.CustomerID, LimitKey: key, FromWindow: from, ToWindow: to,
		HeldSince: HeldSince(h.Now), CountsExpired: countsExpired, AsOf: h.Now, LeaseLiveSince: LeaseLiveSince(h.Now),
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
// start; a rate limit's level is read from its recent buckets. An expired
// request hold still counts in a window that ends.
func (h Holder) standing(ctx context.Context, c Claim, row *gen.GetLimitWindowForUpdateRow) (Standing, error) {
	s := Standing{Capacity: c.Capacity}
	w, from := c.Window, c.Window.StartsAt
	switch c.Reset.Kind {
	case catalog.ResetRolling:
		from = RollingFrom(c.Reset.WindowSeconds, h.Now)
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
	current := !h.Now.Before(w.StartsAt) && (w.EndsAt == nil || h.Now.Before(*w.EndsAt)) || c.Reset.Kind == catalog.ResetRolling
	held, err := h.Held(ctx, c.Key, from, w.StartsAt, w.EndsAt != nil, current)
	s.Held = held
	return s, err
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

func (h Holder) lock(ctx context.Context, key string, start time.Time) (*gen.GetLimitWindowForUpdateRow, error) {
	row, err := h.Q.GetLimitWindowForUpdate(ctx, gen.GetLimitWindowForUpdateParams{MerchantID: h.MerchantID, CustomerID: h.CustomerID, LimitKey: key, WindowStartsAt: start})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// lockOrder is the claims' indices in window id order.
func lockOrder(claims []Claim) []int {
	order := make([]int, len(claims))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := claims[order[a]], claims[order[b]]
		return x.Key < y.Key || (x.Key == y.Key && x.Window.StartsAt.Before(y.Window.StartsAt))
	})
	return order
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
