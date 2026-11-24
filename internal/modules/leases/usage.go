package leases

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/modules/money"
)

// UsageReader is every read of what usage used that a view makes: the
// customer's money standing (with its unposted usage), a limit's use in a
// window, and recent spend. Views read nothing of usage but through it.
type UsageReader interface {
	// Capacity is the customer's money in a currency: its balance and
	// everything held against it, other than the syncing lease's in-flight
	// report.
	Capacity(ctx context.Context, customer uuid.UUID, currency string) (money.AdmissionCapacity, error)
	// Limit is where a claim's limit stands in its window and, were it
	// refused now, when it resets.
	Limit(ctx context.Context, customer uuid.UUID, c limits.Claim) (limits.Standing, *time.Time, error)
	// RecentSpend is what the customer's usage charged in a currency in the
	// last recentWindow.
	RecentSpend(ctx context.Context, customer uuid.UUID, currency string) (int64, error)
}

// storeUsage answers a sync's views in batches: every viewed customer's
// money and recent spend are read at once (prefetch), and their limit rows
// at once on the first limit read; PostgreSQL in the views' snapshot, then
// the event store. A window a read finds at capacity is recorded once the
// snapshot ends (reached).
type storeUsage struct {
	v       *viewing
	store   *eventstore.Store
	caps    map[viewKey]money.AdmissionCapacity
	failed  map[viewKey]error
	recent  map[viewKey]int64
	loaded  map[uuid.UUID][]eventstore.LimitRow
	reached []reachedWindow
}

// reachedWindow is a window a view found at capacity.
type reachedWindow struct {
	customer uuid.UUID
	claim    limits.Claim
	standing limits.Standing
}

// prefetch reads the money and recent spend of every customer and currency
// the views name.
func (u *storeUsage) prefetch(ctx context.Context, keys []viewKey) error {
	u.caps, u.failed, u.recent = map[viewKey]money.AdmissionCapacity{}, map[viewKey]error{}, map[viewKey]int64{}
	byCurrency := map[string][]uuid.UUID{}
	var currencies []string
	for _, k := range keys {
		if _, ok := byCurrency[k.currency]; !ok {
			currencies = append(currencies, k.currency)
		}
		byCurrency[k.currency] = append(byCurrency[k.currency], k.customer)
	}
	v := u.v
	for _, currency := range currencies {
		caps, failed, err := v.money.ViewCapacities(ctx, v.q, byCurrency[currency], currency, &v.run.lease.ID)
		if err != nil {
			return err
		}
		for c, capacity := range caps {
			u.caps[viewKey{c, currency}] = capacity
		}
		for c, err := range failed {
			u.failed[viewKey{c, currency}] = err
		}
	}
	if u.store == nil {
		return eventstore.Required("a view")
	}
	for _, currency := range currencies {
		customers := make([]billing.CustomerID, 0, len(byCurrency[currency]))
		for _, c := range byCurrency[currency] {
			customers = append(customers, billing.CustomerID(c))
		}
		recent, err := u.store.MinuteAmounts(ctx, customers, currency, v.run.now.Add(-recentWindow))
		if err != nil {
			return err
		}
		for c, amount := range recent {
			u.recent[viewKey{c.UUID(), currency}] = amount
		}
	}
	return nil
}

func (u *storeUsage) Capacity(_ context.Context, customer uuid.UUID, currency string) (money.AdmissionCapacity, error) {
	k := viewKey{customer, currency}
	return u.caps[k], u.failed[k]
}

func (u *storeUsage) RecentSpend(_ context.Context, customer uuid.UUID, currency string) (int64, error) {
	return u.recent[viewKey{customer, currency}], nil
}

func (u *storeUsage) Limit(ctx context.Context, customer uuid.UUID, c limits.Claim) (limits.Standing, *time.Time, error) {
	v := u.v
	rows, err := u.rows(ctx, customer)
	if err != nil {
		return limits.Standing{}, nil, err
	}
	h := limits.Holder{Q: v.q, Events: u.store, Loaded: rows, MerchantID: v.run.merchant, CustomerID: customer, Now: v.run.now, ExceptLease: &v.run.lease.ID}
	s, row, err := h.Standing(ctx, c)
	if err != nil {
		return s, nil, err
	}
	if limits.Reached(c, s, row) {
		u.reached = append(u.reached, reachedWindow{customer: customer, claim: c, standing: s})
	}
	return s, h.ResetsAt(ctx, c, s), nil
}

// rows are a viewed customer's limit rows: every viewed customer's, read at
// once, back as far as the longest rolling or rate limit counts.
func (u *storeUsage) rows(ctx context.Context, customer uuid.UUID) ([]eventstore.LimitRow, error) {
	if u.loaded != nil {
		if rows, ok := u.loaded[customer]; ok {
			return rows, nil
		}
		return nil, nil
	}
	if u.store == nil {
		return nil, eventstore.Required("a view")
	}
	v := u.v
	r := v.run
	since := r.now
	for _, def := range v.defs {
		switch def.Reset.Kind {
		case catalog.ResetRolling:
			since = earliest(since, limits.RollingFrom(def.Reset.WindowSeconds, r.now))
		case catalog.ResetRate:
			since = earliest(since, limits.RateFrom(def.Reset.WindowSeconds, r.now))
		}
	}
	var customers []billing.CustomerID
	for _, vp := range r.p.Views {
		if !vp.CustomerID.IsZero() && (r.Allows == nil || r.Allows(vp.CustomerID)) {
			customers = append(customers, vp.CustomerID)
		}
	}
	rows, err := u.store.LimitUsage(ctx, customers, since)
	if err != nil {
		return nil, err
	}
	u.loaded = map[uuid.UUID][]eventstore.LimitRow{}
	for _, c := range customers {
		u.loaded[c.UUID()] = []eventstore.LimitRow{}
	}
	for _, row := range rows {
		u.loaded[row.CustomerID.UUID()] = append(u.loaded[row.CustomerID.UUID()], row)
	}
	return u.loaded[customer], nil
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
