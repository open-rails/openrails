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

// Usage and running charges reach the ledger in postings; until then a
// customer's balance deducts usage's provisional price (the unposted running
// totals, usage_totals) and what its running charges accrued. A posting
// writes one usage receipt per customer and currency listing all it took:
// per pricing period, meter cell and card print, the price of the period's
// settled quantity (posted and taken) less the price of what posted before,
// card by card; and each running charge from its last posting through the
// posting's instant. The periods whose price rose and the running charges
// are spent together: the lots first, one credit_spend each, the rest owed
// in one owed_accrual split by meter, at (usage_posting, usage_receipt:<id>,
// 1). A period whose price fell (a volume tier) is credited at the same
// coordinate, as a lot. The totals it took fold into each period's posted
// row.
//
// The daily posting runs once the accounting-zone day of a customer's oldest
// pending usage or running charge ends (the billing pass's first scan) and
// posts running charges through that zone's last midnight. Postings also run
// before a usage invoice bills what is owed, before a credit lot's expiry,
// revocation or refund (so lots are spent in time order), and before a
// merchant purge.

const usageReceiptSourcePrefix = "usage_receipt:"

// Posting reasons (usage_receipts.reason).
const (
	usagePostingDaily   = "daily"
	usagePostingBilling = "billing"
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
// when the daily posting takes usage of t, or a running charge posted
// through t.
func dayAfter(z *time.Location, t time.Time) time.Time {
	l := t.In(z)
	return time.Date(l.Year(), l.Month(), l.Day()+1, 0, 0, 0, 0, z).UTC()
}

// dayStart is the start of the day t falls in, in zone z: what the daily
// posting posts running charges through.
func dayStart(z *time.Location, t time.Time) time.Time {
	l := t.In(z)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, z).UTC()
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

// PostUsage posts, in q's transaction and at now, what a customer's usage
// and running charges in a currency cost and the ledger has not taken: the
// usage that occurred before cut (all of it with a zero cut) and the running
// charges through through (none with a zero through). reason is
// usage_receipts.reason: credit before a credit lot moves, purge before a
// merchant purge.
func (s *MoneyService) PostUsage(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, cut, through, now time.Time, reason string) (bool, error) {
	if _, bound := merchant.FromContext(ctx); !bound {
		ctx = merchant.WithID(ctx, billing.MerchantID(merchantID))
	}
	return s.WithDatabase(s.db.InTx(q)).at(now).postUsageTx(ctx, q, merchantID, customer, currency, cut, through, reason)
}

// postUsageTx is the one choke point usage and running charges reach the
// ledger through. Under the customer's lock and its queue row's (every
// intake of the customer finished, and later ones wait), it prices each
// period it takes usage of from the running totals, posts each running
// charge through through (no later than its stop), writes one receipt of
// both, folds the totals it took into the posted rows and leaves the queue
// row due for what is left. A zero cut takes all usage; a cut before now (a
// lot's lapse) takes the lapse segments before it, in the periods that began
// before it.
func (s *MoneyService) postUsageTx(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, cut, through time.Time, reason string) (bool, error) {
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
	// The merchant's configuration is read before the ledger moves: a first
	// read loads it on its own connection, which opens the general ledger's
	// accounts and must not wait for this transaction's.
	zone, err := s.AccountingZone(ctx)
	if err != nil {
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
	if !through.IsZero() {
		through = instant(earlier(through, now))
	}
	r := &receipt{cut: cut, liveAt: liveAt, through: through}

	totals, err := q.ListUsageTotals(ctx, gen.ListUsageTotalsParams(key))
	if err != nil {
		return false, err
	}
	posted, taken := periodTotals{}, periodTotals{}
	ends := map[time.Time]time.Time{}
	left := false
	for _, t := range totals {
		period := t.PeriodStartsAt.UTC()
		switch {
		case t.LeaseID == uuid.Nil:
			posted.add(period, t)
		case !everything && !(t.LapseAt.Before(cut) && period.Before(cut)):
			left = true
			continue
		default:
			ends[period] = t.PeriodEndsAt.UTC()
		}
		taken.add(period, t)
	}
	if len(ends) > 0 {
		var prints []string
		for p := range ends {
			for k := range taken[p] {
				prints = append(prints, k.print)
			}
		}
		cards, err := loadPrints(ctx, q, merchantID, prints)
		if err != nil {
			return false, err
		}
		if err := r.priceUsage(cards, posted, taken, ends); err != nil {
			return false, err
		}
	}

	// Running charges move first: a balance read no longer counts what the
	// receipt takes of them.
	charges, err := q.ListUnpostedRunningCharges(ctx, gen.ListUnpostedRunningChargesParams(key))
	if err != nil {
		return false, err
	}
	var chargesDue time.Time
	for _, c := range charges {
		if c, err = s.takeRunningCharge(ctx, q, r, c); err != nil {
			return false, err
		}
		if c.StoppedAt == nil || c.PostedThroughAt.Before(*c.StoppedAt) {
			if due := dayAfter(zone, c.PostedThroughAt); chargesDue.IsZero() || due.Before(chargesDue) {
				chargesDue = due
			}
		}
	}

	moved := false
	if len(r.lines) > 0 || len(r.charges) > 0 {
		if err := s.postReceipt(ctx, q, merchantID, customer, currency, reason, r); err != nil {
			return false, err
		}
		moved = true
	}
	fold := gen.FoldUsageTotalsParams{MerchantID: merchantID, CustomerID: customer, Currency: currency, Now: now}
	if !everything {
		fold.Cut = &cut
	}
	if err := q.FoldUsageTotals(ctx, fold); err != nil {
		return moved, err
	}
	// What is left is usage at or after the cut and running charges past
	// their posting: due at the end of the day each reached.
	due := chargesDue
	if usageDue := dayAfter(zone, liveAt); left && (due.IsZero() || usageDue.Before(due)) {
		due = usageDue
	}
	if due.IsZero() {
		return moved, q.DeleteUsagePending(ctx, gen.DeleteUsagePendingParams(key))
	}
	return moved, q.SetUsagePendingDue(ctx, gen.SetUsagePendingDueParams{DueAt: due, Now: now,
		MerchantID: merchantID, CustomerID: customer, Currency: currency})
}

// takeRunningCharge moves c's posting to the receipt's instant, no later
// than its stop, and lists what that adds. A zero instant, or one not past
// its last posting, leaves it. The row moves before the ledger does, so the
// accrual read from it no longer counts what is being posted.
func (s *MoneyService) takeRunningCharge(ctx context.Context, q *gen.Queries, r *receipt, c gen.BillingRunningCharge) (gen.BillingRunningCharge, error) {
	end := r.through
	if c.StoppedAt != nil && c.StoppedAt.Before(end) {
		end = c.StoppedAt.UTC()
	}
	if end.IsZero() || !end.After(c.PostedThroughAt) {
		return c, nil
	}
	total, err := RunningChargeTotal(c.RatePerHour, c.StartedAt, end)
	if err != nil {
		return c, err
	}
	amount := total - c.PostedAmount
	if amount < 0 {
		return c, fmt.Errorf("running charge %s: posting through %s is behind its posted amount", c.ID, end)
	}
	row, err := q.PostRunningCharge(ctx, gen.PostRunningChargeParams{MerchantID: c.MerchantID, ID: c.ID, Through: end, PostedAmount: total, Now: instant(s.now())})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, fmt.Errorf("running charge %s changed under the customer lock", c.ID)
	}
	if err != nil {
		return c, err
	}
	if amount > 0 {
		r.charges = append(r.charges, receiptCharge{RunningChargeID: c.ID, StartsAt: c.PostedThroughAt.UTC(), EndsAt: end, Amount: amount})
	}
	return row, nil
}

// receipt is what one posting takes, before it is written: its usage lines
// in period order, each period's net, and its running charges.
type receipt struct {
	// cut bounds the usage taken, liveAt the lots drawn; through is the
	// running charges' instant, zero when none post.
	cut, liveAt, through time.Time
	lines                []receiptLine
	periods              []periodNet
	charges              []receiptCharge
}

// periodNet is one period's lines on a receipt: lines[first:last], summing
// to amount.
type periodNet struct {
	start       time.Time
	first, last int
	amount      int64
}

// receiptLine is one usage line of a receipt (usage_receipts.lines): what a
// posting added for one meter cell under one card print in a pricing period,
// and of it what a merchant's unit forgave (negative: took back).
type receiptLine struct {
	PeriodStartsAt time.Time `json:"period_starts_at"`
	PeriodEndsAt   time.Time `json:"period_ends_at"`
	PostingLine
	Forgiven int64 `json:"forgiven"`
}

// receiptCharge is one running charge of a receipt (usage_receipts.charges):
// what it accrued from its last posting to EndsAt.
type receiptCharge struct {
	RunningChargeID uuid.UUID `json:"running_charge_id"`
	StartsAt        time.Time `json:"starts_at"`
	EndsAt          time.Time `json:"ends_at"`
	Amount          int64     `json:"amount"`
}

// priceUsage lists, per period with usage taken, what the price of its
// settled total adds to the price of what posted before: what its earlier
// receipts took, which sum to that price.
func (r *receipt) priceUsage(cards usagePrints, posted, taken periodTotals, ends map[time.Time]time.Time) error {
	periods := make([]time.Time, 0, len(ends))
	for p := range ends {
		periods = append(periods, p)
	}
	slices.SortFunc(periods, func(a, b time.Time) int { return a.Compare(b) })
	for _, period := range periods {
		target, err := cards.priceQuantities(taken.quantities(period))
		if err != nil {
			return err
		}
		before, err := cards.priceQuantities(posted.quantities(period))
		if err != nil {
			return err
		}
		lines, amount := postingDelta(target[period], before[period])
		if len(lines) == 0 {
			continue
		}
		net := periodNet{start: period, first: len(r.lines), amount: amount}
		for _, l := range lines {
			r.lines = append(r.lines, receiptLine{PeriodStartsAt: period, PeriodEndsAt: ends[period], PostingLine: l})
		}
		net.last = len(r.lines)
		r.periods = append(r.periods, net)
	}
	return nil
}

// spent is what the receipt spends: the periods whose price rose and the
// running charges. credited is what it credits: the periods whose price fell.
func (r *receipt) spent() (spent, credited int64) {
	for _, p := range r.periods {
		if p.amount > 0 {
			spent = addSat(spent, p.amount)
		} else {
			credited = addSat(credited, -p.amount)
		}
	}
	for _, c := range r.charges {
		spent = addSat(spent, c.Amount)
	}
	return spent, credited
}

// forgive shares what a merchant's unit could not take across the periods
// whose price rose, by their amounts, and each period's share across its
// positive lines, to the largest remainder.
func (r *receipt) forgive(amount int64) {
	var rose []int
	var weights []int64
	for i, p := range r.periods {
		if p.amount > 0 {
			rose, weights = append(rose, i), append(weights, p.amount)
		}
	}
	for i, part := range largestRemainder(amount, weights) {
		r.shareForgiven(r.periods[rose[i]], part)
	}
}

// settle decides what the receipt moves, falls first: a falling period's
// lot is spendable by the rises. In a merchant's unit a falling period takes
// back what it forgave before, the rest of the fall is credited, and the
// rises are spent from the lots and that credit, forgiving what neither
// holds; spendable is what the lots hold, forgave each falling period's
// earlier forgiveness. Elsewhere nothing is forgiven.
func (r *receipt) settle(unit bool, spendable int64, forgave map[time.Time]int64) (spend, credit int64) {
	spend, credit = r.spent()
	if !unit {
		return spend, credit
	}
	credit -= r.takeBack(forgave)
	forgiven := max(spend-addSat(spendable, credit), 0)
	r.forgive(forgiven)
	return spend - forgiven, credit
}

// takeBack takes back of each period whose price fell what it forgave before,
// at most its fall, from its negative lines; and answers what it took.
func (r *receipt) takeBack(forgave map[time.Time]int64) int64 {
	var total int64
	for _, p := range r.periods {
		if p.amount >= 0 {
			continue
		}
		back := min(-p.amount, max(forgave[p.start], 0))
		r.shareForgiven(p, -back)
		total = addSat(total, back)
	}
	return total
}

// shareForgiven shares amount (either sign) across a period's lines of its
// sign, by their amounts, to the largest remainder.
func (r *receipt) shareForgiven(p periodNet, amount int64) {
	sign := int64(1)
	if amount < 0 {
		sign = -1
	}
	var idx []int
	var weights []int64
	for i := p.first; i < p.last; i++ {
		if a := r.lines[i].Amount * sign; a > 0 {
			idx, weights = append(idx, i), append(weights, a)
		}
	}
	for i, part := range largestRemainder(amount*sign, weights) {
		r.lines[idx[i]].Forgiven += part * sign
	}
}

// postReceipt writes the receipt and moves the ledger: first what it credits,
// the periods whose price fell, as a lot; then what it spends, the lots first
// (that one included) and the rest owed, split by period and meter, each
// dated before its period's end and when its usage was taken, and by running
// charges, dated before the posting's instant, never before the end of the
// customer's last usage invoice. In a merchant's unit, never owed, a period
// whose price fell takes back what it forgave before it credits, and what
// the lots cannot take is forgiven.
func (s *MoneyService) postReceipt(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency, reason string, r *receipt) error {
	now := instant(s.now())
	posting := *s
	posting.clock = clockwork.NewFakeClockAt(now)
	accrued := r.through
	if accrued.IsZero() {
		accrued = r.liveAt
	}
	posting.posting = &ledgerPosting{accruedAsOf: accrued, liveAt: r.liveAt}
	unit := IsMerchantUnit(currency)
	var spendable int64
	var forgave map[time.Time]int64
	if unit {
		var err error
		if spendable, err = posting.spendableUnits(ctx, q, merchantID, customer, currency); err != nil {
			return err
		}
		if _, falls := r.spent(); falls > 0 {
			if forgave, err = periodForgiveness(ctx, q, merchantID, customer, currency, r); err != nil {
				return err
			}
		}
	}
	spend, credit := r.settle(unit, spendable, forgave)
	lines, err := json.Marshal(append([]receiptLine{}, r.lines...))
	if err != nil {
		return err
	}
	charges, err := json.Marshal(append([]receiptCharge{}, r.charges...))
	if err != nil {
		return err
	}
	var amount, forgiven int64
	for _, l := range r.lines {
		amount, forgiven = addSat(amount, l.Amount), addSat(forgiven, l.Forgiven)
	}
	for _, c := range r.charges {
		amount = addSat(amount, c.Amount)
	}
	row, err := q.InsertUsageReceipt(ctx, gen.InsertUsageReceiptParams{MerchantID: merchantID, CustomerID: customer, Currency: currency,
		CutAt: r.cut, Reason: reason, Amount: amount, Lines: lines, Charges: charges, Forgiven: forgiven, Now: now})
	if err != nil {
		return err
	}
	coord := ledger.Coord{Operation: OpUsagePosting, Source: usageReceiptSourcePrefix + row.ID.String(), SourceID: "1"}
	if credit > 0 {
		if err := s.creditFall(ctx, q, merchantID, customer, currency, row.ID, coord, credit, now); err != nil {
			return err
		}
	}
	if spend > 0 {
		billed, err := q.LatestUsageInvoiceEndInCurrency(ctx, gen.LatestUsageInvoiceEndInCurrencyParams{MerchantID: merchantID, CustomerID: customer, Currency: currency})
		if err != nil {
			return err
		}
		// Usage is dated before its period's end and its cut, running charges
		// before the instant they were posted through; none after now.
		dated := func(bound time.Time) time.Time {
			return later(earlier(bound.Add(-time.Microsecond), now), billed)
		}
		posting.posting.owed = r.owedShares(func(end time.Time) time.Time { return dated(earlier(end, r.cut)) }, dated(r.through))
		key := MustIdempotencyKey(coord.Operation, coord.Source, coord.SourceID)
		customerID := identity.CustomerID(customer)
		_, _, applied, err := posting.spendBalanceThenOwedTx(ctx, q, customerID, customer.String(), currency, key, spend, true)
		if err != nil {
			return err
		}
		if !applied {
			return fmt.Errorf("usage receipt %s: the ledger already held it", row.ID)
		}
	}
	return nil
}

// periodForgiveness is what the earlier receipts of each period whose price
// fell forgave, net: every receipt that took its usage was cut at or after
// its start.
func periodForgiveness(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, r *receipt) (map[time.Time]int64, error) {
	var periods []time.Time
	for _, p := range r.periods {
		if p.amount < 0 {
			periods = append(periods, p.start)
		}
	}
	rows, err := q.SumPeriodForgiveness(ctx, gen.SumPeriodForgivenessParams{MerchantID: merchantID, CustomerID: customer, Currency: currency,
		CutFrom: slices.MinFunc(periods, func(a, b time.Time) int { return a.Compare(b) }), Periods: periods})
	if err != nil {
		return nil, err
	}
	out := make(map[time.Time]int64, len(rows))
	for _, row := range rows {
		out[row.PeriodStartsAt.UTC()] = row.Forgiven
	}
	return out, nil
}

// owedShares split what a receipt leaves owed: per period whose price rose
// and meter, what its positive lines added, dated by the period's end; and
// the running charges, dated chargesAt. Each names when its charges began.
func (r *receipt) owedShares(dated func(end time.Time) time.Time, chargesAt time.Time) []owedShare {
	var out []owedShare
	for _, p := range r.periods {
		if p.amount <= 0 {
			continue
		}
		at := dated(r.lines[p.first].PeriodEndsAt)
		byMeter := map[string]int{}
		for _, l := range r.lines[p.first:p.last] {
			if l.Amount <= 0 {
				continue
			}
			i, ok := byMeter[l.MeterKey]
			if !ok {
				i = len(out)
				byMeter[l.MeterKey] = i
				out = append(out, owedShare{source: "metered:" + l.MeterKey, from: p.start, at: at})
			}
			out[i].weight = addSat(out[i].weight, l.Amount)
		}
	}
	if len(r.charges) > 0 {
		charges := owedShare{source: runningChargeOwedSource, from: r.charges[0].StartsAt, at: chargesAt}
		for _, c := range r.charges {
			charges.weight = addSat(charges.weight, c.Amount)
			charges.from = earlier(charges.from, c.StartsAt)
		}
		out = append(out, charges)
	}
	return out
}

// creditFall credits what the periods whose price fell took beyond it, as a
// lot that never lapses, at the receipt's coordinate.
func (s *MoneyService) creditFall(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, receiptID uuid.UUID, coord ledger.Coord,
	amount int64, now time.Time) error {
	l := ledger.New(q, merchantID)
	account, err := l.EnsureCustomerBalance(ctx, customer, currency)
	if err != nil {
		return err
	}
	source := receiptID.String()
	lot, err := q.InsertCreditGrant(ctx, gen.InsertCreditGrantParams{MerchantID: merchantID, CustomerID: customer, AccountID: account, Unit: currency,
		Units: amount, FundingSource: string(credits.Grant), Source: "usage_reversal", SourceID: &source, Invoker: "openrails", StartsAt: now})
	if err != nil {
		return fmt.Errorf("usage receipt %s: record the lot: %w", receiptID, err)
	}
	if _, err := l.ReverseUsage(ctx, customer, currency, amount, coord, lot.ID); err != nil {
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

// postingDelta is what a period's price adds to the price of what posted
// before, line by line, and in all.
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
		return cmp.Or(strings.Compare(a.meter, b.meter), strings.Compare(a.cell, b.cell), strings.Compare(a.print, b.print))
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

// PostDueUsage posts, in its own transaction, everything a customer's usage
// in a currency costs and the ledger has not taken, and its running charges
// through the accounting zone's last midnight: the daily posting, once the
// accounting-zone day of its oldest pending usage or charge ended.
func (s *MoneyService) PostDueUsage(ctx context.Context, customer uuid.UUID, currency string, now time.Time) (bool, error) {
	zone, err := s.AccountingZone(ctx)
	if err != nil {
		return false, err
	}
	return s.postUsageOwnTx(ctx, customer, currency, now, dayStart(zone, now), usagePostingDaily)
}

// PostUsageNow posts at now, in its own transaction, everything a customer's
// usage and running charges in a currency cost: before its agreement ends
// and its owed is billed.
func (s *MoneyService) PostUsageNow(ctx context.Context, customer uuid.UUID, currency string, now time.Time) (bool, error) {
	return s.postUsageOwnTx(ctx, customer, currency, now, now, usagePostingBilling)
}

func (s *MoneyService) postUsageOwnTx(ctx context.Context, customer uuid.UUID, currency string, now, through time.Time, reason string) (bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	var posted bool
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		posted, err = s.WithDatabase(s.db.NewWithPgxTx(tx)).at(now).postEverything(ctx, gen.New(tx), mid.UUID(), customer, currency, through, reason)
		return err
	})
	return posted, err
}

// postEverything posts all of a customer's usage in a currency, and its
// running charges through through, in q's transaction: first the lots that
// lapsed expire, each once the usage and running charges before its lapse
// posted, so they are spent in time order.
func (s *MoneyService) postEverything(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, through time.Time, reason string) (bool, error) {
	if _, err := s.creditLots(q, merchantID).ExpireLapsedBy(ctx, customer, currency, s.now()); err != nil {
		return false, err
	}
	return s.postUsageTx(ctx, q, merchantID, customer, currency, time.Time{}, through, reason)
}

// usageReceipts are the usage receipts the transfers that took usage or
// running charges belong to, by transfer.
func usageReceipts(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, transfers []gen.BillingLedgerTransfer) (map[uuid.UUID]*billing.UsageReceipt, error) {
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
	out := map[uuid.UUID]*billing.UsageReceipt{}
	if len(receipts) == 0 {
		return out, nil
	}
	rows, err := q.ListUsageReceiptsByIDs(ctx, gen.ListUsageReceiptsByIDsParams{MerchantID: merchantID, CustomerID: customer, Ids: receipts})
	if err != nil {
		return nil, err
	}
	decoded := make([][]receiptCharge, len(rows))
	var chargeIDs []uuid.UUID
	for i, row := range rows {
		if err := json.Unmarshal(row.Charges, &decoded[i]); err != nil {
			return nil, fmt.Errorf("usage receipt %s: decode charges: %w", row.ID, err)
		}
		for _, c := range decoded[i] {
			chargeIDs = append(chargeIDs, c.RunningChargeID)
		}
	}
	named := map[uuid.UUID]gen.BillingRunningCharge{}
	if len(chargeIDs) > 0 {
		found, err := q.ListRunningChargesByIDs(ctx, gen.ListRunningChargesByIDsParams{MerchantID: merchantID, Ids: chargeIDs})
		if err != nil {
			return nil, err
		}
		for _, c := range found {
			named[c.ID] = c
		}
	}
	for i, row := range rows {
		var lines []receiptLine
		if err := json.Unmarshal(row.Lines, &lines); err != nil {
			return nil, fmt.Errorf("usage receipt %s: decode lines: %w", row.ID, err)
		}
		r := &billing.UsageReceipt{Through: row.CutAt.UTC(), Amount: row.Amount, Lines: []billing.UsageReceiptLine{}, Charges: []billing.UsageReceiptCharge{}}
		type cell struct {
			period      time.Time
			meter, cell string
		}
		var order []cell
		sums := map[cell]*billing.UsageReceiptLine{}
		for _, l := range lines {
			k := cell{l.PeriodStartsAt.UTC(), l.MeterKey, l.Cell}
			line := sums[k]
			if line == nil {
				line = &billing.UsageReceiptLine{PeriodStartsAt: l.PeriodStartsAt.UTC(), PeriodEndsAt: l.PeriodEndsAt.UTC(), MeterKey: l.MeterKey}
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
			r.Lines = append(r.Lines, *sums[k])
		}
		for _, c := range decoded[i] {
			charge := billing.UsageReceiptCharge{RunningChargeID: billing.RunningChargeID(c.RunningChargeID), StartsAt: c.StartsAt.UTC(), EndsAt: c.EndsAt.UTC(), Amount: c.Amount}
			if row, ok := named[c.RunningChargeID]; ok {
				charge.Reference, charge.Resource = row.Reference, row.Resource
			}
			r.Charges = append(r.Charges, charge)
		}
		for _, t := range ids[row.ID] {
			out[t] = r
		}
	}
	return out, nil
}
