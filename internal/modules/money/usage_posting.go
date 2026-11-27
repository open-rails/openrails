package money

import (
	"cmp"
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
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/modules/money/credits"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/retention"
)

// Usage reaches the ledger in postings; until then a customer's balance
// deducts its provisional price (the unposted running totals, usage_totals).
// A posting takes every settled usage, except one before a credit lot's
// lapse, which takes the lapse segments before it. Per pricing period it
// prices the period's settled quantity (posted and taken), card by card,
// less what the period's earlier usage receipts took, and writes one usage
// receipt itemized by meter with one or more ledger transfers: the lots
// first, one credit_spend each, and the rest owed in one owed_accrual split
// by meter, at (usage_posting, usage_receipt:<id>, 1). A period whose price
// fell (a volume tier) is credited at once, as a reversal receipt. The
// totals it took fold into the period's posted row.
//
// Postings run once the accounting-zone day of a customer's oldest pending
// usage ends (the billing pass's first scan), before a usage invoice bills
// what is owed, before a credit lot's expiry, revocation or refund (so lots
// are spent in time order), and before a merchant purge deletes the store.

const usageReceiptSourcePrefix = "usage_receipt:"

// Posting reasons (usage_receipts.reason).
const (
	usagePostingDaily    = "daily"
	usagePostingBilling  = "billing"
	usagePostingReversal = "reversal"
)

// zones caches loaded zones by name.
var zones sync.Map

// AccountingZone is the zone the merchant's months close in: its
// accounting.timezone, UTC when it names none or no configuration is bound.
// A configuration it cannot read is an error, never UTC.
func (s *MoneyService) AccountingZone(ctx context.Context) (*time.Location, error) {
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

// dayAfter is the start of the day after the one t falls in, in zone z:
// when the daily posting takes usage of t.
func dayAfter(z *time.Location, t time.Time) time.Time {
	l := t.In(z)
	return time.Date(l.Year(), l.Month(), l.Day()+1, 0, 0, 0, 0, z).UTC()
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

// PostUsage posts, in q's transaction and at now, what a customer's usage in
// a currency costs and the ledger has not taken: everything with a zero cut,
// else what occurred before cut (a credit lot's lapse). reason is
// usage_receipts.reason: credit before a credit lot moves, purge before a
// merchant purge.
func (s *MoneyService) PostUsage(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, cut, now time.Time, reason string) (bool, error) {
	if _, bound := merchant.FromContext(ctx); !bound {
		ctx = merchant.WithID(ctx, billing.MerchantID(merchantID))
	}
	return s.WithDatabase(s.db.InTx(q)).at(now).postUsageTx(ctx, q, merchantID, customer, currency, cut, reason)
}

// postUsageTx is the one choke point usage reaches the ledger through. Under
// the customer's lock and its queue row's (every intake of the customer
// finished, and later ones wait), it prices each period it takes usage of
// from the running totals, posts what that adds to the period's receipts,
// folds the totals it took into the posted rows and leaves the queue row due
// for what is left. A zero cut takes everything; a cut before now (a lot's
// lapse) takes the lapse segments before it, in the periods that began
// before it.
func (s *MoneyService) postUsageTx(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, cut time.Time, reason string) (bool, error) {
	key := gen.GetUsagePendingParams{MerchantID: merchantID, CustomerID: customer, Currency: currency}
	if _, err := q.GetUsagePending(ctx, key); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: merchantID, ID: customer}); err != nil {
		return false, err
	}
	if _, err := q.LockUsagePending(ctx, gen.LockUsagePendingParams(key)); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	now := instant(s.now())
	everything := cut.IsZero()
	if horizon := now.Add(retention.UsageClockSkew); everything || cut.After(horizon) {
		cut, everything = horizon, true
	}
	cut = instant(cut)
	// Lots live just before the cut are drawn, and none past now.
	liveAt := earlier(cut, now)
	totals, err := q.ListUsageTotals(ctx, gen.ListUsageTotalsParams(key))
	if err != nil {
		return false, err
	}
	taken := periodTotals{}
	ends := map[time.Time]time.Time{}
	left := false
	for _, t := range totals {
		period := t.PeriodStartsAt.UTC()
		switch {
		case t.LeaseID != uuid.Nil && !everything && !(t.LapseAt.Before(cut) && period.Before(cut)):
			left = true
			continue
		case t.LeaseID != uuid.Nil:
			ends[period] = t.PeriodEndsAt.UTC()
		}
		taken.add(period, t)
	}
	var periods []time.Time
	for p := range ends {
		periods = append(periods, p)
	}
	slices.SortFunc(periods, func(a, b time.Time) int { return a.Compare(b) })
	moved := false
	if len(periods) > 0 {
		receipts, err := q.ListUsageReceipts(ctx, gen.ListUsageReceiptsParams{MerchantID: merchantID, CustomerID: customer, Currency: currency, Periods: periods})
		if err != nil {
			return false, err
		}
		posted, forgiven, err := receiptLines(receipts)
		if err != nil {
			return false, err
		}
		var prints []string
		for _, p := range periods {
			for k := range taken[p] {
				prints = append(prints, k.print)
			}
		}
		cards, err := loadPrints(ctx, q, merchantID, prints)
		if err != nil {
			return false, err
		}
		billed, err := q.LatestUsageInvoiceEndInCurrency(ctx, gen.LatestUsageInvoiceEndInCurrencyParams{MerchantID: merchantID, CustomerID: customer, Currency: currency})
		if err != nil {
			return false, err
		}
		for _, period := range periods {
			target, err := cards.priceQuantities(taken.quantities(period))
			if err != nil {
				return moved, err
			}
			lines, amount := postingDelta(target[period], posted[period])
			switch {
			case amount > 0:
				err = s.postPeriod(ctx, q, merchantID, customer, currency, period, ends[period], cut, liveAt, reason, amount, lines, billed)
			case amount < 0:
				err = s.reversePeriod(ctx, q, merchantID, customer, currency, period, ends[period], cut, amount, forgiven[period], lines)
			}
			if err != nil {
				return moved, err
			}
			moved = moved || amount != 0
		}
	}
	fold := gen.FoldUsageTotalsParams{MerchantID: merchantID, CustomerID: customer, Currency: currency, Now: now}
	if !everything {
		fold.Cut = &cut
	}
	if err := q.FoldUsageTotals(ctx, fold); err != nil {
		return moved, err
	}
	if !left {
		return moved, q.DeleteUsagePending(ctx, gen.DeleteUsagePendingParams(key))
	}
	// What is left occurred at or after the cut.
	zone, err := s.AccountingZone(ctx)
	if err != nil {
		return moved, err
	}
	return moved, q.SetUsagePendingDue(ctx, gen.SetUsagePendingDueParams{DueAt: dayAfter(zone, liveAt), Now: now,
		MerchantID: merchantID, CustomerID: customer, Currency: currency})
}

// periodTotals are a customer's running totals a posting prices, by period,
// meter cell and print.
type periodTotals map[time.Time]map[lineKey]*periodQuantity

func (p periodTotals) add(period time.Time, t gen.BillingUsageTotal) {
	if p[period] == nil {
		p[period] = map[lineKey]*periodQuantity{}
	}
	k := lineKey{t.MeterKey, t.Cell, t.Print}
	q := p[period][k]
	if q == nil {
		q = &periodQuantity{period: period, meter: t.MeterKey, cell: t.Cell, print: t.Print}
		p[period][k] = q
	}
	q.quantity, q.stated, q.requests = addSat(q.quantity, t.Quantity), addSat(q.stated, t.Stated), addSat(q.requests, t.Requests)
}

// quantities are one period's totals, in line order.
func (p periodTotals) quantities(period time.Time) []periodQuantity {
	out := make([]periodQuantity, 0, len(p[period]))
	for _, q := range p[period] {
		out = append(out, *q)
	}
	slices.SortFunc(out, func(a, b periodQuantity) int {
		return cmp.Or(strings.Compare(a.meter, b.meter), strings.Compare(a.cell, b.cell), strings.Compare(a.print, b.print))
	})
	return out
}

// receiptLines are what each period's receipts took, line by line, and what
// they forgave, by share.
func receiptLines(receipts []gen.BillingUsageReceipt) (periodLines, map[time.Time]map[forgivenKey]int64, error) {
	posted := periodLines{}
	forgiven := map[time.Time]map[forgivenKey]int64{}
	for _, r := range receipts {
		period := r.PeriodStartsAt.UTC()
		lines, err := decodePostingLines(r.Lines)
		if err != nil {
			return nil, nil, fmt.Errorf("usage receipt %s: %w", r.ID, err)
		}
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
		shares, err := decodeForgivenLines(r.ForgivenLines)
		if err != nil {
			return nil, nil, fmt.Errorf("usage receipt %s: %w", r.ID, err)
		}
		for _, l := range shares {
			if forgiven[period] == nil {
				forgiven[period] = map[forgivenKey]int64{}
			}
			forgiven[period][l.key()] += l.Amount
		}
	}
	return posted, forgiven, nil
}

// postingDelta is what a period's price adds to its receipts, line by line,
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
// of the customer's last usage invoice. It draws the lots live just before
// liveAt; in a merchant's unit, never owed, what they cannot take is forgiven.
func (s *MoneyService) postPeriod(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, period, end, cut, liveAt time.Time,
	reason string, amount int64, lines []PostingLine, billed time.Time) error {
	encoded, err := json.Marshal(lines)
	if err != nil {
		return err
	}
	now := instant(s.now())
	posting := *s
	posting.clock = clockwork.NewFakeClockAt(now)
	posting.posting = &runningChargePosting{through: liveAt}
	var forgiven int64
	var split []byte
	if IsMerchantUnit(currency) {
		covered, err := posting.spendableUnits(ctx, q, merchantID, customer, currency)
		if err != nil {
			return err
		}
		if forgiven = max(amount-covered, 0); forgiven > 0 {
			lines, err := s.forgivenLines(ctx, q, customer, currency, period, cut, forgiven)
			if err != nil {
				return err
			}
			if split, err = json.Marshal(lines); err != nil {
				return err
			}
		}
	}
	row, err := q.InsertUsageReceipt(ctx, gen.InsertUsageReceiptParams{MerchantID: merchantID, CustomerID: customer, Currency: currency,
		PeriodStartsAt: period, PeriodEndsAt: end, CutAt: cut, Reason: reason, Amount: amount, Forgiven: forgiven, ForgivenLines: split, Lines: encoded,
		Now: now})
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
	posting.posting = &runningChargePosting{through: liveAt, owed: owed, from: period,
		owedAt: later(earlier(earlier(end, cut).Add(-time.Microsecond), now), billed)}
	key := MustIdempotencyKey(OpUsagePosting, usageReceiptSourcePrefix+row.ID.String(), "1")
	payer := identity.CustomerID(customer)
	_, _, applied, err := posting.spendBalanceThenOwedTx(ctx, q, payer, customer.String(), currency, key, amount-forgiven, true)
	if err != nil {
		return err
	}
	if !applied {
		return fmt.Errorf("usage receipt %s: the ledger already held it", row.ID)
	}
	return nil
}

// reversePeriod credits what a period's receipts took beyond its price (a
// volume tier's fall), as a lot that never lapses. What the period forgave is
// taken back first, from its stored shares: only what the ledger took is
// credited.
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
	row, err := q.InsertUsageReceipt(ctx, gen.InsertUsageReceiptParams{MerchantID: merchantID, CustomerID: customer, Currency: currency,
		PeriodStartsAt: period, PeriodEndsAt: end, CutAt: cut, Reason: usagePostingReversal, Amount: amount, Forgiven: unforgiven, ForgivenLines: split,
		Lines: encoded, Now: now})
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
	coord := ledger.Coord{Operation: OpUsagePosting, Source: usageReceiptSourcePrefix + row.ID.String(), SourceID: "1"}
	if _, err := l.ReverseUsage(ctx, customer, currency, -amount, coord, lot.ID); err != nil {
		return err
	}
	// The revenue it took back is the lot's, deferred at face.
	if lot, err = q.GetCreditLot(ctx, gen.GetCreditLotParams{MerchantID: merchantID, ID: lot.ID}); err != nil {
		return err
	}
	if err := books.New(q, merchantID).ReverseUsage(ctx, books.CreditLotOf(lot), coord, now); err != nil {
		return err
	}
	return ledger.WatchChange(ctx, q, merchantID, customer, currency, true, now)
}

// PostDueUsage posts, in its own transaction, everything a customer's usage
// in a currency costs and the ledger has not taken: the daily posting, once
// the accounting-zone day of its oldest pending usage ended.
func (s *MoneyService) PostDueUsage(ctx context.Context, customer uuid.UUID, currency string, now time.Time) (bool, error) {
	return s.postUsageOwnTx(ctx, customer, currency, now, usagePostingDaily)
}

// PostUsageNow posts at now, in its own transaction, everything a customer's
// usage in a currency costs: before its agreement ends and its owed is
// billed.
func (s *MoneyService) PostUsageNow(ctx context.Context, customer uuid.UUID, currency string, now time.Time) (bool, error) {
	return s.postUsageOwnTx(ctx, customer, currency, now, usagePostingBilling)
}

func (s *MoneyService) postUsageOwnTx(ctx context.Context, customer uuid.UUID, currency string, now time.Time, reason string) (bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	var posted bool
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		posted, err = s.WithDatabase(s.db.NewWithPgxTx(tx)).at(now).postEverything(ctx, gen.New(tx), mid.UUID(), customer, currency, reason)
		return err
	})
	return posted, err
}

// postEverything posts all of a customer's usage in a currency, in q's
// transaction: first the lots that lapsed expire, each once the usage and
// running charges before its lapse posted, so usage is spent in time order.
func (s *MoneyService) postEverything(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency, reason string) (bool, error) {
	if _, err := s.creditLots(q, merchantID).ExpireLapsedBy(ctx, customer, currency, s.now()); err != nil {
		return false, err
	}
	return s.postUsageTx(ctx, q, merchantID, customer, currency, time.Time{}, reason)
}

// usageDeductions are the usage receipts the transfers that took usage
// belong to, by transfer.
func usageDeductions(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, transfers []gen.BillingLedgerTransfer) (map[uuid.UUID]*billing.UsageDeduction, error) {
	ids := map[uuid.UUID][]uuid.UUID{}
	var receipts []uuid.UUID
	for _, t := range transfers {
		if t.Operation != string(OpUsagePosting) || !strings.HasPrefix(t.Source, usageReceiptSourcePrefix) {
			continue
		}
		id, err := uuid.Parse(strings.TrimPrefix(t.Source, usageReceiptSourcePrefix))
		if err != nil {
			continue
		}
		if _, ok := ids[id]; !ok {
			receipts = append(receipts, id)
		}
		ids[id] = append(ids[id], t.ID)
	}
	out := map[uuid.UUID]*billing.UsageDeduction{}
	if len(receipts) == 0 {
		return out, nil
	}
	rows, err := q.ListUsageReceiptsByIDs(ctx, gen.ListUsageReceiptsByIDsParams{MerchantID: merchantID, CustomerID: customer, Ids: receipts})
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
