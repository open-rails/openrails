package limits

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/retention"
)

// Lease timing. Neither is a setting: correctness holds for any value, and
// only speed depends on them.
const (
	// SyncInterval is how often a live gate syncs its lease.
	SyncInterval = time.Second
	// LeaseLifetime is how long a lease lives without a sync.
	LeaseLifetime = 10 * time.Second
)

// SpendKeyPrefix names a billing policy's spend window among limit windows:
// spend: and its hashed identity.
const SpendKeyPrefix = "spend:"

// Claim is what one hold asks of one limit: Cost units in Window, against
// Capacity (the products' sum, an override, or a spend window's limit,
// before adjustments). Opens is a session or first-use window the hold opens.
type Claim struct {
	Key      string
	Unit     string
	Reset    catalog.LimitReset
	Window   Window
	Opens    bool
	Capacity int64
	Cost     int64
	Overrun  catalog.Overrun
}

// Standing is where a limit stands in a claim's window: its capacity with
// adjustments, its settled units (a rate limit's level now) and what live
// holds set aside.
type Standing struct {
	Capacity int64
	Settled  int64
	Held     int64
}

// Remaining is Capacity - Settled - Held, saturating.
func (s Standing) Remaining() int64 { return subSat(subSat(s.Capacity, s.Settled), s.Held) }

// Taken is a hold's outcome: the first claim refused (-1 when all fit) and
// when that limit resets, or what the hold sets aside of each claim.
type Taken struct {
	Refused  int
	ResetsAt *time.Time
	Held     []int64
}

// Holder takes and settles one customer's claims. Its caller holds the
// customer's spend lock; window rows are locked in key order.
type Holder struct {
	Q          *gen.Queries
	MerchantID uuid.UUID
	CustomerID uuid.UUID
	Now        time.Time
}

// heldSince bounds the holds a window sum reads to the retained months.
func (h Holder) heldSince() time.Time { return retention.AdmissionsDropBefore(h.Now) }

// Take checks every claim and, when all fit, sets aside what each takes:
// all or nothing. require_estimate fits when settled + held + cost is at
// most capacity; admit_while_positive while any capacity remains, holding
// at most what remains. A claim that takes its window to capacity records
// reached_at. A refusal opens no window and holds nothing.
func (h Holder) Take(ctx context.Context, claims []Claim) (Taken, error) {
	h.Now = h.Now.UTC()
	out := Taken{Refused: -1, Held: make([]int64, len(claims))}
	rows := make([]*gen.GetLimitWindowForUpdateRow, len(claims))
	standings := make([]Standing, len(claims))
	for _, i := range lockOrder(claims) {
		c := claims[i]
		if !c.Opens {
			if err := h.ensure(ctx, c.Key, c.Window, c.Capacity); err != nil {
				return out, err
			}
		}
		row, err := h.lock(ctx, c.Key, c.Window.StartsAt)
		if err != nil {
			return out, err
		}
		rows[i] = row
		if standings[i], err = h.standing(ctx, c.Key, c.Reset, c.Window, c.Capacity, row); err != nil {
			return out, err
		}
	}
	for i, c := range claims {
		s := standings[i]
		fits := c.Cost <= s.Remaining()
		if c.Overrun == catalog.OverrunAdmitWhilePositive {
			fits = s.Remaining() > 0
		}
		if !fits {
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
		var reached *time.Time
		if total := addSat(addSat(s.Settled, s.Held), out.Held[i]); total > 0 && total >= s.Capacity && (row == nil || row.ReachedAt == nil) {
			reached = &h.Now
		}
		if reached == nil && c.Reset.Kind != catalog.ResetRate {
			continue
		}
		// A rate limit's level is written as of now, so its drain stays exact.
		settled := s.Settled
		if c.Reset.Kind != catalog.ResetRate && row != nil {
			settled = row.Settled
		}
		if err := h.write(ctx, c.Key, c.Window.StartsAt, settled, c.Capacity, reached); err != nil {
			return out, err
		}
	}
	return out, nil
}

// Locked is the claims' windows locked in order, with where each stands.
type Locked struct {
	Claims    []Claim
	Standings []Standing
	rows      []*gen.GetLimitWindowForUpdateRow
}

// Lock locks every claim's window in key order and reads where each stands,
// for a caller that sizes what it sets aside itself. A session or first-use
// window a claim opens is not written until Commit.
func (h Holder) Lock(ctx context.Context, claims []Claim) (Locked, error) {
	h.Now = h.Now.UTC()
	l := Locked{Claims: claims, Standings: make([]Standing, len(claims)), rows: make([]*gen.GetLimitWindowForUpdateRow, len(claims))}
	for _, i := range lockOrder(claims) {
		c := claims[i]
		if !c.Opens {
			if err := h.ensure(ctx, c.Key, c.Window, c.Capacity); err != nil {
				return l, err
			}
		}
		row, err := h.lock(ctx, c.Key, c.Window.StartsAt)
		if err != nil {
			return l, err
		}
		l.rows[i] = row
		if l.Standings[i], err = h.standing(ctx, c.Key, c.Reset, c.Window, c.Capacity, row); err != nil {
			return l, err
		}
	}
	return l, nil
}

// Commit records what a caller changed of each locked claim's holds: added
// (negative gives back) on top of the standing it read. It opens the windows
// that held something, records reached_at and writes a rate limit's level as
// of now, as Take does.
func (h Holder) Commit(ctx context.Context, l Locked, added []int64) error {
	h.Now = h.Now.UTC()
	for _, i := range lockOrder(l.Claims) {
		c, s, row := l.Claims[i], l.Standings[i], l.rows[i]
		if c.Opens && row == nil {
			if added[i] <= 0 {
				continue
			}
			if err := h.ensure(ctx, c.Key, c.Window, c.Capacity); err != nil {
				return err
			}
		}
		var reached *time.Time
		if total := addSat(addSat(s.Settled, s.Held), added[i]); added[i] > 0 && total > 0 && total >= s.Capacity && (row == nil || row.ReachedAt == nil) {
			reached = &h.Now
		}
		if reached == nil && c.Reset.Kind != catalog.ResetRate {
			continue
		}
		settled := s.Settled
		if c.Reset.Kind != catalog.ResetRate && row != nil {
			settled = row.Settled
		}
		if err := h.write(ctx, c.Key, c.Window.StartsAt, settled, c.Capacity, reached); err != nil {
			return err
		}
	}
	return nil
}

// ResetsAt is when a refused claim's limit resets.
func (h Holder) ResetsAt(ctx context.Context, c Claim, s Standing) *time.Time {
	h.Now = h.Now.UTC()
	return h.resetsAt(ctx, c, s)
}

// Settlement is units a capture or report settles into one limit's window:
// the window its hold was issued in, or the current one. KeepCapacity keeps
// the capacity the window last stored (a spend window's).
type Settlement struct {
	Key          string
	Reset        catalog.LimitReset
	Window       Window
	Capacity     int64
	KeepCapacity bool
	Units        int64
}

// Settle adds each settlement's units to its window in full, never clamped,
// and records reached_at when settled and held units first reach capacity.
// The caller has ended the hold whose usage this is, so it no longer counts
// as held.
func (h Holder) Settle(ctx context.Context, settlements []Settlement) error {
	h.Now = h.Now.UTC()
	order := make([]int, 0, len(settlements))
	for i, s := range settlements {
		if s.Units != 0 {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := settlements[order[a]], settlements[order[b]]
		return x.Key < y.Key || (x.Key == y.Key && x.Window.StartsAt.Before(y.Window.StartsAt))
	})
	for _, i := range order {
		s := settlements[i]
		if err := h.ensure(ctx, s.Key, s.Window, s.Capacity); err != nil {
			return err
		}
		row, err := h.lock(ctx, s.Key, s.Window.StartsAt)
		if err != nil {
			return err
		}
		if row == nil {
			return errors.New("limit window vanished under its lock")
		}
		capacity := s.Capacity
		if s.KeepCapacity {
			capacity = row.Capacity
		}
		standing, err := h.standing(ctx, s.Key, s.Reset, s.Window, capacity, row)
		if err != nil {
			return err
		}
		settled := addSat(row.Settled, s.Units)
		if s.Reset.Kind == catalog.ResetRate {
			settled = addSat(standing.Settled, s.Units)
		}
		standing.Settled = addSat(standing.Settled, s.Units)
		var reached *time.Time
		if row.ReachedAt == nil && addSat(standing.Settled, standing.Held) >= standing.Capacity {
			reached = &h.Now
		}
		if err := h.write(ctx, s.Key, s.Window.StartsAt, settled, capacity, reached); err != nil {
			return err
		}
	}
	return nil
}

// Held is what live holds set aside of a limit from window from through to.
func (h Holder) Held(ctx context.Context, key string, from, to time.Time, countsExpired bool) (int64, error) {
	return h.Q.SumWindowHeld(ctx, gen.SumWindowHeldParams{
		MerchantID: h.MerchantID, CustomerID: h.CustomerID, LimitKey: key, FromWindow: from, ToWindow: to,
		HeldSince: h.heldSince(), CountsExpired: countsExpired, AsOf: h.Now, LeaseLiveSince: h.Now.Add(-LeaseLifetime),
	})
}

// standing is a limit's standing in w given its row there (nil: none). A
// rolling limit counts its buckets from the window's start; a rate limit's
// level drains from the row's last write. An expired request hold still
// counts in a window that ends.
func (h Holder) standing(ctx context.Context, key string, reset catalog.LimitReset, w Window, capacity int64, row *gen.GetLimitWindowForUpdateRow) (Standing, error) {
	s := Standing{Capacity: capacity}
	from := w.StartsAt
	switch reset.Kind {
	case catalog.ResetRolling:
		from = RollingFrom(reset.WindowSeconds, h.Now)
		buckets, err := h.Q.ListLimitWindowsFrom(ctx, gen.ListLimitWindowsFromParams{MerchantID: h.MerchantID, CustomerID: h.CustomerID, LimitKey: key, WindowStartsAt: from})
		if err != nil {
			return s, err
		}
		for _, b := range buckets {
			s.Capacity = addSat(s.Capacity, b.Adjusted)
			s.Settled = addSat(s.Settled, b.Settled)
		}
	case catalog.ResetRate:
		if row != nil {
			s.Settled = RateLevel(row.Settled, capacity, reset.WindowSeconds, row.UpdatedAt, h.Now)
		}
	default:
		if row != nil {
			s.Capacity = addSat(s.Capacity, row.Adjusted)
			s.Settled = row.Settled
		}
	}
	held, err := h.Held(ctx, key, from, w.StartsAt, w.EndsAt != nil)
	s.Held = held
	return s, err
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
		from := RollingFrom(c.Reset.WindowSeconds, h.Now)
		buckets, err := h.Q.ListLimitWindowsFrom(ctx, gen.ListLimitWindowsFromParams{MerchantID: h.MerchantID, CustomerID: h.CustomerID, LimitKey: c.Key, WindowStartsAt: from})
		if err != nil {
			return c.Window.EndsAt
		}
		for _, b := range buckets {
			if b.Settled != 0 {
				return ptr(b.WindowStartsAt.UTC().Add(BucketWidth(c.Reset.WindowSeconds) + time.Duration(c.Reset.WindowSeconds)*time.Second))
			}
		}
		return ptr(c.Window.StartsAt.Add(BucketWidth(c.Reset.WindowSeconds) + time.Duration(c.Reset.WindowSeconds)*time.Second))
	}
	return c.Window.EndsAt
}

func (h Holder) ensure(ctx context.Context, key string, w Window, capacity int64) error {
	return h.Q.EnsureLimitWindow(ctx, gen.EnsureLimitWindowParams{MerchantID: h.MerchantID, CustomerID: h.CustomerID, LimitKey: key,
		WindowStartsAt: w.StartsAt, WindowEndsAt: w.EndsAt, Capacity: capacity, AsOf: h.Now})
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

func (h Holder) write(ctx context.Context, key string, start time.Time, settled, capacity int64, reached *time.Time) error {
	return h.Q.WriteLimitWindow(ctx, gen.WriteLimitWindowParams{Settled: settled, Capacity: capacity, ReachedAt: reached, AsOf: h.Now,
		MerchantID: h.MerchantID, CustomerID: h.CustomerID, LimitKey: key, WindowStartsAt: start})
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
