package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money/credits"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/retention"
)

// Usage posts to the ledger at each month's end in the merchant's
// accounting zone (the close) and before any other movement of the
// customer's balance in its currency: a top-up or staff grant, a credit
// lot's expiry, revocation or refund, a usage order and an agreement's
// revocation. A posting is cut at an instant: per pricing period, the price
// of the period's deduplicated usage that occurred before the cut, less what
// the period's earlier postings took. It draws the balance first, one
// credit_spend per lot live just before the cut, and leaves the rest owed in
// one owed_accrual split by meter, at (usage_posting, usage_posting:<id>, 1).
// A period whose price fell below what posted (a volume tier) carries the
// difference until its late usage window closes, then credits it back.

const usagePostingSourcePrefix = "usage_posting:"

// Posting reasons (usage_postings.reason).
const (
	usagePostingClose    = "close"
	usagePostingMovement = "movement"
	usagePostingReversal = "reversal"
)

// zones caches loaded zones by name.
var zones sync.Map

// accountingZone is the zone the merchant's months close in: its
// accounting.timezone, UTC when it names none or no configuration is bound.
// A configuration it cannot read is an error, never UTC.
func (s *MoneyService) accountingZone(ctx context.Context) (*time.Location, error) {
	cfg, _, err := merchantconfig.NewStore(s.db).Get(ctx)
	switch {
	case errors.Is(err, merchantconfig.ErrNoSettings):
		return time.UTC, nil
	case err != nil:
		return nil, err
	case cfg.AccountingTimezone == "" || cfg.AccountingTimezone == "UTC":
		return time.UTC, nil
	}
	if z, ok := zones.Load(cfg.AccountingTimezone); ok {
		return z.(*time.Location), nil
	}
	z, err := time.LoadLocation(cfg.AccountingTimezone)
	if err != nil {
		return nil, err
	}
	zones.Store(cfg.AccountingTimezone, z)
	return z, nil
}

// monthAfter is the start of the month after the one t falls in, in zone z:
// when the close posts usage of t.
func monthAfter(z *time.Location, t time.Time) time.Time {
	l := t.In(z)
	return time.Date(l.Year(), l.Month()+1, 1, 0, 0, 0, 0, z).UTC()
}

// monthStart is the start of the month t falls in, in zone z.
func monthStart(z *time.Location, t time.Time) time.Time {
	l := t.In(z)
	return time.Date(l.Year(), l.Month(), 1, 0, 0, 0, 0, z).UTC()
}

// at is the service on a clock stopped at now.
func (s *MoneyService) at(now time.Time) *MoneyService {
	svc := *s
	svc.clock = clockwork.NewFakeClockAt(now)
	return &svc
}

// eventStore is the event store usage is read from: the service's, else the
// one bound to q's transaction.
func (s *MoneyService) eventStore(q *gen.Queries) *eventstore.Store {
	if s != nil && s.db != nil {
		if store := s.db.EventStore(); store != nil {
			return store
		}
	}
	return db.EventStoreOf(q)
}

var _ db.UsageLedger = (*MoneyService)(nil)

// PostUsage posts at now, in q's transaction and on the caller's clock, what
// a customer's usage in a currency costs before cut and the ledger has not
// taken: before a movement of its balance. It reports whether anything
// posted.
func (s *MoneyService) PostUsage(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, cut, now time.Time) (bool, error) {
	if _, bound := merchant.FromContext(ctx); !bound {
		ctx = merchant.WithID(ctx, billing.MerchantID(merchantID))
	}
	return s.WithDatabase(s.db.InTx(q)).at(now).postUsageTx(ctx, q, merchantID, customer, currency, cut, usagePostingMovement)
}

// UnpostedUsage is what a customer's usage in a currency costs beyond what
// its postings took, read in q's transaction.
func (s *MoneyService) UnpostedUsage(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string) (int64, error) {
	if _, bound := merchant.FromContext(ctx); !bound {
		ctx = merchant.WithID(ctx, billing.MerchantID(merchantID))
	}
	return s.WithDatabase(s.db.InTx(q)).unpostedUsage(ctx, q, merchantID, customer, currency)
}

// postUsageTx is the one choke point usage reaches the ledger through: the
// close and every movement post here. Under the customer's lock it posts each
// period's price before cut less what the period took, credits a period
// whose price fell once its late window closed, and leaves the customer's
// unposted mark at what is left. A cut before one already posted takes that
// one's: usage before it is posted.
func (s *MoneyService) postUsageTx(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, cut time.Time, reason string) (bool, error) {
	key := gen.GetUsageUnpostedParams{MerchantID: merchantID, CustomerID: customer, Currency: currency}
	if _, err := q.GetUsageUnposted(ctx, key); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	store := s.eventStore(q)
	if store == nil {
		return false, eventstore.Required("posting usage")
	}
	if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: merchantID, ID: customer}); err != nil {
		return false, err
	}
	row, err := q.LockUsageUnposted(ctx, gen.LockUsageUnpostedParams(key))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now := instant(s.now())
	cut = instant(earlier(cut, now))
	postings, err := q.ListUsagePostings(ctx, gen.ListUsagePostingsParams{MerchantID: merchantID, CustomerID: customer, Currency: currency, PeriodStartsAt: row.SinceAt})
	if err != nil {
		return false, err
	}
	for _, p := range postings {
		cut = later(cut, p.CutAt)
	}
	rows, err := store.PostingQuantities(ctx, billing.CustomerID(customer), currency, row.SinceAt, cut)
	if err != nil {
		return false, err
	}
	var prints []string
	for _, r := range rows {
		prints = append(prints, r.Print)
	}
	cards, err := loadPrints(ctx, q, merchantID, prints)
	if err != nil {
		return false, err
	}
	target, err := cards.priceQuantities(rows)
	if err != nil {
		return false, err
	}
	posted := periodLines{}
	for _, p := range postings {
		lines, err := decodePostingLines(p.Lines)
		if err != nil {
			return false, err
		}
		period := p.PeriodStartsAt.UTC()
		if posted[period] == nil {
			posted[period] = map[lineKey]*PostingLine{}
		}
		for _, l := range lines {
			k := lineKey{l.MeterKey, l.Cell, l.Print}
			have := posted[period][k]
			if have == nil {
				have = &PostingLine{MeterKey: l.MeterKey, Cell: l.Cell, Print: l.Print}
				posted[period][k] = have
			}
			have.Requests, have.Quantity = addSat(have.Requests, l.Requests), addSat(have.Quantity, l.Quantity)
			have.Stated, have.Amount = addSat(have.Stated, l.Stated), addSat(have.Amount, l.Amount)
		}
	}
	anchor, err := s.periodAnchorFor(ctx, q, merchantID, customer, currency)
	if err != nil {
		return false, err
	}
	billed, err := q.LatestUsageOrderEndInCurrency(ctx, gen.LatestUsageOrderEndInCurrencyParams{MerchantID: merchantID, CustomerID: customer, Currency: currency})
	if err != nil {
		return false, err
	}
	periods := make([]time.Time, 0, len(target)+len(posted))
	for p := range target {
		periods = append(periods, p)
	}
	for p := range posted {
		if _, ok := target[p]; !ok {
			periods = append(periods, p)
		}
	}
	slices.SortFunc(periods, func(a, b time.Time) int { return a.Compare(b) })
	forgiven := map[time.Time]map[forgivenKey]int64{}
	for _, p := range postings {
		lines, err := decodeForgivenLines(p.ForgivenLines)
		if err != nil {
			return false, fmt.Errorf("usage posting %s: %w", p.ID, err)
		}
		period := p.PeriodStartsAt.UTC()
		for _, l := range lines {
			if forgiven[period] == nil {
				forgiven[period] = map[forgivenKey]int64{}
			}
			forgiven[period][l.key()] += l.Amount
		}
	}
	var since, due time.Time
	keep := func(s, d time.Time) {
		if since.IsZero() || s.Before(since) {
			since = s
		}
		if due.IsZero() || d.Before(due) {
			due = d
		}
	}
	for _, r := range rows {
		if r.Later > 0 {
			zone, err := s.accountingZone(ctx)
			if err != nil {
				return false, err
			}
			keep(r.PeriodStartsAt, monthAfter(zone, cut))
			break
		}
	}
	moved := false
	for _, period := range periods {
		_, end, err := anchor.period(period)
		if err != nil || !end.After(period) {
			end = period.AddDate(0, 1, 0)
		}
		lines, amount := postingDelta(target[period], posted[period])
		switch {
		case amount > 0:
			if err := s.postPeriod(ctx, q, merchantID, customer, currency, period, end, cut, reason, amount, lines, billed); err != nil {
				return moved, err
			}
			moved = true
		case amount < 0 && !now.Before(end.Add(retention.UsageIngestWindow)):
			if err := s.reversePeriod(ctx, q, merchantID, customer, currency, period, end, cut, amount, forgiven[period], lines); err != nil {
				return moved, err
			}
			moved = true
		case amount < 0:
			keep(period, end.Add(retention.UsageIngestWindow))
		}
	}
	if since.IsZero() {
		return moved, q.DeleteUsageUnposted(ctx, gen.DeleteUsageUnpostedParams(key))
	}
	return moved, q.SetUsageUnposted(ctx, gen.SetUsageUnpostedParams{SinceAt: since, DueAt: later(due, since.Add(time.Microsecond)), Now: now,
		MerchantID: merchantID, CustomerID: customer, Currency: currency})
}

// postingDelta is what a period's price adds to its postings, line by line,
// and in all.
func postingDelta(target, posted map[lineKey]*PostingLine) ([]PostingLine, int64) {
	keys := make([]lineKey, 0, len(target)+len(posted))
	for k := range target {
		keys = append(keys, k)
	}
	for k := range posted {
		if _, ok := target[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b lineKey) int {
		if c := strings.Compare(a.meter, b.meter); c != 0 {
			return c
		}
		if c := strings.Compare(a.cell, b.cell); c != 0 {
			return c
		}
		return strings.Compare(a.print, b.print)
	})
	var out []PostingLine
	var total int64
	for _, k := range keys {
		var t, p PostingLine
		if v := target[k]; v != nil {
			t = *v
		}
		if v := posted[k]; v != nil {
			p = *v
		}
		d := PostingLine{MeterKey: k.meter, Cell: k.cell, Print: k.print, Requests: t.Requests - p.Requests, Quantity: t.Quantity - p.Quantity,
			Stated: t.Stated - p.Stated, Amount: t.Amount - p.Amount}
		if d == (PostingLine{MeterKey: k.meter, Cell: k.cell, Print: k.print}) {
			continue
		}
		total = addSat(total, d.Amount)
		out = append(out, d)
	}
	return out, total
}

// postPeriod posts what a period's usage adds: balance first, the rest owed,
// split by meter and dated when the usage occurred but never before the end
// of the customer's last usage order. In a merchant's unit, never owed, what
// the lots live before the cut cannot take is forgiven.
func (s *MoneyService) postPeriod(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, period, end, cut time.Time,
	reason string, amount int64, lines []PostingLine, billed time.Time) error {
	encoded, err := json.Marshal(lines)
	if err != nil {
		return err
	}
	now := instant(s.now())
	posting := *s
	posting.clock = clockwork.NewFakeClockAt(now)
	posting.posting = &runningChargePosting{through: cut}
	var forgiven int64
	var split []byte
	if IsMerchantUnit(currency) {
		covered, err := posting.spendableUnits(ctx, q, merchantID, customer, currency)
		if err != nil {
			return err
		}
		if forgiven = max(amount-covered, 0); forgiven > 0 {
			lines, err := s.forgivenLines(ctx, q, merchantID, customer, currency, period, cut, forgiven)
			if err != nil {
				return err
			}
			if split, err = json.Marshal(lines); err != nil {
				return err
			}
		}
	}
	row, err := q.InsertUsagePosting(ctx, gen.InsertUsagePostingParams{MerchantID: merchantID, CustomerID: customer, Currency: currency,
		PeriodStartsAt: period, PeriodEndsAt: end, CutAt: cut, Reason: reason, Amount: amount, Forgiven: forgiven, ForgivenLines: split, Lines: encoded, Now: now})
	if err != nil {
		return err
	}
	if forgiven == amount {
		return nil
	}
	byMeter := map[string]int64{}
	var meters []string
	for _, l := range lines {
		if l.Amount <= 0 {
			continue
		}
		if _, ok := byMeter[l.MeterKey]; !ok {
			meters = append(meters, l.MeterKey)
		}
		byMeter[l.MeterKey] = addSat(byMeter[l.MeterKey], l.Amount)
	}
	slices.Sort(meters)
	owed := make([]owedShare, 0, len(meters))
	for _, m := range meters {
		owed = append(owed, owedShare{meter: m, weight: byMeter[m]})
	}
	posting.posting = &runningChargePosting{through: cut, owed: owed,
		owedAt: later(earlier(earlier(end, cut).Add(-time.Microsecond), now), billed)}
	key := MustIdempotencyKey(OpUsagePosting, usagePostingSourcePrefix+row.ID.String(), "1")
	payer := identity.CustomerID(customer)
	_, _, applied, err := posting.spendBalanceThenOwedTx(ctx, q, payer, customer.String(), currency, key, amount-forgiven, true)
	if err != nil {
		return err
	}
	if !applied {
		return fmt.Errorf("usage posting %s: the ledger already held it", row.ID)
	}
	return nil
}

// reversePeriod credits what a period posted beyond its price, as a lot that
// never lapses. What the period forgave is taken back first, from its stored
// shares: only what the ledger took is credited.
func (s *MoneyService) reversePeriod(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, period, end, cut time.Time,
	amount int64, forgave map[forgivenKey]int64, lines []PostingLine) error {
	encoded, err := json.Marshal(lines)
	if err != nil {
		return err
	}
	now := instant(s.now())
	var total int64
	for _, v := range forgave {
		total += max(v, 0)
	}
	unforgiven := -min(-amount, total)
	var split []byte
	if unforgiven != 0 {
		if split, err = json.Marshal(takeBackForgiven(forgave, unforgiven)); err != nil {
			return err
		}
	}
	row, err := q.InsertUsagePosting(ctx, gen.InsertUsagePostingParams{MerchantID: merchantID, CustomerID: customer, Currency: currency,
		PeriodStartsAt: period, PeriodEndsAt: end, CutAt: cut, Reason: usagePostingReversal, Amount: amount, Forgiven: unforgiven, ForgivenLines: split, Lines: encoded, Now: now})
	if err != nil {
		return err
	}
	if amount -= unforgiven; amount == 0 {
		return nil
	}
	l := ledger.New(q, merchantID)
	account, err := l.EnsureCustomerBalance(ctx, customer, currency)
	if err != nil {
		return err
	}
	source := row.ID.String()
	lot, err := q.InsertCreditGrant(ctx, gen.InsertCreditGrantParams{MerchantID: merchantID, CustomerID: customer, AccountID: account, Unit: currency,
		Units: -amount, FundingSource: string(credits.Grant), Source: "usage_reversal", SourceID: &source, Invoker: "openrails", StartsAt: now})
	if err != nil {
		return fmt.Errorf("usage reversal %s: record the lot: %w", row.ID, err)
	}
	coord := ledger.Coord{Operation: OpUsagePosting, Source: usagePostingSourcePrefix + row.ID.String(), SourceID: "1"}
	if _, err := l.ReverseUsage(ctx, customer, currency, -amount, coord, lot.ID); err != nil {
		return err
	}
	return ledger.WatchChange(ctx, q, merchantID, customer, currency, true, now)
}

// unpostedUsage is what a customer's usage in a currency costs beyond what
// its postings took: per period, the price of its usage so far less what
// posted, never below zero. A customer with nothing unposted reads nothing
// from the event store.
func (s *MoneyService) unpostedUsage(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string) (int64, error) {
	row, err := q.GetUsageUnposted(ctx, gen.GetUsageUnpostedParams{MerchantID: merchantID, CustomerID: customer, Currency: currency})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	store := s.eventStore(q)
	if store == nil {
		return 0, eventstore.Required("a balance with unposted usage")
	}
	rows, err := store.PeriodQuantities(ctx, []billing.CustomerID{billing.CustomerID(customer)}, currency, row.SinceAt)
	if err != nil {
		return 0, err
	}
	var prints []string
	for _, r := range rows {
		prints = append(prints, r.Print)
	}
	cards, err := loadPrints(ctx, q, merchantID, prints)
	if err != nil {
		return 0, err
	}
	target, err := cards.priceQuantities(cards.known(rows))
	if err != nil {
		return 0, err
	}
	postings, err := q.ListUsagePostings(ctx, gen.ListUsagePostingsParams{MerchantID: merchantID, CustomerID: customer, Currency: currency, PeriodStartsAt: row.SinceAt})
	if err != nil {
		return 0, err
	}
	posted := map[time.Time]int64{}
	for _, p := range postings {
		posted[p.PeriodStartsAt.UTC()] = addSat(posted[p.PeriodStartsAt.UTC()], p.Amount)
	}
	var unposted int64
	for period, lines := range target {
		var price int64
		for _, l := range lines {
			price = addSat(price, l.Amount)
		}
		unposted = addSat(unposted, max(price-posted[period], 0))
	}
	return unposted, nil
}

// withUnposted adds a customer's unposted usage to what its capacity holds.
func (s *MoneyService) withUnposted(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, c AdmissionCapacity) (AdmissionCapacity, error) {
	unposted, err := s.unpostedUsage(ctx, q, merchantID, customer, currency)
	c.Held = addSat(c.Held, unposted)
	return c, err
}

// CloseUsage posts, in its own transaction, a customer's usage in a currency
// before the start of now's month: the month-end close. It also credits a
// period whose price fell once its late window closed.
func (s *MoneyService) CloseUsage(ctx context.Context, customer uuid.UUID, currency string, now time.Time) (bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	zone, err := s.accountingZone(ctx)
	if err != nil {
		return false, err
	}
	var posted bool
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		posted, err = s.WithDatabase(s.db.NewWithPgxTx(tx)).at(now).postUsageTx(ctx, gen.New(tx), mid.UUID(), customer, currency, monthStart(zone, now), usagePostingClose)
		return err
	})
	return posted, err
}

// postUsageNow posts what a customer's usage costs through now, in the
// caller's transaction: before a movement of its balance.
func (s *MoneyService) postUsageNow(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string) (bool, error) {
	return s.postUsageTx(ctx, q, merchantID, customer, currency, s.now(), usagePostingMovement)
}

// PostUsageNow posts at now, in its own transaction, what a customer's usage
// in a currency costs: before its agreement is revoked.
func (s *MoneyService) PostUsageNow(ctx context.Context, customer uuid.UUID, currency string, now time.Time) (bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	var posted bool
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		posted, err = s.WithDatabase(s.db.NewWithPgxTx(tx)).at(now).postUsageNow(ctx, gen.New(tx), mid.UUID(), customer, currency)
		return err
	})
	return posted, err
}

// usageDeductions are the postings the transfers that took usage belong to,
// by transfer.
func usageDeductions(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, transfers []gen.BillingLedgerTransfer) (map[uuid.UUID]*billing.UsageDeduction, error) {
	ids := map[uuid.UUID][]uuid.UUID{}
	var postings []uuid.UUID
	for _, t := range transfers {
		if t.Operation != string(OpUsagePosting) || !strings.HasPrefix(t.Source, usagePostingSourcePrefix) {
			continue
		}
		id, err := uuid.Parse(strings.TrimPrefix(t.Source, usagePostingSourcePrefix))
		if err != nil {
			continue
		}
		if _, ok := ids[id]; !ok {
			postings = append(postings, id)
		}
		ids[id] = append(ids[id], t.ID)
	}
	out := map[uuid.UUID]*billing.UsageDeduction{}
	if len(postings) == 0 {
		return out, nil
	}
	rows, err := q.ListUsagePostingsByIDs(ctx, gen.ListUsagePostingsByIDsParams{MerchantID: merchantID, CustomerID: customer, Ids: postings})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		lines, err := decodePostingLines(row.Lines)
		if err != nil {
			return nil, err
		}
		d := &billing.UsageDeduction{PeriodStartsAt: row.PeriodStartsAt.UTC(), PeriodEndsAt: row.PeriodEndsAt.UTC(), Through: row.CutAt.UTC(),
			Amount: row.Amount, Lines: []billing.UsageDeductionLine{}}
		type cell struct{ meter, cell string }
		var order []cell
		sums := map[cell]*billing.UsageDeductionLine{}
		for _, l := range lines {
			k := cell{l.MeterKey, l.Cell}
			line := sums[k]
			if line == nil {
				line = &billing.UsageDeductionLine{MeterKey: l.MeterKey}
				if l.Cell != "" {
					c := l.Cell
					line.Cell = &c
				}
				sums[k] = line
				order = append(order, k)
			}
			line.Requests, line.Quantity, line.Amount = addSat(line.Requests, l.Requests), addSat(line.Quantity, l.Quantity), addSat(line.Amount, l.Amount)
		}
		for _, k := range order {
			d.Lines = append(d.Lines, *sums[k])
		}
		for _, t := range ids[row.ID] {
			out[t] = d
		}
	}
	return out, nil
}
