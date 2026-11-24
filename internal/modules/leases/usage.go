package leases

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/modules/money"
)

// UsageReader is every read of what usage used that a view makes: the
// customer's money standing (with its unposted usage), a limit's use in a
// window, and recent spend. Views read nothing of usage but through it.
type UsageReader interface {
	// Capacity is the customer's money in a currency: its balance and
	// everything held against it, other than except's in-flight report.
	Capacity(ctx context.Context, customer uuid.UUID, currency string) (money.AdmissionCapacity, error)
	// Limit is where a claim's limit stands in its window and, were it
	// refused now, when it resets.
	Limit(ctx context.Context, customer uuid.UUID, c limits.Claim) (limits.Standing, *time.Time, error)
	// RecentSpend is what the customer's usage charged in a currency since
	// an instant.
	RecentSpend(ctx context.Context, customer uuid.UUID, currency string, since time.Time) (int64, error)
}

// storeUsage reads usage from the event store, after the sync's own usage
// was inserted, and money and window rows from PostgreSQL in the sync's
// transaction, leaving out the syncing lease's own in-flight report. The
// limit rows of every customer the sync views are read at once, on the first
// limit read. A window a read finds at capacity records reached_at.
type storeUsage struct {
	run    *syncRun
	store  *eventstore.Store
	loaded map[uuid.UUID][]eventstore.LimitRow
}

func (r *syncRun) usageReader() *storeUsage {
	return &storeUsage{run: r, store: r.DB.EventStore()}
}

func (u *storeUsage) Capacity(ctx context.Context, customer uuid.UUID, currency string) (money.AdmissionCapacity, error) {
	r := u.run
	return r.money.ViewCapacityTx(ctx, r.q, identity.CustomerID(customer), currency, r.lease.ID)
}

func (u *storeUsage) Limit(ctx context.Context, customer uuid.UUID, c limits.Claim) (limits.Standing, *time.Time, error) {
	r := u.run
	rows, err := u.rows(ctx, customer)
	if err != nil {
		return limits.Standing{}, nil, err
	}
	h := limits.Holder{Q: r.q, Events: u.store, Loaded: rows, MerchantID: r.merchant, CustomerID: customer, Now: r.now, ExceptLease: &r.lease.ID}
	s, row, err := h.Standing(ctx, c)
	if err != nil {
		return s, nil, err
	}
	if err := h.Reach(ctx, c, s, row); err != nil {
		return s, nil, err
	}
	return s, h.ResetsAt(ctx, c, s), nil
}

func (u *storeUsage) RecentSpend(ctx context.Context, customer uuid.UUID, currency string, since time.Time) (int64, error) {
	if u.store == nil {
		return 0, eventstore.Required("a view")
	}
	return u.store.MinuteAmount(ctx, billing.CustomerID(customer), currency, since, time.Time{})
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
	r := u.run
	defs, err := limits.Definitions(ctx, r.q, r.merchant)
	if err != nil {
		return nil, err
	}
	since := r.now
	for _, def := range defs {
		switch def.Reset.Kind {
		case catalog.ResetRolling:
			since = earliest(since, limits.RollingFrom(def.Reset.WindowSeconds, r.now))
		case catalog.ResetRate:
			since = earliest(since, limits.RateFrom(def.Reset.WindowSeconds, r.now))
		}
	}
	var customers []billing.CustomerID
	for _, v := range r.p.Views {
		if !v.CustomerID.IsZero() && (r.Allows == nil || r.Allows(v.CustomerID)) {
			customers = append(customers, v.CustomerID)
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
