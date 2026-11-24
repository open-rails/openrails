package money

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/catalogrules/syntax"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Usage settles in batches. A batch is priced per group of items that share
// a customer, currency, pricing period, meter cell and rate card: each group
// is charged R(Q+Σq) - R(Q) on its total, and its amount is shared out to its
// items by quantity. The amounts accrue to the customer's day
// (usage_accruals), which posts to the ledger once; the day's rollups
// (usage_daily) and per-minute spend (usage_minutes) take the same amounts. A
// failed item is not priced: what its grace leaves is charged at once, with
// its own ledger legs.

// ErrUsageOutsideIngestWindow refuses usage dated before the ingest window or
// ahead of the clock.
var ErrUsageOutsideIngestWindow = apperr.Invalidf("occurred_at must be within the last %d days and not in the future", int(retention.UsageIngestWindow/retention.Day)).WithParam("occurred_at")

// UsageItem is one usage report to settle.
type UsageItem struct {
	Customer   identity.CustomerID
	Invoker    string
	Delegated  bool
	Currency   string
	EventType  string
	Dimensions map[string]int64
	Metadata   map[string]any
	// Amount is the stated amount a dynamic card charges; a failed item's cost.
	Amount     *int64
	Failed     bool
	OccurredAt time.Time
	// Cards are the money rate cards that price the item, pinned by its
	// hold; nil prices it by the cards in force.
	Cards []byte
	// RequireCards refuses an item no card prices: a capture settles money.
	RequireCards bool
	// Hold is the hold the item was decided against; nil settles in the
	// limits' current windows.
	Hold *SettleHold
	// FailedWindows are the windows a failed item counts toward: its grace,
	// or its delegated invoker's cutoff.
	FailedWindows []FailedUsageWindow
	// FailedKey is where a charged failure posts.
	FailedKey IdempotencyKey
}

// SettleHold is the hold an item settles into: its parts' windows, and for a
// live lease's hold what the item takes from it.
type SettleHold struct {
	ID        uuid.UUID
	CreatedAt time.Time
	Parts     []HoldPart
	// Consume takes the item's units from the parts: a live lease's hold.
	Consume bool
}

// HoldPart is what a hold set aside of one limit, in the window it was
// issued in.
type HoldPart struct {
	Key            string
	Unit           string
	WindowStartsAt time.Time
	WindowEndsAt   *time.Time
	Held           int64
}

// UsageResult is what settling one item did. Err refuses the item, which
// settled nothing. Units are what it took from each limit, by key: the
// amount for balance, a spend window or a currency limit, rate-card units for
// a named one.
type UsageResult struct {
	Err      error
	Amount   int64
	Forgiven int64
	Charges  []billing.UsageCharge
	Units    map[string]int64
}

// UsageBatch is a priced batch, ready to settle.
type UsageBatch struct {
	items   []UsageItem
	Results []UsageResult
	priced  []pricedItem
	defs    []limits.Definition
	meters  map[string]limits.Meter
	now     time.Time
}

type pricedItem struct {
	occurred  time.Time
	period    time.Time
	periodEnd time.Time
	charges   []itemCharge
	// units are what the item counts toward each named-unit limit whose
	// rate cards read it.
	units map[string]int64
}

type itemCharge struct {
	meter, cell string
	model       catalog.Model
	quantity    int64
	amount      int64
}

// settleRefusal is an item's refusal on the wire.
func settleRefusal(err error) error {
	var coded *apperr.Error
	if errors.As(err, &coded) {
		return err
	}
	if errors.Is(err, ErrIdempotencyKeyReused) {
		return err
	}
	return apperr.Invalidf("%s", err.Error())
}

// PriceUsageTx validates and prices a batch and posts what its failed items
// charge. The caller's transaction holds it; customers are locked in id
// order. An item that cannot be priced is refused alone.
func (s *MoneyService) PriceUsageTx(ctx context.Context, q *gen.Queries, items []UsageItem) (*UsageBatch, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	merchantID := mid.UUID()
	now := s.now()
	b := &UsageBatch{items: items, Results: make([]UsageResult, len(items)), priced: make([]pricedItem, len(items)), now: now}
	from, to := now.Add(-retention.UsageIngestWindow), now.Add(retention.UsageClockSkew)
	for i := range items {
		it := &items[i]
		it.EventType = strings.TrimSpace(it.EventType)
		it.Invoker = strings.TrimSpace(it.Invoker)
		it.Currency = normalizeCurrency(it.Currency)
		if it.OccurredAt.IsZero() {
			it.OccurredAt = now
		}
		it.OccurredAt = instant(it.OccurredAt)
		switch {
		case it.Customer.IsZero():
			b.Results[i].Err = apperr.Invalidf("customer_id required").WithParam("customer_id")
		case it.EventType == "" || len(it.EventType) > 255:
			b.Results[i].Err = apperr.Invalidf("event_type must contain 1 to 255 bytes").WithParam("event_type")
		case moneyutil.ValidateCurrency(it.Currency) != nil:
			b.Results[i].Err = apperr.New(400, billing.CodeCurrencyUnsupported, "currency is not supported").WithParam("currency")
		case it.Amount != nil && *it.Amount < 0:
			b.Results[i].Err = apperr.Invalidf("amount must be nonnegative").WithParam("amount")
		case it.Failed && it.Amount == nil:
			b.Results[i].Err = apperr.Invalidf("failed usage must state amount").WithParam("amount")
		case len(it.Invoker) > 255:
			b.Results[i].Err = apperr.Invalidf("invoker must be at most 255 bytes").WithParam("invoker")
		case it.Delegated && (!it.Failed || it.Invoker == ""):
			b.Results[i].Err = apperr.Invalidf("a delegated invoker reports only failed usage, and names its invoker").WithParam("invoker_type")
		case it.OccurredAt.Before(from) || it.OccurredAt.After(to):
			b.Results[i].Err = ErrUsageOutsideIngestWindow
		}
	}
	// Customers in id order, then each currency's balance account: a
	// customer's first usage opens it, the mark its periods anchor on.
	type account struct {
		customer uuid.UUID
		currency string
	}
	var accounts []account
	seen := map[account]bool{}
	for i, it := range items {
		if b.Results[i].Err != nil {
			continue
		}
		a := account{it.Customer.UUID(), it.Currency}
		if !seen[a] {
			seen[a] = true
			accounts = append(accounts, a)
		}
	}
	slices.SortFunc(accounts, func(x, y account) int {
		return cmp.Or(compareUUID(x.customer, y.customer), strings.Compare(x.currency, y.currency))
	})
	locked := map[uuid.UUID]bool{}
	anchors := map[account]periodAnchor{}
	l := ledger.New(q, merchantID)
	for _, a := range accounts {
		if !locked[a.customer] {
			if err := ensureCustomer(ctx, q, merchantID, a.customer); err != nil {
				return nil, err
			}
			if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: merchantID, ID: a.customer}); err != nil {
				return nil, err
			}
			locked[a.customer] = true
		}
		if _, err := l.EnsureCustomerBalance(ctx, a.customer, a.currency); err != nil {
			return nil, err
		}
		// A lot that lapsed is expired first, after the usage before it posts
		// from it: what this batch accrues never draws a lapsed lot.
		if _, err := s.grantLedger(q, merchantID).ExpireLapsedBy(ctx, a.customer, a.currency, now); err != nil {
			return nil, err
		}
		anchor, err := s.periodAnchorFor(ctx, q, merchantID, a.customer, a.currency)
		if err != nil {
			return nil, err
		}
		anchors[a] = anchor
	}

	defs, err := limits.Definitions(ctx, q, merchantID)
	if err != nil {
		return nil, err
	}
	var meters map[string]limits.Meter
	if len(defs) > 0 {
		if meters, err = limits.Meters(ctx, q, merchantID); err != nil {
			return nil, err
		}
	}
	b.defs, b.meters = defs, meters
	// Usage counts toward every limit that reads it; only a limit a product
	// or override gives the customer capacity in refuses what it cannot
	// price.
	granted := map[uuid.UUID]map[string]bool{}
	if len(defs) > 0 {
		for _, a := range accounts {
			if _, ok := granted[a.customer]; ok {
				continue
			}
			if granted[a.customer], err = (limits.Customer{Q: q, MerchantID: merchantID, CustomerID: a.customer, Now: now}).Granted(ctx); err != nil {
				return nil, err
			}
		}
	}

	// Each item's quantities under its cards.
	type member struct {
		item     int
		quantity int64
		stated   *int64
	}
	type group struct {
		customer uuid.UUID
		currency string
		period   time.Time
		meter    string
		cell     string
		print    string
		card     ratingCard
		members  []member
		total    int64
	}
	groups := map[string]*group{}
	current := map[string][]ratingCard{}
	cardErr := map[string]error{}
	for i, it := range items {
		if b.Results[i].Err != nil {
			continue
		}
		period, periodEnd, err := anchors[account{it.Customer.UUID(), it.Currency}].period(it.OccurredAt)
		if err != nil {
			return nil, err
		}
		b.priced[i] = pricedItem{occurred: it.OccurredAt, period: period, periodEnd: periodEnd, units: map[string]int64{}}
		if it.Failed {
			continue
		}
		usage := limits.Usage{EventType: it.EventType, Dimensions: it.Dimensions, Metadata: it.Metadata}
		for _, def := range defs {
			if syntax.LimitUnitCurrency(def.Unit) || !limits.Counts(def, meters, it.EventType) {
				continue
			}
			units, err := limits.Units(def, meters, usage)
			if err != nil {
				if !granted[it.Customer.UUID()][def.Key] {
					continue
				}
				b.Results[i].Err = settleRefusal(err)
				break
			}
			b.priced[i].units[def.Key] = units
		}
		if b.Results[i].Err != nil {
			continue
		}
		var cards []ratingCard
		if len(it.Cards) > 0 {
			if err := json.Unmarshal(it.Cards, &cards); err != nil {
				return nil, fmt.Errorf("decode pinned rate cards: %w", err)
			}
		} else {
			key := it.EventType + "\x00" + it.Currency
			if _, ok := current[key]; !ok && cardErr[key] == nil {
				current[key], cardErr[key] = usageCards(ctx, q, merchantID, it.EventType, it.Currency)
			}
			if cardErr[key] != nil {
				var coded *apperr.Error
				if !errors.As(cardErr[key], &coded) {
					return nil, cardErr[key]
				}
				b.Results[i].Err = cardErr[key]
				continue
			}
			cards = current[key]
		}
		if it.RequireCards && len(cards) == 0 {
			b.Results[i].Err = usageNotPriced("event_type", "no rate card prices event type %q", it.EventType)
			continue
		}
		quantities, err := usageQuantities(cards, eventFields{Dimensions: it.Dimensions, Metadata: it.Metadata, Amount: it.Amount})
		if err != nil {
			b.Results[i].Err = settleRefusal(err)
			continue
		}
		for _, u := range quantities {
			// A card that cannot price the item refuses it before anything
			// is written.
			if err := u.priceable(); err != nil {
				b.Results[i].Err = apperr.New(http.StatusUnprocessableEntity, billing.CodeUsageNotPriced, err.Error()).WithParam("event_type")
				break
			}
			print, err := cardPrint(u.card)
			if err != nil {
				return nil, err
			}
			key := strings.Join([]string{it.Customer.UUID().String(), it.Currency, period.Format(time.RFC3339Nano), u.card.MeterKey, u.cell, print}, "\x00")
			g := groups[key]
			if g == nil {
				g = &group{customer: it.Customer.UUID(), currency: it.Currency, period: period, meter: u.card.MeterKey, cell: u.cell, print: print, card: u.card}
				groups[key] = g
			}
			if g.total > maxQuantity-u.quantity {
				b.Results[i].Err = apperr.Invalidf("meter %q quantity overflows", u.card.MeterKey)
				break
			}
			g.members = append(g.members, member{item: i, quantity: u.quantity, stated: u.stated})
			g.total += u.quantity
		}
	}
	// A refused item leaves every group it joined.
	ordered := make([]*group, 0, len(groups))
	for _, g := range groups {
		kept := g.members[:0]
		g.total = 0
		for _, m := range g.members {
			if b.Results[m.item].Err == nil {
				kept = append(kept, m)
				g.total += m.quantity
			}
		}
		g.members = kept
		if len(g.members) > 0 {
			ordered = append(ordered, g)
		}
	}
	slices.SortFunc(ordered, func(x, y *group) int {
		return cmp.Or(compareUUID(x.customer, y.customer), strings.Compare(x.currency, y.currency), x.period.Compare(y.period),
			strings.Compare(x.meter, y.meter), strings.Compare(x.cell, y.cell), strings.Compare(x.print, y.print))
	})
	for _, g := range ordered {
		total, err := q.AddMeterPeriodQuantity(ctx, gen.AddMeterPeriodQuantityParams{
			MerchantID: merchantID, CustomerID: g.customer, Currency: g.currency, MeterKey: g.meter,
			Cell: g.cell, PeriodStartsAt: g.period, Quantity: g.total, Now: now,
		})
		if err != nil {
			return nil, fmt.Errorf("meter %q period quantity: %w", g.meter, err)
		}
		shares := make([]int64, len(g.members))
		if g.card.Price.Model == catalog.ModelDynamic {
			for j, m := range g.members {
				if m.quantity > 0 && m.stated != nil {
					shares[j] = *m.stated
				}
			}
		} else {
			amount, err := marginalPrice(g.card, g.cell, total-g.total, g.total)
			if err != nil {
				return nil, err
			}
			quantities := make([]int64, len(g.members))
			for j, m := range g.members {
				quantities[j] = m.quantity
			}
			shares = share(amount, quantities)
		}
		for j, m := range g.members {
			p := &b.priced[m.item]
			p.charges = append(p.charges, itemCharge{meter: g.meter, cell: g.cell, model: g.card.Price.Model, quantity: m.quantity, amount: shares[j]})
		}
	}
	for i := range items {
		if b.Results[i].Err != nil {
			continue
		}
		p := &b.priced[i]
		slices.SortStableFunc(p.charges, func(x, y itemCharge) int {
			return cmp.Or(strings.Compare(x.meter, y.meter), strings.Compare(x.cell, y.cell))
		})
		r := &b.Results[i]
		r.Charges = make([]billing.UsageCharge, 0, len(p.charges))
		for _, c := range p.charges {
			if r.Amount > maxQuantity-c.amount {
				return nil, fmt.Errorf("usage amount overflows int64")
			}
			r.Amount += c.amount
			out := billing.UsageCharge{MeterKey: c.meter, Model: c.model, Quantity: c.quantity, Amount: c.amount}
			if c.cell != "" {
				cell := c.cell
				out.Cell = &cell
			}
			r.Charges = append(r.Charges, out)
		}
	}

	// A failure is charged what its grace does not forgive, at once, and
	// counts toward its windows.
	for i, it := range items {
		if b.Results[i].Err != nil || !it.Failed {
			continue
		}
		stated := *it.Amount
		periods, err := failedUsagePeriods(it.FailedWindows, now)
		if err != nil {
			return nil, err
		}
		key := failedUsageKey{merchant: merchantID, customer: it.Customer.UUID(), currency: it.Currency}
		// A delegated invoker's failure, or one with no grace windows, is
		// not charged.
		var charge int64
		switch {
		case it.Delegated:
			key.invoker = it.Invoker
		case len(periods) == 0:
		default:
			left, err := key.graceLeft(ctx, q, periods)
			if err != nil {
				return nil, err
			}
			charge = max(stated-left, 0)
		}
		if err := key.count(ctx, q, periods, stated); err != nil {
			return nil, err
		}
		if charge > 0 {
			if it.FailedKey.IsZero() {
				return nil, fmt.Errorf("a charged failure needs its ledger key")
			}
			if _, _, _, err := s.spendBalanceThenOwedTx(ctx, q, it.Customer, it.Invoker, it.Currency, it.FailedKey, charge, true); err != nil {
				return nil, err
			}
		}
		b.Results[i].Amount, b.Results[i].Forgiven = charge, stated-charge
		b.Results[i].Charges = []billing.UsageCharge{}
	}
	return b, nil
}

// SettleUsageTx settles a priced batch in the caller's transaction: the
// accruals, rollups and per-minute spend take its amounts, live lease holds
// give up what their usage took, and the limits take it in full, never
// clamped.
func (s *MoneyService) SettleUsageTx(ctx context.Context, q *gen.Queries, b *UsageBatch) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	merchantID := mid.UUID()
	if err := s.settleLimitsTx(ctx, q, merchantID, b); err != nil {
		return err
	}
	if err := s.accrueTx(ctx, q, merchantID, b); err != nil {
		return err
	}
	return rollUpTx(ctx, q, merchantID, b)
}

// accrueTx adds each succeeded item's amount to its day's accrual.
func (s *MoneyService) accrueTx(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, b *UsageBatch) error {
	type key struct {
		customer uuid.UUID
		currency string
		day      time.Time
		period   time.Time
	}
	type sum struct {
		amount    int64
		periodEnd time.Time
		lines     map[[2]string]*AccrualLine
	}
	sums := map[key]*sum{}
	var keys []key
	for i, it := range b.items {
		if b.Results[i].Err != nil || it.Failed {
			continue
		}
		p := b.priced[i]
		k := key{it.Customer.UUID(), it.Currency, dayOf(p.occurred), p.period}
		acc := sums[k]
		if acc == nil {
			acc = &sum{periodEnd: p.periodEnd, lines: map[[2]string]*AccrualLine{}}
			sums[k] = acc
			keys = append(keys, k)
		}
		acc.amount = addSat(acc.amount, b.Results[i].Amount)
		for _, c := range p.charges {
			line := acc.lines[[2]string{c.meter, c.cell}]
			if line == nil {
				line = &AccrualLine{MeterKey: c.meter, Cell: c.cell}
				acc.lines[[2]string{c.meter, c.cell}] = line
			}
			line.Requests++
			line.Quantity = addSat(line.Quantity, c.quantity)
			line.Amount = addSat(line.Amount, c.amount)
		}
	}
	slices.SortFunc(keys, func(x, y key) int {
		return cmp.Or(compareUUID(x.customer, y.customer), strings.Compare(x.currency, y.currency), x.day.Compare(y.day), x.period.Compare(y.period))
	})
	for _, k := range keys {
		acc := sums[k]
		if acc.amount == 0 && len(acc.lines) == 0 {
			continue
		}
		row, err := q.LockUsageAccrual(ctx, gen.LockUsageAccrualParams{MerchantID: merchantID, CustomerID: k.customer, Currency: k.currency, DayAt: k.day, PeriodStartsAt: k.period})
		found := err == nil
		var lines []AccrualLine
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			if lines, err = decodeAccrualLines(row.Lines); err != nil {
				return err
			}
		}
		for _, add := range acc.lines {
			lines = mergeAccrualLine(lines, *add)
		}
		slices.SortFunc(lines, func(x, y AccrualLine) int {
			return cmp.Or(strings.Compare(x.MeterKey, y.MeterKey), strings.Compare(x.Cell, y.Cell))
		})
		encoded, err := json.Marshal(lines)
		if err != nil {
			return err
		}
		if !found {
			if _, err := q.InsertUsageAccrual(ctx, gen.InsertUsageAccrualParams{MerchantID: merchantID, CustomerID: k.customer, Currency: k.currency,
				DayAt: k.day, PeriodStartsAt: k.period, PeriodEndsAt: acc.periodEnd, Amount: acc.amount, Lines: encoded, Now: b.now}); err != nil {
				return err
			}
		} else if err := q.AddToUsageAccrual(ctx, gen.AddToUsageAccrualParams{Amount: addSat(row.Amount, acc.amount), Lines: encoded, Now: b.now, MerchantID: merchantID, ID: row.ID}); err != nil {
			return err
		}
		if acc.amount > 0 {
			// An accrual takes from what the customer may spend: one that
			// crosses a line of its balance watch queues a look, and one that
			// takes arrears to the threshold asks for a usage order.
			if err := ledger.WatchChange(ctx, q, merchantID, k.customer, k.currency, false, b.now); err != nil {
				return err
			}
			if err := s.queueAccruedUsageBilling(ctx, q, merchantID, k.customer, k.currency); err != nil {
				return err
			}
		}
	}
	return nil
}

// rollUpTx adds the batch to the day's rollups and the minutes' spend.
func rollUpTx(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, b *UsageBatch) error {
	type dayKey struct {
		customer                                           uuid.UUID
		day                                                time.Time
		currency, eventType, meter, cell, invoker, outcome string
	}
	type dayRow struct{ requests, quantity, amount, forgiven int64 }
	type minuteKey struct {
		customer uuid.UUID
		currency string
		minute   time.Time
	}
	days := map[dayKey]*dayRow{}
	minutes := map[minuteKey]int64{}
	add := func(k dayKey, requests, quantity, amount, forgiven int64) {
		r := days[k]
		if r == nil {
			r = &dayRow{}
			days[k] = r
		}
		r.requests, r.quantity = addSat(r.requests, requests), addSat(r.quantity, quantity)
		r.amount, r.forgiven = addSat(r.amount, amount), addSat(r.forgiven, forgiven)
	}
	for i, it := range b.items {
		res := b.Results[i]
		if res.Err != nil {
			continue
		}
		p := b.priced[i]
		outcome := UsageSucceeded
		own := int64(0)
		if it.Failed {
			outcome, own = UsageFailed, res.Amount
		}
		base := dayKey{customer: it.Customer.UUID(), day: dayOf(p.occurred), currency: it.Currency, eventType: it.EventType, invoker: it.Invoker, outcome: outcome}
		add(base, 1, 0, own, res.Forgiven)
		for _, c := range p.charges {
			k := base
			k.meter, k.cell = c.meter, c.cell
			add(k, 1, c.quantity, c.amount, 0)
		}
		if res.Amount > 0 {
			mk := minuteKey{it.Customer.UUID(), it.Currency, p.occurred.Truncate(time.Minute)}
			minutes[mk] = addSat(minutes[mk], res.Amount)
		}
	}
	if len(days) > 0 {
		keys := make([]dayKey, 0, len(days))
		for k := range days {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(x, y dayKey) int {
			return cmp.Or(compareUUID(x.customer, y.customer), x.day.Compare(y.day), strings.Compare(x.currency, y.currency),
				strings.Compare(x.eventType, y.eventType), strings.Compare(x.meter, y.meter), strings.Compare(x.cell, y.cell),
				strings.Compare(x.invoker, y.invoker), strings.Compare(x.outcome, y.outcome))
		})
		arg := gen.UpsertUsageDailyParams{MerchantID: merchantID, Now: b.now}
		var eventTypes []string
		for _, k := range keys {
			if !slices.Contains(eventTypes, k.eventType) {
				eventTypes = append(eventTypes, k.eventType)
			}
		}
		if err := q.ShareUsageEventTypes(ctx, gen.ShareUsageEventTypesParams{MerchantID: merchantID, EventTypes: eventTypes}); err != nil {
			return err
		}
		for _, k := range keys {
			r := days[k]
			arg.CustomerIds = append(arg.CustomerIds, k.customer)
			arg.DayAts = append(arg.DayAts, k.day)
			arg.Currencies = append(arg.Currencies, k.currency)
			arg.EventTypes = append(arg.EventTypes, k.eventType)
			arg.MeterKeys = append(arg.MeterKeys, k.meter)
			arg.Cells = append(arg.Cells, k.cell)
			arg.Invokers = append(arg.Invokers, k.invoker)
			arg.Outcomes = append(arg.Outcomes, k.outcome)
			arg.Requests = append(arg.Requests, r.requests)
			arg.Quantities = append(arg.Quantities, r.quantity)
			arg.Amounts = append(arg.Amounts, r.amount)
			arg.ForgivenAmounts = append(arg.ForgivenAmounts, r.forgiven)
		}
		if err := q.UpsertUsageDaily(ctx, arg); err != nil {
			return err
		}
	}
	if len(minutes) > 0 {
		keys := make([]minuteKey, 0, len(minutes))
		for k := range minutes {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(x, y minuteKey) int {
			return cmp.Or(compareUUID(x.customer, y.customer), strings.Compare(x.currency, y.currency), x.minute.Compare(y.minute))
		})
		arg := gen.UpsertUsageMinutesParams{MerchantID: merchantID}
		for _, k := range keys {
			arg.CustomerIds = append(arg.CustomerIds, k.customer)
			arg.Currencies = append(arg.Currencies, k.currency)
			arg.Minutes = append(arg.Minutes, k.minute)
			arg.Amounts = append(arg.Amounts, minutes[k])
		}
		if err := q.UpsertUsageMinutes(ctx, arg); err != nil {
			return err
		}
	}
	return nil
}

// settleLimitsTx takes each item's units from the hold it was decided
// against and settles them, with what the customer's other declared limits
// count of it, into their windows: the hold's for a limit it holds, the
// current window for the rest.
func (s *MoneyService) settleLimitsTx(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, b *UsageBatch) error {
	defs := b.defs
	var err error
	byCustomer := map[uuid.UUID][]int{}
	var customers []uuid.UUID
	for i, it := range b.items {
		if b.Results[i].Err != nil {
			continue
		}
		c := it.Customer.UUID()
		if _, ok := byCustomer[c]; !ok {
			customers = append(customers, c)
		}
		byCustomer[c] = append(byCustomer[c], i)
	}
	slices.SortFunc(customers, compareUUID)
	type consumed struct {
		hold   uuid.UUID
		at     time.Time
		key    string
		amount int64
	}
	for _, customer := range customers {
		reader := limits.Customer{Q: q, MerchantID: merchantID, CustomerID: customer, Now: b.now,
			Period: func(ctx context.Context, unit string, at time.Time) (time.Time, time.Time, error) {
				return s.PricingPeriod(ctx, q, merchantID, customer, unit, at)
			}}
		var current map[string]limits.Claim
		if len(defs) > 0 {
			if current, err = reader.Windows(ctx, defs); err != nil {
				return err
			}
		}
		type windowKey struct {
			key   string
			start time.Time
		}
		settled := map[windowKey]*limits.Settlement{}
		var order []windowKey
		var takes []consumed
		settle := func(st limits.Settlement) {
			if st.Units == 0 {
				return
			}
			k := windowKey{st.Key, st.Window.StartsAt}
			if cur := settled[k]; cur != nil {
				cur.Units = addSat(cur.Units, st.Units)
				return
			}
			settled[k] = &st
			order = append(order, k)
		}
		for _, i := range byCustomer[customer] {
			it, res, priced := b.items[i], &b.Results[i], b.priced[i]
			amount := res.Amount
			res.Units = map[string]int64{catalog.BalanceLimit: amount}
			held := map[string]bool{}
			if it.Hold != nil {
				for _, p := range it.Hold.Parts {
					held[p.Key] = true
					w := limits.Window{StartsAt: p.WindowStartsAt.UTC(), EndsAt: p.WindowEndsAt, Open: true}
					units := amount
					switch def, declared := findDef(defs, p.Key); {
					case p.Key == catalog.BalanceLimit:
					case strings.HasPrefix(p.Key, limits.SpendKeyPrefix):
						settle(limits.Settlement{Key: p.Key, Reset: catalog.LimitReset{Kind: catalog.ResetFixed}, Window: w, KeepCapacity: true, Units: amount})
					case declared:
						if def.Unit != it.Currency {
							units = priced.units[def.Key]
						}
						// A grant that lapsed since the hold leaves the limit out of
						// current: the hold's window counts only if its row exists.
						c, ok := current[def.Key]
						ungranted := !ok || c.Ungranted
						settle(limits.Settlement{Key: def.Key, Reset: def.Reset, Window: w, Capacity: c.Capacity, KeepCapacity: ungranted, Units: units, Ungranted: ungranted})
					default:
						continue
					}
					res.Units[p.Key] = units
					if it.Hold.Consume && units > 0 {
						takes = append(takes, consumed{it.Hold.ID, it.Hold.CreatedAt, p.Key, units})
					}
				}
			}
			// A limit the hold does not name takes the usage in its current
			// window: a granted one's, or an ungranted one's row that exists.
			for _, def := range defs {
				c, ok := current[def.Key]
				if held[def.Key] || !ok {
					continue
				}
				units, counts := priced.units[def.Key]
				if def.Unit == it.Currency {
					units, counts = amount, true
				}
				if !counts {
					continue
				}
				settle(limits.Settlement{Key: def.Key, Reset: def.Reset, Window: c.Window, Capacity: c.Capacity, KeepCapacity: c.Ungranted, Units: units, Ungranted: c.Ungranted})
				res.Units[def.Key] = units
			}
		}
		// What the usage took leaves its live hold before the windows are
		// settled, so it is not counted twice.
		for _, t := range takes {
			if err := q.ConsumeLeaseHoldPart(ctx, gen.ConsumeLeaseHoldPartParams{Units: t.amount, MerchantID: merchantID, HoldID: t.hold, HoldCreatedAt: t.at, LimitKey: t.key}); err != nil {
				return err
			}
		}
		list := make([]limits.Settlement, 0, len(order))
		for _, k := range order {
			list = append(list, *settled[k])
		}
		if err := (limits.Holder{Q: q, MerchantID: merchantID, CustomerID: customer, Now: b.now}).Settle(ctx, list); err != nil {
			return err
		}
	}
	return nil
}

func findDef(defs []limits.Definition, key string) (limits.Definition, bool) {
	for _, d := range defs {
		if d.Key == key {
			return d, true
		}
	}
	return limits.Definition{}, false
}

// AccrualLine is what one meter cell of an accrual is for.
type AccrualLine struct {
	MeterKey     string `json:"meter_key"`
	Cell         string `json:"cell"`
	Requests     int64  `json:"requests"`
	Quantity     int64  `json:"quantity"`
	Amount       int64  `json:"amount"`
	PostedAmount int64  `json:"posted_amount"`
}

func decodeAccrualLines(raw []byte) ([]AccrualLine, error) {
	var lines []AccrualLine
	if len(raw) == 0 {
		return lines, nil
	}
	if err := json.Unmarshal(raw, &lines); err != nil {
		return nil, fmt.Errorf("decode accrual lines: %w", err)
	}
	return lines, nil
}

func mergeAccrualLine(lines []AccrualLine, add AccrualLine) []AccrualLine {
	for i := range lines {
		if lines[i].MeterKey == add.MeterKey && lines[i].Cell == add.Cell {
			lines[i].Requests = addSat(lines[i].Requests, add.Requests)
			lines[i].Quantity = addSat(lines[i].Quantity, add.Quantity)
			lines[i].Amount = addSat(lines[i].Amount, add.Amount)
			return lines
		}
	}
	return append(lines, add)
}

// share splits amount across weights exactly: each takes its cumulative
// share's floor, so the parts sum to amount and none is negative.
func share(amount int64, weights []int64) []int64 {
	out := make([]int64, len(weights))
	var total big.Int
	for _, w := range weights {
		total.Add(&total, big.NewInt(w))
	}
	if total.Sign() == 0 {
		if len(out) > 0 {
			out[len(out)-1] = amount
		}
		return out
	}
	var cumulative, prev, x big.Int
	a := big.NewInt(amount)
	for i, w := range weights {
		cumulative.Add(&cumulative, big.NewInt(w))
		x.Mul(a, &cumulative)
		x.Quo(&x, &total)
		out[i] = new(big.Int).Sub(&x, &prev).Int64()
		prev.Set(&x)
	}
	return out
}

// cardPrint names a rate card's terms: items priced under two revisions of a
// card are separate groups.
func cardPrint(card ratingCard) (string, error) {
	raw, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8]), nil
}

func dayOf(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

func compareUUID(a, b uuid.UUID) int { return strings.Compare(string(a[:]), string(b[:])) }

func addSat(a, b int64) int64 {
	switch {
	case b > 0 && a > maxQuantity-b:
		return maxQuantity
	case b < 0 && a < -maxQuantity-1-b:
		return -maxQuantity - 1
	}
	return a + b
}

// periodAnchor is what a customer's pricing periods in a currency are cut
// on: the usage orders' boundary and anchor.
type periodAnchor struct {
	anchor   time.Time
	boundary string
}

func (p periodAnchor) start(at time.Time) (time.Time, error) {
	return CurrentBillingPeriodStart(at, p.anchor, p.boundary)
}

// period is the period at falls in, as PricingPeriod cuts it.
func (p periodAnchor) period(at time.Time) (time.Time, time.Time, error) {
	start, err := p.start(at)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	switch merchantconfig.NormalizeBillingBoundary(p.boundary) {
	case BillingBoundaryCalendarMonth:
		return start, start.AddDate(0, 1, 0), nil
	case BillingBoundaryFixedInterval:
		return start, start.Add(fixedBillingInterval), nil
	}
	anchor := p.anchor
	if anchor.IsZero() {
		anchor = time.Unix(0, 0).UTC()
	}
	return start, NextUsageBillingDate(start, anchor), nil
}

// periodAnchorFor is usagePeriodStart's anchor and boundary, read once per
// customer and currency.
func (s *MoneyService) periodAnchorFor(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, currency string) (periodAnchor, error) {
	cfg, cur, err := s.usageBillingConfig(ctx, currency)
	if err != nil {
		return periodAnchor{}, err
	}
	if cur != nil {
		row, err := s.ensureUsageBillingAccount(ctx, q, customerID, currency, cfg)
		if err != nil {
			return periodAnchor{}, err
		}
		return periodAnchor{anchor: *row.UsageBillingAnchor, boundary: BillingBoundaryAnniversary}, nil
	}
	boundary, err := s.billingPeriodBoundary(ctx)
	if err != nil && !errors.Is(err, merchantconfig.ErrNoSettings) {
		return periodAnchor{}, err
	}
	var anchor time.Time
	if boundary == BillingBoundaryAnniversary {
		if anchor, err = q.GetCustomerPeriodAnchor(ctx, gen.GetCustomerPeriodAnchorParams{MerchantID: merchantID, CustomerID: customerID, Currency: currency}); err != nil {
			return periodAnchor{}, fmt.Errorf("customer period anchor: %w", err)
		}
	}
	return periodAnchor{anchor: anchor, boundary: boundary}, nil
}
