package money

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Usage reaches the ledger in postings; until then a customer's balance
// holds its provisional price (usage_pending_amounts). A posting takes
// everything the event store holds (its cut is past every usage intake
// accepted), except one before a credit lot's lapse, cut at it. Per pricing
// period it posts the price of the period's deduplicated usage before the
// cut, less what the period's earlier postings took: the balance first, one
// credit_spend per lot live just before the cut, and the rest owed in one
// owed_accrual split by meter, at (usage_posting, usage_posting:<id>, 1). A
// period whose price fell below what posted (a volume tier) carries the
// difference until its late usage window closes, then credits it back; usage
// the event store no longer holds is not a price that fell.
//
// Postings run once the accounting-zone day of a customer's oldest pending
// usage ends (the billing pass's first scan), before a usage invoice bills
// what is owed, before a credit lot's expiry, revocation or refund (so lots
// are spent in time order), and before a merchant purge deletes the store.

const usagePostingSourcePrefix = "usage_posting:"

// Posting reasons (usage_postings.reason).
const (
	usagePostingDaily    = "daily"
	usagePostingBilling  = "billing"
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
// else what occurred before cut. reason is usage_postings.reason: credit
// before a credit lot moves, purge before a merchant purge.
func (s *MoneyService) PostUsage(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, cut, now time.Time, reason string) (bool, error) {
	if _, bound := merchant.FromContext(ctx); !bound {
		ctx = merchant.WithID(ctx, billing.MerchantID(merchantID))
	}
	return s.WithDatabase(s.db.InTx(q)).at(now).postUsageTx(ctx, q, merchantID, customer, currency, cut, reason)
}

// postUsageTx is the one choke point usage reaches the ledger through. Under
// the customer's lock and its queue row's, it posts each period's price
// before cut less what the period took, credits a period whose price fell
// once its late window closed, and leaves the queue row at what is left. A
// zero cut takes everything: no intake can be inserting while the row is
// locked, so every usage the store holds is in what was pending and nothing
// else is, and the pending amounts go. A cut before one already posted takes
// that one's: usage before it is posted. Of the usage an earlier posting may
// have read, a line is priced at no less than what posted: usage the store no
// longer holds is never read as a price that fell, and usage ingested since
// is priced on top.
func (s *MoneyService) postUsageTx(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, cut time.Time, reason string) (bool, error) {
	key := gen.GetUsagePendingParams{MerchantID: merchantID, CustomerID: customer, Currency: currency}
	if _, err := q.GetUsagePending(ctx, key); errors.Is(err, pgx.ErrNoRows) {
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
	row, err := q.LockUsagePending(ctx, gen.LockUsagePendingParams(key))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now := instant(s.now())
	everything := cut.IsZero()
	if horizon := now.Add(retention.UsageClockSkew); everything || cut.After(horizon) {
		cut = horizon
	}
	cut = instant(cut)
	postings, err := q.ListUsagePostings(ctx, gen.ListUsagePostingsParams{MerchantID: merchantID, CustomerID: customer, Currency: currency, PeriodStartsAt: row.SinceAt})
	if err != nil {
		return false, err
	}
	var prior eventstore.PostingWatermark
	for _, p := range postings {
		prior.Cut, prior.Ingested = later(prior.Cut, p.CutAt.UTC()), later(prior.Ingested, p.IngestedThrough.UTC())
	}
	// Lots live just before the cut asked for are drawn, and none past now.
	liveAt := earlier(cut, now)
	cut = later(cut, prior.Cut)
	rows, err := store.PostingQuantities(ctx, billing.CustomerID(customer), currency, row.SinceAt, cut, prior)
	if err != nil {
		return false, err
	}
	posted := periodLines{}
	var prints []string
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
				prints = append(prints, l.Print)
			}
			have.Requests, have.Quantity = addSat(have.Requests, l.Requests), addSat(have.Quantity, l.Quantity)
			have.Stated, have.Amount = addSat(have.Stated, l.Stated), addSat(have.Amount, l.Amount)
		}
	}
	for _, r := range rows {
		prints = append(prints, r.Print)
	}
	cards, err := loadPrints(ctx, q, merchantID, prints)
	if err != nil {
		return false, err
	}
	priced, err := s.knownPrints(ctx, q, store, merchantID, customer, currency, row.SinceAt, cards, rows)
	if err != nil {
		return false, err
	}
	target, err := cards.priceQuantities(takenQuantities(priced, posted))
	if err != nil {
		return false, err
	}
	ingested := prior.Ingested
	for _, r := range priced {
		if r.Read.Requests > 0 || r.Fresh.Requests > 0 {
			ingested = later(ingested, r.IngestedThrough)
		}
	}
	anchor, err := s.periodAnchorFor(ctx, q, merchantID, customer, currency)
	if err != nil {
		return false, err
	}
	billed, err := q.LatestUsageInvoiceEndInCurrency(ctx, gen.LatestUsageInvoiceEndInCurrencyParams{MerchantID: merchantID, CustomerID: customer, Currency: currency})
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
	first, hasLater := earliestLater(rows)
	if hasLater {
		zone, err := s.accountingZone(ctx)
		if err != nil {
			return false, err
		}
		keep(first, dayAfter(zone, earlier(cut, now)))
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
			if err := s.postPeriod(ctx, q, merchantID, customer, currency, period, end, cut, liveAt, ingested, reason, amount, lines, billed); err != nil {
				return moved, err
			}
			moved = true
		case amount < 0 && !now.Before(end.Add(retention.UsageIngestWindow)):
			if err := s.reversePeriod(ctx, q, merchantID, customer, currency, period, end, cut, ingested, amount, forgiven[period], lines); err != nil {
				return moved, err
			}
			moved = true
		case amount < 0:
			keep(period, end.Add(retention.UsageIngestWindow))
		}
	}
	if since.IsZero() {
		return moved, q.DeleteUsagePending(ctx, gen.DeleteUsagePendingParams(key))
	}
	if everything && !hasLater {
		if err := q.DeleteUsagePendingAmounts(ctx, gen.DeleteUsagePendingAmountsParams(key)); err != nil {
			return moved, err
		}
	}
	return moved, q.SetUsagePending(ctx, gen.SetUsagePendingParams{SinceAt: since, DueAt: later(due, since.Add(time.Microsecond)), Now: now,
		MerchantID: merchantID, CustomerID: customer, Currency: currency})
}

// earliestLater is the earliest pricing period with usage at or after a
// posting's cut: where the customer's queue row stays.
func earliestLater(rows []eventstore.PostingQuantity) (time.Time, bool) {
	var first time.Time
	for _, r := range rows {
		if r.Later > 0 && (first.IsZero() || r.PeriodStartsAt.Before(first)) {
			first = r.PeriodStartsAt
		}
	}
	return first, !first.IsZero()
}

// takenQuantities are what a posting prices, line by line: of the usage an
// earlier posting may have read, no less than what posted (the store may have
// lost it), and the rest of the usage before the cut on top.
func takenQuantities(rows []eventstore.PostingQuantity, posted periodLines) []eventstore.PeriodQuantity {
	type line struct {
		period time.Time
		lineKey
	}
	read, fresh := map[line]eventstore.Quantities{}, map[line]eventstore.Quantities{}
	var order []line
	add := func(k line) {
		if _, ok := read[k]; !ok {
			read[k] = eventstore.Quantities{}
			order = append(order, k)
		}
	}
	for _, r := range rows {
		k := line{r.PeriodStartsAt.UTC(), lineKey{r.Meter, r.Cell, r.Print}}
		add(k)
		read[k] = sumQuantities(read[k], r.Read)
		fresh[k] = sumQuantities(fresh[k], r.Fresh)
	}
	for period, lines := range posted {
		for k := range lines {
			add(line{period, k})
		}
	}
	slices.SortFunc(order, func(a, b line) int {
		return cmp.Or(a.period.Compare(b.period), strings.Compare(a.meter, b.meter), strings.Compare(a.cell, b.cell), strings.Compare(a.print, b.print))
	})
	out := make([]eventstore.PeriodQuantity, 0, len(order))
	for _, k := range order {
		taken := read[k]
		if p := posted[k.period][k.lineKey]; p != nil {
			taken = eventstore.Quantities{Quantity: max(taken.Quantity, p.Quantity), Stated: max(taken.Stated, p.Stated), Requests: max(taken.Requests, p.Requests)}
		}
		taken = sumQuantities(taken, fresh[k])
		if taken == (eventstore.Quantities{}) {
			continue
		}
		out = append(out, eventstore.PeriodQuantity{PeriodStartsAt: k.period, Meter: k.meter, Cell: k.cell, Print: k.print,
			Quantity: taken.Quantity, Stated: taken.Stated, Requests: taken.Requests})
	}
	return out
}

func sumQuantities(a, b eventstore.Quantities) eventstore.Quantities {
	return eventstore.Quantities{Quantity: addSat(a.Quantity, b.Quantity), Stated: addSat(a.Stated, b.Stated), Requests: addSat(a.Requests, b.Requests)}
}

// knownPrints are the posting's rows whose card prints are recorded. A print
// the store names but PostgreSQL does not is usage whose intake inserted its
// events and never committed: one a rate card in force still prices is
// recorded, dated when its first event was ingested; the rest cannot be
// priced and are left out, never holding up the customer's other usage.
func (s *MoneyService) knownPrints(ctx context.Context, q *gen.Queries, store *eventstore.Store, merchantID, customer uuid.UUID, currency string,
	from time.Time, cards usagePrints, rows []eventstore.PostingQuantity) ([]eventstore.PostingQuantity, error) {
	var missing []string
	for _, r := range rows {
		if _, ok := cards[r.Print]; !ok && !slices.Contains(missing, r.Print) {
			missing = append(missing, r.Print)
		}
	}
	if len(missing) == 0 {
		return rows, nil
	}
	sources, err := store.PrintSources(ctx, billing.CustomerID(customer), currency, from, missing)
	if err != nil {
		return nil, err
	}
	var eventTypes []string
	for _, src := range sources {
		if !slices.Contains(eventTypes, src.EventType) {
			eventTypes = append(eventTypes, src.EventType)
		}
	}
	slices.Sort(eventTypes)
	var recorded []string
	for _, eventType := range eventTypes {
		current, err := usageCards(ctx, q, merchantID, eventType, currency)
		var coded *apperr.Error
		if errors.As(err, &coded) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, card := range current {
			print, err := cardPrint(card)
			if err != nil {
				return nil, err
			}
			src, ok := sources[print]
			if !ok || !slices.Contains(missing, print) || slices.Contains(recorded, print) {
				continue
			}
			raw, err := json.Marshal(card)
			if err != nil {
				return nil, err
			}
			if err := q.InsertUsageCardPrint(ctx, gen.InsertUsageCardPrintParams{MerchantID: merchantID, Print: print, Card: raw, Now: src.IngestedAt}); err != nil {
				return nil, err
			}
			recorded = append(recorded, print)
		}
	}
	if len(recorded) > 0 {
		found, err := loadPrints(ctx, q, merchantID, recorded)
		if err != nil {
			return nil, err
		}
		maps.Copy(cards, found)
	}
	return slices.DeleteFunc(slices.Clone(rows), func(r eventstore.PostingQuantity) bool { _, ok := cards[r.Print]; return !ok }), nil
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
// of the customer's last usage invoice. It draws the lots live just before
// liveAt; in a merchant's unit, never owed, what they cannot take is forgiven.
func (s *MoneyService) postPeriod(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, period, end, cut, liveAt, ingested time.Time,
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
		PeriodStartsAt: period, PeriodEndsAt: end, CutAt: cut, Reason: reason, Amount: amount, Forgiven: forgiven, ForgivenLines: split, Lines: encoded,
		IngestedThrough: ingested, Now: now})
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
func (s *MoneyService) reversePeriod(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, period, end, cut, ingested time.Time,
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
		PeriodStartsAt: period, PeriodEndsAt: end, CutAt: cut, Reason: usagePostingReversal, Amount: amount, Forgiven: unforgiven, ForgivenLines: split,
		Lines: encoded, IngestedThrough: ingested, Now: now})
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
// the accounting-zone day of its oldest pending usage ended. It also credits a
// period whose price fell once its late window closed.
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
