package money

import (
	"context"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// Batched reads of many customers' money, for a sync's views and the watch
// of the balances its usage moved: one read for all of them, never one per
// customer, in PostgreSQL alone.

// UsageAccount is one customer's balance in a currency that usage moved.
type UsageAccount struct {
	Customer uuid.UUID
	Currency string
}

// ViewCapacities are many customers' capacities in one currency as views
// start from: GetAdmissionCapacity's for each, read without its writes or
// lock. A customer whose usage terms do not resolve is in failed alone.
func (s *MoneyService) ViewCapacities(ctx context.Context, q *gen.Queries, customers []uuid.UUID, currency string) (out map[uuid.UUID]AdmissionCapacity, failed map[uuid.UUID]error, err error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, nil, err
	}
	cur := normalizeCurrency(currency)
	balances, err := s.capacities(ctx, q, mid.UUID(), customers, cur)
	if err != nil {
		return nil, nil, err
	}
	out, failed = make(map[uuid.UUID]AdmissionCapacity, len(balances)), map[uuid.UUID]error{}
	for c, capacity := range balances {
		if capacity, err = s.usageAdmissionCapacity(ctx, q, identity.CustomerID(c), cur, capacity); err != nil {
			failed[c] = err
			continue
		}
		out[c] = capacity
	}
	return out, failed, nil
}

// capacities reads the customers' balances, pending usage and accrual.
func (s *MoneyService) capacities(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, customers []uuid.UUID, currency string) (map[uuid.UUID]AdmissionCapacity, error) {
	rows, err := q.ListViewCapacities(ctx, gen.ListViewCapacitiesParams{MerchantID: merchantID, Currency: currency, AsOf: s.now(), CustomerIds: customers})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]AdmissionCapacity, len(rows))
	for _, r := range rows {
		out[r.CustomerID] = AdmissionCapacity{Balance: r.Balance, Pending: r.Pending, Accrued: r.Accrued, BillingMode: r.BillingMode,
			CreditLimit: r.CreditLimitAmount, OutstandingOwed: r.OutstandingOwed}
	}
	return out, nil
}

// WatchUsage is told, in its own transaction after the usage committed, of
// the balances a sync's usage moved: what CommitUsageTx's callers in one
// transaction get from watchUsageTx.
func (s *MoneyService) WatchUsage(ctx context.Context, d *db.DB, changed []UsageAccount) error {
	if len(changed) == 0 {
		return nil
	}
	return d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		// A look holds its watch briefly; one that does not is left to it.
		if _, err := q.SetConfig(ctx, gen.SetConfigParams{Setting: "lock_timeout", Value: "2000", IsLocal: true}); err != nil {
			return err
		}
		return s.WithDatabase(d.NewWithPgxTx(tx)).watchUsageTx(ctx, q, changed)
	})
}

// watchUsageTx queues a look at each changed balance whose watch asks for
// one (ledger.WatchAvailable), and a usage invoice where what the customer
// owes reaches its threshold, reading every balance in one batch. It holds
// the watches against a look (ShareBalanceWatches) instead of the customers'
// locks.
func (s *MoneyService) watchUsageTx(ctx context.Context, q *gen.Queries, changed []UsageAccount) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	merchantID := mid.UUID()
	byCurrency := map[string][]uuid.UUID{}
	for _, a := range changed {
		if !slices.Contains(byCurrency[a.Currency], a.Customer) {
			byCurrency[a.Currency] = append(byCurrency[a.Currency], a.Customer)
		}
	}
	now := instant(s.now())
	for _, currency := range slices.Sorted(func(yield func(string) bool) {
		for c := range byCurrency {
			if !yield(c) {
				return
			}
		}
	}) {
		customers := byCurrency[currency]
		slices.SortFunc(customers, compareUUID)
		watches, err := q.ShareBalanceWatches(ctx, gen.ShareBalanceWatchesParams{MerchantID: merchantID, Currency: currency, CustomerIds: customers})
		if err != nil {
			return err
		}
		_, billed, err := s.usageBillingConfig(ctx, currency)
		if err != nil {
			return err
		}
		if len(watches) == 0 && billed == nil {
			continue
		}
		balances, err := s.capacities(ctx, q, merchantID, customers, currency)
		if err != nil {
			return err
		}
		for _, w := range watches {
			c := balances[w.CustomerID]
			if err := ledger.WatchAvailable(ctx, q, w, satSub(c.Balance, c.Unposted()), now); err != nil {
				return err
			}
		}
		if billed == nil {
			continue
		}
		for _, c := range customers {
			if u := balances[c].Pending; u > 0 {
				if err := s.requestUsageBilling(ctx, q, c, currency, false, max(u-max(balances[c].Balance, 0), 0)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
