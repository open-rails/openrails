package leases

import (
	"context"
	"time"

	"github.com/google/uuid"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
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

// pgUsage reads usage from PostgreSQL, in the sync's transaction, leaving
// out the syncing lease's own in-flight report.
type pgUsage struct {
	q        *gen.Queries
	money    *money.MoneyService
	merchant uuid.UUID
	now      time.Time
	except   uuid.UUID
}

func (u pgUsage) Capacity(ctx context.Context, customer uuid.UUID, currency string) (money.AdmissionCapacity, error) {
	return u.money.ViewCapacityTx(ctx, u.q, identity.CustomerID(customer), currency, u.except)
}

func (u pgUsage) Limit(ctx context.Context, customer uuid.UUID, c limits.Claim) (limits.Standing, *time.Time, error) {
	h := limits.Holder{Q: u.q, MerchantID: u.merchant, CustomerID: customer, Now: u.now, ExceptLease: &u.except}
	s, err := h.Standing(ctx, c)
	if err != nil {
		return s, nil, err
	}
	return s, h.ResetsAt(ctx, c, s), nil
}

func (u pgUsage) RecentSpend(ctx context.Context, customer uuid.UUID, currency string, since time.Time) (int64, error) {
	return u.q.SumCustomerRecentSpend(ctx, gen.SumCustomerRecentSpendParams{MerchantID: u.merchant, CustomerID: customer, Currency: currency, Since: since})
}
