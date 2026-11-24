package money

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// Batched reads of many customers' money, for a sync's views and the watch
// of the balances its usage moved: one read per kind for all of them, never
// one per customer. PostgreSQL is read before the event store: every writer
// inserts its events before it commits what they replace (an in-flight
// report, an unposted mark), so a reader in that order
// counts an item twice at worst, never not at all.

// UsageAccount is one customer's balance in a currency that usage moved.
type UsageAccount struct {
	Customer uuid.UUID
	Currency string
}

// unpostedMarks are customers' unposted marks in a currency and what their
// periods' postings took, read in PostgreSQL before the event store.
type unpostedMarks struct {
	currency string
	since    map[uuid.UUID]time.Time
	posted   map[uuid.UUID]map[time.Time]int64
}

func readUnpostedMarks(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, customers []uuid.UUID, currency string) (unpostedMarks, error) {
	m := unpostedMarks{currency: currency, since: map[uuid.UUID]time.Time{}, posted: map[uuid.UUID]map[time.Time]int64{}}
	if len(customers) == 0 {
		return m, nil
	}
	rows, err := q.ListUsageUnpostedOf(ctx, gen.ListUsageUnpostedOfParams{MerchantID: merchantID, Currency: currency, CustomerIds: customers})
	if err != nil || len(rows) == 0 {
		return m, err
	}
	var from time.Time
	marked := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		m.since[r.CustomerID] = r.SinceAt.UTC()
		marked = append(marked, r.CustomerID)
		if from.IsZero() || r.SinceAt.Before(from) {
			from = r.SinceAt.UTC()
		}
	}
	postings, err := q.ListUsagePostingsOf(ctx, gen.ListUsagePostingsOfParams{MerchantID: merchantID, CustomerIds: marked, Currency: currency, PeriodStartsAt: from})
	if err != nil {
		return m, err
	}
	for _, p := range postings {
		since, ok := m.since[p.CustomerID]
		period := p.PeriodStartsAt.UTC()
		if !ok || period.Before(since) {
			continue
		}
		if m.posted[p.CustomerID] == nil {
			m.posted[p.CustomerID] = map[time.Time]int64{}
		}
		m.posted[p.CustomerID][period] = addSat(m.posted[p.CustomerID][period], p.Amount)
	}
	return m, nil
}

// price is what each marked customer's usage costs beyond what posted, as
// unpostedUsage prices it: one event store read for all of them.
func (m unpostedMarks) price(ctx context.Context, q *gen.Queries, store *eventstore.Store, merchantID uuid.UUID) (map[uuid.UUID]int64, error) {
	out := map[uuid.UUID]int64{}
	if len(m.since) == 0 {
		return out, nil
	}
	if store == nil {
		return nil, eventstore.Required("a balance with unposted usage")
	}
	var from time.Time
	customers := make([]billing.CustomerID, 0, len(m.since))
	for c, since := range m.since {
		customers = append(customers, billing.CustomerID(c))
		if from.IsZero() || since.Before(from) {
			from = since
		}
	}
	rows, err := store.PeriodQuantities(ctx, customers, m.currency, from)
	if err != nil {
		return nil, err
	}
	byCustomer := map[uuid.UUID][]eventstore.PeriodQuantity{}
	var prints []string
	for _, r := range rows {
		c := r.CustomerID.UUID()
		if r.PeriodStartsAt.Before(m.since[c]) {
			continue
		}
		byCustomer[c] = append(byCustomer[c], r)
		prints = append(prints, r.Print)
	}
	cards, err := loadPrints(ctx, q, merchantID, prints)
	if err != nil {
		return nil, err
	}
	for c, rows := range byCustomer {
		target, err := cards.priceQuantities(cards.known(rows))
		if err != nil {
			return nil, err
		}
		var unposted int64
		for period, lines := range target {
			var price int64
			for _, l := range lines {
				price = addSat(price, l.Amount)
			}
			unposted = addSat(unposted, max(price-m.posted[c][period], 0))
		}
		out[c] = unposted
	}
	return out, nil
}

// ViewCapacities are many customers' capacities in one currency as views
// start from: GetAdmissionCapacity's for each, read without its writes or
// lock, leaving out the except lease's in-flight report. Run them in one snapshot: a
// posting that commits between two reads would otherwise be missed by one
// and counted by the other.
// A customer whose usage terms do not resolve is in failed alone.
func (s *MoneyService) ViewCapacities(ctx context.Context, q *gen.Queries, customers []uuid.UUID, currency string, except *uuid.UUID) (out map[uuid.UUID]AdmissionCapacity, failed map[uuid.UUID]error, err error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, nil, err
	}
	cur := normalizeCurrency(currency)
	balances, unposted, err := s.capacities(ctx, q, mid.UUID(), customers, cur, except)
	if err != nil {
		return nil, nil, err
	}
	out, failed = make(map[uuid.UUID]AdmissionCapacity, len(balances)), map[uuid.UUID]error{}
	for c, capacity := range balances {
		capacity.Held = addSat(capacity.Held, unposted[c])
		if capacity, err = s.usageAdmissionCapacity(ctx, q, identity.CustomerID(c), cur, capacity); err != nil {
			failed[c] = err
			continue
		}
		out[c] = capacity
	}
	return out, failed, nil
}

// capacities reads the customers' balances and holds, and what their usage
// costs beyond what posted: marks and postings, then balances, then the
// event store.
func (s *MoneyService) capacities(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, customers []uuid.UUID, currency string, except *uuid.UUID) (map[uuid.UUID]AdmissionCapacity, map[uuid.UUID]int64, error) {
	marks, err := readUnpostedMarks(ctx, q, merchantID, customers, currency)
	if err != nil {
		return nil, nil, err
	}
	now := s.now()
	rows, err := q.ListViewCapacities(ctx, gen.ListViewCapacitiesParams{MerchantID: merchantID, Currency: currency, AsOf: now,
		LeaseLiveSince: limits.LeaseLiveSince(now), ExceptLease: except, CustomerIds: customers})
	if err != nil {
		return nil, nil, err
	}
	unposted, err := marks.price(ctx, q, s.eventStore(q), merchantID)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[uuid.UUID]AdmissionCapacity, len(rows))
	for _, r := range rows {
		out[r.CustomerID] = AdmissionCapacity{Balance: r.Balance, Held: r.Held, BillingMode: r.BillingMode, CreditLimit: r.CreditLimitAmount, OutstandingOwed: r.OutstandingOwed}
	}
	return out, unposted, nil
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
// one (ledger.WatchAvailable), and a usage order where what the customer
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
		balances, unposted, err := s.capacities(ctx, q, merchantID, customers, currency, nil)
		if err != nil {
			return err
		}
		for _, w := range watches {
			c := balances[w.CustomerID]
			if err := ledger.WatchAvailable(ctx, q, w, c.Balance-c.Held-unposted[w.CustomerID], now); err != nil {
				return err
			}
		}
		if billed == nil {
			continue
		}
		for _, c := range customers {
			if u := unposted[c]; u > 0 {
				if err := s.requestUsageBilling(ctx, q, c, currency, false, max(u-max(balances[c].Balance, 0), 0)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
