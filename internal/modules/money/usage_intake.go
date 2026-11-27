package money

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/catalogrules/syntax"
	"github.com/open-rails/openrails/internal/currency"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Usage is taken in, not posted: intake adds each item's quantities and
// provisional price (the marginal price at the period position PostgreSQL's
// running totals hold: what limits count) to its lease's running totals
// (usage_totals), which the balance deducts as pending until a posting takes
// them, and makes it one event in the event store, carrying its priced meter
// cells (under the card print in force, in its pricing period) and the limit
// windows that count it, located when it was admitted. A failed item is not
// priced: what its grace leaves is charged at once, with its own ledger legs.
//
// Intake is two steps per batch in the caller's transaction, gathered in its
// UsageSettlement: PrepareUsageTx validates, prices and locates, writing
// nothing; then CommitUsageTx writes the PostgreSQL side. The event store
// takes the settlement's events only once the transaction committed, so a
// resend inserts nothing again. It is append-only and takes no customer's
// lock, except to change what that lock guards: to open a session or
// first-use window (two first uses in different seconds would open two), or
// to charge a failed item's grace (a balance movement). Concurrent intakes of
// one customer may price from the same period position; postings price the
// period's total, so only provisional amounts can differ.

// ErrUsageOutsideIngestWindow refuses usage dated before the ingest window or
// ahead of the clock.
var ErrUsageOutsideIngestWindow = apperr.Invalidf("occurred_at must be within the last %d days and not in the future", int(retention.UsageIngestWindow/retention.Day)).WithParam("occurred_at")

// ErrUsageMetadataTooLarge refuses usage whose metadata, as compact JSON, is
// longer than billing.MaxUsageMetadataBytes.
var ErrUsageMetadataTooLarge = apperr.Invalidf("metadata must be at most %d bytes as compact JSON", billing.MaxUsageMetadataBytes).WithParam("metadata")

// usageMetadata is an item's metadata as the event store keeps it: compact
// JSON, refused past billing.MaxUsageMetadataBytes.
func usageMetadata(m map[string]any) (string, error) {
	if len(m) == 0 {
		return "", nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return "", apperr.Invalidf("metadata must be a JSON object").WithParam("metadata")
	}
	raw := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(raw) > billing.MaxUsageMetadataBytes {
		return "", ErrUsageMetadataTooLarge
	}
	return string(raw), nil
}

// UsageItem is one usage report to take in.
type UsageItem struct {
	// EventID is the item's intake identity (eventstore.LeaseEventID or
	// RequestEventID); LeaseID and N number a lease's item.
	EventID          uuid.UUID
	LeaseID          uuid.UUID
	N                int64
	Source, SourceID string
	Customer         identity.CustomerID
	Invoker          string
	Delegated        bool
	Currency         string
	EventType        string
	Dimensions       map[string]int64
	Metadata         map[string]any
	// Amount is the stated amount a dynamic card charges; a failed item's cost.
	Amount     *int64
	Failed     bool
	OccurredAt time.Time
	// AdmittedAt places an item in the windows current when its request was
	// admitted; nil, in those current at intake.
	AdmittedAt *time.Time
	// Spend are the billing policy's spend windows the item counts toward.
	Spend []limits.Claim
	// FailedWindows are the windows a failed item counts toward: its grace,
	// or its delegated invoker's cutoff.
	FailedWindows []FailedUsageWindow
	// FailedKey is where a charged failure posts.
	FailedKey IdempotencyKey
	// metadata is Metadata as stored, set by PrepareUsageTx.
	metadata string
}

// UsageResult is what taking in one item did. Err refuses the item, which
// wrote nothing. Units are what it took from each limit, by key: the amount
// for balance, a spend window or a currency limit, counted units for a named
// one.
type UsageResult struct {
	Err      error
	Amount   int64
	Forgiven int64
	Charges  []billing.UsageCharge
	Units    map[string]int64
}

// UsageIntake is a prepared batch of usage, ready to commit.
type UsageIntake struct {
	settled  *UsageSettlement
	merchant uuid.UUID
	items    []UsageItem
	Results  []UsageResult
	events   []*eventstore.UsageEvent
	priced   []pricedItem
	prints   map[string]ratingCard
	opens    map[uuid.UUID][]limitOpening
	// opened are the customers whose items open a window.
	opened map[uuid.UUID]bool
	failed []failedCharge
	now    time.Time
}

// limitOpening is a session or first-use window a batch opens, or a granted
// window it gives a row.
type limitOpening struct {
	key      string
	window   limits.Window
	capacity int64
}

// failedCharge is what a failed item's grace did not forgive.
type failedCharge struct {
	item   int
	amount int64
}

type account struct {
	customer uuid.UUID
	currency string
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

// PrepareUsageTx validates and prices a batch of the settlement's
// transaction, locates its limit windows and reads its failed items' grace.
// It locks only the customers whose items open a window or charge a failure,
// in id order, and locates their items again under the lock. It writes
// nothing. An item that cannot be priced is refused alone.
func (s *MoneyService) PrepareUsageTx(ctx context.Context, q *gen.Queries, settled *UsageSettlement, items []UsageItem) (*UsageIntake, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if settled == nil || settled.store == nil {
		return nil, eventstore.Required("usage")
	}
	store := settled.store
	merchantID := mid.UUID()
	now := instant(s.now())
	b := &UsageIntake{settled: settled, merchant: merchantID, items: items, Results: make([]UsageResult, len(items)),
		events: make([]*eventstore.UsageEvent, len(items)), prints: map[string]ratingCard{}, opens: map[uuid.UUID][]limitOpening{}, opened: map[uuid.UUID]bool{}, now: now}
	from, to := now.Add(-retention.UsageIngestWindow), now.Add(retention.UsageClockSkew)
	var eventTypes []string
	currencies := s.UsageCurrencies()
	for i := range items {
		it := &items[i]
		it.EventType = strings.TrimSpace(it.EventType)
		it.Invoker = strings.TrimSpace(it.Invoker)
		it.Currency = normalizeCurrency(it.Currency)
		if it.OccurredAt.IsZero() {
			it.OccurredAt = now
		}
		it.OccurredAt = instant(it.OccurredAt)
		metadata, metadataErr := usageMetadata(it.Metadata)
		it.metadata = metadata
		switch {
		case it.EventID == uuid.Nil:
			return nil, fmt.Errorf("usage item %d has no intake identity", i)
		case it.Customer.IsZero():
			b.Results[i].Err = apperr.Invalidf("customer_id required").WithParam("customer_id")
		case it.EventType == "" || len(it.EventType) > 255:
			b.Results[i].Err = apperr.Invalidf("event_type must contain 1 to 255 bytes").WithParam("event_type")
		case !currencies.Supported(ctx, it.Currency):
			b.Results[i].Err = apperr.New(400, billing.CodeCurrencyUnsupported, "currency is not supported").WithParam("currency")
		case it.Amount != nil && *it.Amount < 0:
			b.Results[i].Err = apperr.Invalidf("amount must be nonnegative").WithParam("amount")
		case it.Failed && it.Amount == nil:
			b.Results[i].Err = apperr.Invalidf("failed usage must state amount").WithParam("amount")
		case len(it.Invoker) > 255:
			b.Results[i].Err = apperr.Invalidf("invoker must be at most 255 bytes").WithParam("invoker")
		case it.Delegated && (!it.Failed || it.Invoker == ""):
			b.Results[i].Err = apperr.Invalidf("a delegated invoker reports only failed usage, and names its invoker").WithParam("invoker_type")
		case metadataErr != nil:
			b.Results[i].Err = metadataErr
		case it.OccurredAt.Before(from) || it.OccurredAt.After(to):
			b.Results[i].Err = ErrUsageOutsideIngestWindow
		}
		if b.Results[i].Err == nil && !slices.Contains(eventTypes, it.EventType) {
			eventTypes = append(eventTypes, it.EventType)
		}
	}
	// A meter correction cannot slip between reading the meters and the
	// usage committing.
	if len(eventTypes) > 0 {
		if err := q.ShareUsageEventTypes(ctx, gen.ShareUsageEventTypesParams{MerchantID: merchantID, EventTypes: eventTypes}); err != nil {
			return nil, err
		}
	}
	// Each currency's balance account: a customer's first usage opens it,
	// the mark its periods anchor on.
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
	// Rows, accounts and anchors are made and read for all customers at once.
	if len(accounts) > 0 {
		var customers []uuid.UUID
		balances := make([]ledger.CustomerCurrency, len(accounts))
		for i, a := range accounts {
			if i == 0 || a.customer != accounts[i-1].customer {
				customers = append(customers, a.customer)
			}
			balances[i] = ledger.CustomerCurrency{Customer: a.customer, Currency: a.currency}
		}
		if err := q.EnsureCustomerRows(ctx, gen.EnsureCustomerRowsParams{MerchantID: merchantID, Ids: customers}); err != nil {
			return nil, err
		}
		if err := ledger.New(q, merchantID).EnsureCustomerBalances(ctx, balances); err != nil {
			return nil, err
		}
	}
	anchors, err := s.periodAnchors(ctx, q, merchantID, accounts)
	if err != nil {
		return nil, err
	}
	lapses, err := lotLapses(ctx, q, merchantID, accounts, to)
	if err != nil {
		return nil, err
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
	// Usage counts toward every limit that reads it; only a limit a product
	// or override gives the customer capacity in refuses what it cannot
	// count.
	granted := map[uuid.UUID]map[string]bool{}
	if len(defs) > 0 {
		for _, a := range accounts {
			if _, ok := granted[a.customer]; ok {
				continue
			}
			if granted[a.customer], err = (limits.Customer{Q: q, Events: store, MerchantID: merchantID, CustomerID: a.customer, Now: now}).Granted(ctx); err != nil {
				return nil, err
			}
		}
	}

	priced, err := s.priceItems(ctx, q, b, defs, meters, granted, anchors, lapses)
	if err != nil {
		return nil, err
	}
	b.priced = priced
	if err := s.locateItems(ctx, q, b, defs, priced, nil); err != nil {
		return nil, err
	}
	if err := s.lockWhereGuarded(ctx, q, b, defs, priced); err != nil {
		return nil, err
	}
	if err := b.chargeFailures(ctx, now); err != nil {
		return nil, err
	}
	for i, it := range items {
		if b.Results[i].Err != nil {
			continue
		}
		b.events[i] = b.event(i, it, priced[i])
	}
	return b, nil
}

// pricedItem is an item's pricing: its period and lapse segment, and what
// each named-unit limit that reads it counts.
type pricedItem struct {
	period  time.Time
	end     time.Time
	lapse   time.Time
	units   map[string]int64
	charges []itemCharge
	limits  []eventstore.UsageLimit
}

type itemCharge struct {
	meter, cell, print string
	model              catalog.Model
	quantity, stated   int64
	amount, discount   int64
}

// priceItems resolves each item's cards and quantities and prices them
// provisionally: each group sharing a period, meter cell and print is
// charged R(Q + Σq) - R(Q) at the period position Q the running totals hold,
// never below zero, shared out by quantity. Where a volume tier's price
// falls, the fall is the group's discount, shared the same way, for history:
// amounts less discounts sum to the period's price.
func (s *MoneyService) priceItems(ctx context.Context, q *gen.Queries, b *UsageIntake, defs []limits.Definition, meters map[string]limits.Meter,
	granted map[uuid.UUID]map[string]bool, anchors map[account]periodAnchor, lapses map[account][]time.Time) ([]pricedItem, error) {
	priced := make([]pricedItem, len(b.items))
	type member struct {
		item             int
		quantity, stated int64
	}
	type group struct {
		account
		key     cellKey
		print   string
		card    ratingCard
		members []member
		total   int64
	}
	groups := map[string]*group{}
	current := map[string][]ratingCard{}
	cardErr := map[string]error{}
	earliest := time.Time{}
	var customers []uuid.UUID
	for i, it := range b.items {
		if b.Results[i].Err != nil {
			continue
		}
		a := account{it.Customer.UUID(), it.Currency}
		period, end, err := anchors[a].period(it.OccurredAt)
		if err != nil {
			return nil, err
		}
		priced[i] = pricedItem{period: period, end: end, lapse: lapseSegment(lapses[a], period, it.OccurredAt), units: map[string]int64{}}
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
			priced[i].units[def.Key] = units
		}
		if b.Results[i].Err != nil {
			continue
		}
		key := it.EventType + "\x00" + it.Currency
		if _, ok := current[key]; !ok && cardErr[key] == nil {
			current[key], cardErr[key] = usageCards(ctx, q, b.merchant, it.EventType, it.Currency)
		}
		if cardErr[key] != nil {
			var coded *apperr.Error
			if !errors.As(cardErr[key], &coded) {
				return nil, cardErr[key]
			}
			b.Results[i].Err = cardErr[key]
			continue
		}
		cards := current[key]
		quantities, err := usageQuantities(cards, eventFields{Dimensions: it.Dimensions, Metadata: it.Metadata, Amount: it.Amount})
		if err != nil {
			b.Results[i].Err = settleRefusal(err)
			continue
		}
		for _, u := range quantities {
			if err := u.priceable(); err != nil {
				b.Results[i].Err = apperr.New(http.StatusUnprocessableEntity, billing.CodeUsageNotPriced, err.Error()).WithParam("event_type")
				break
			}
			print, err := cardPrint(u.card)
			if err != nil {
				return nil, err
			}
			b.prints[print] = u.card
			a := account{it.Customer.UUID(), it.Currency}
			k := strings.Join([]string{a.customer.String(), a.currency, period.Format(time.RFC3339Nano), u.card.MeterKey, u.cell, print}, "\x00")
			g := groups[k]
			if g == nil {
				g = &group{account: a, key: cellKey{period, u.card.MeterKey, u.cell}, print: print, card: u.card}
				groups[k] = g
			}
			if g.total > maxQuantity-u.quantity {
				b.Results[i].Err = apperr.Invalidf("meter %q quantity overflows", u.card.MeterKey)
				break
			}
			stated := int64(0)
			if u.card.Price.Model == catalog.ModelDynamic && u.stated != nil {
				stated = *u.stated
			}
			g.members = append(g.members, member{item: i, quantity: u.quantity, stated: stated})
			g.total += u.quantity
		}
		if b.Results[i].Err == nil && len(quantities) > 0 {
			customers = append(customers, it.Customer.UUID())
			if earliest.IsZero() || period.Before(earliest) {
				earliest = period
			}
		}
	}
	// The period positions: what the running totals hold, posted or not.
	position := map[account]map[cellKey]int64{}
	if len(customers) > 0 {
		slices.SortFunc(customers, compareUUID)
		rows, err := q.ListUsagePositions(ctx, gen.ListUsagePositionsParams{MerchantID: b.merchant, CustomerIds: slices.Compact(customers), PeriodStartsAt: earliest})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			a := account{r.CustomerID, r.Currency}
			if position[a] == nil {
				position[a] = map[cellKey]int64{}
			}
			k := cellKey{r.PeriodStartsAt.UTC(), r.MeterKey, r.Cell}
			position[a][k] = addSat(position[a][k], r.Quantity)
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
		return cmp.Or(compareUUID(x.customer, y.customer), strings.Compare(x.currency, y.currency), x.key.period.Compare(y.key.period),
			strings.Compare(x.key.meter, y.key.meter), strings.Compare(x.key.cell, y.key.cell), strings.Compare(x.print, y.print))
	})
	for _, g := range ordered {
		if position[g.account] == nil {
			position[g.account] = map[cellKey]int64{}
		}
		before := position[g.account][g.key]
		shares, discounts := make([]int64, len(g.members)), make([]int64, len(g.members))
		if g.card.Price.Model == catalog.ModelDynamic {
			for j, m := range g.members {
				if m.quantity > 0 {
					shares[j] = m.stated
				}
			}
		} else {
			amount, err := marginalPrice(g.card, g.key.cell, before, g.total)
			if err != nil {
				return nil, err
			}
			quantities := make([]int64, len(g.members))
			for j, m := range g.members {
				quantities[j] = m.quantity
			}
			shares, discounts = share(max(amount, 0), quantities), share(max(-amount, 0), quantities)
		}
		position[g.account][g.key] = addSat(before, g.total)
		for j, m := range g.members {
			p := &priced[m.item]
			p.charges = append(p.charges, itemCharge{meter: g.key.meter, cell: g.key.cell, print: g.print, model: g.card.Price.Model,
				quantity: m.quantity, stated: m.stated, amount: shares[j], discount: discounts[j]})
		}
	}
	for i := range b.items {
		if b.Results[i].Err != nil || b.items[i].Failed {
			continue
		}
		p := &priced[i]
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
	return priced, nil
}

// lockWhereGuarded takes, in id order, the lock of each customer whose items
// open a session or first-use window or charge a failure, and locates those
// customers' items again under it: a window another intake opened meanwhile
// is found, not opened twice.
func (s *MoneyService) lockWhereGuarded(ctx context.Context, q *gen.Queries, b *UsageIntake, defs []limits.Definition, priced []pricedItem) error {
	guarded := map[uuid.UUID]bool{}
	for c := range b.opened {
		guarded[c] = true
	}
	for i, it := range b.items {
		if b.Results[i].Err == nil && it.Failed && !it.Delegated && len(it.FailedWindows) > 0 {
			guarded[it.Customer.UUID()] = true
		}
	}
	if len(guarded) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(guarded))
	for c := range guarded {
		ids = append(ids, c)
	}
	slices.SortFunc(ids, compareUUID)
	for _, c := range ids {
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: b.merchant, ID: c}); err != nil {
			return err
		}
	}
	if len(b.opened) == 0 {
		return nil
	}
	again := b.opened
	b.opened = map[uuid.UUID]bool{}
	for c := range again {
		delete(b.opens, c)
	}
	for i, it := range b.items {
		if b.Results[i].Err == nil && again[it.Customer.UUID()] {
			priced[i].limits, b.Results[i].Units = nil, nil
		}
	}
	return s.locateItems(ctx, q, b, defs, priced, again)
}

// locateItems finds the limit windows each item counts in: its hold's for a
// limit the hold holds, those current when it was admitted for the rest, its
// spend windows, and a failed item's grace or cutoff windows. A session or
// first-use window not yet open opens at the item's second; later items of
// the batch inside it count there too. only narrows it to some customers'
// items.
func (s *MoneyService) locateItems(ctx context.Context, q *gen.Queries, b *UsageIntake, defs []limits.Definition, priced []pricedItem, only map[uuid.UUID]bool) error {
	type located struct {
		customer uuid.UUID
		at       time.Time
	}
	windows := map[located]map[string]limits.Claim{}
	for i := range b.items {
		it, res, p := b.items[i], &b.Results[i], &priced[i]
		if res.Err != nil || only != nil && !only[it.Customer.UUID()] {
			continue
		}
		customer := it.Customer.UUID()
		at := admittedAt(it, b.now)
		if it.Failed {
			periods, err := failedUsagePeriods(it.FailedWindows, at)
			if err != nil {
				return err
			}
			for _, fp := range periods {
				end := fp.end
				if !end.After(b.now) {
					continue
				}
				p.limits = append(p.limits, eventstore.UsageLimit{Key: failedLimitKey(it.Currency, fp.window.Key, it.Delegated, it.Invoker),
					WindowStartsAt: fp.start, WindowEndsAt: &end, Units: *it.Amount})
			}
			res.Units = map[string]int64{}
			continue
		}
		amount := res.Amount
		res.Units = map[string]int64{catalog.BalanceLimit: amount}
		// A window that ended before the intake is over: usage counts toward
		// it no more (a late resend's included) and opens nothing; its rows
		// are deleted a day after its end (retention.LimitWindows).
		add := func(key string, w limits.Window, units int64) {
			if units == 0 || over(w, b.now) {
				return
			}
			p.limits = append(p.limits, eventstore.UsageLimit{Key: key, WindowStartsAt: w.StartsAt.UTC(), WindowEndsAt: w.EndsAt, Units: units})
			res.Units[key] = addSat(res.Units[key], units)
		}
		for _, c := range it.Spend {
			add(c.Key, c.Window, amount)
		}
		if len(defs) == 0 {
			continue
		}
		// Windows are read once per customer and second, at its last instant
		// (or now): a grant made earlier in the item's second is in force, as
		// a window the item opens starts at that second.
		k := located{customer, at.Truncate(time.Second)}
		current, ok := windows[k]
		if !ok {
			var err error
			readAt := k.at.Add(time.Second - time.Microsecond)
			if b.now.Before(readAt) {
				readAt = b.now
			}
			reader := limits.Customer{Q: q, Events: b.settled.store, MerchantID: b.merchant, CustomerID: customer, Now: readAt,
				Period: func(ctx context.Context, unit string, at time.Time) (time.Time, time.Time, error) {
					return s.PricingPeriod(ctx, q, b.merchant, customer, unit, at)
				}}
			if current, err = reader.Windows(ctx, defs); err != nil {
				return err
			}
			windows[k] = current
		}
		// Every limit takes the usage in its window: a granted one's, or an
		// ungranted one's row that exists.
		for _, def := range defs {
			c, ok := current[def.Key]
			if !ok {
				continue
			}
			units, counts := p.units[def.Key]
			if def.Unit == it.Currency {
				units, counts = amount, true
			}
			if !counts || units == 0 {
				continue
			}
			if c.Opens && over(limits.Opening(def.Reset, at), b.now) || !c.Opens && over(bucketed(def.Reset, c.Window, at), b.now) {
				continue
			}
			w := c.Window
			switch {
			case c.Opens:
				w = b.opening(customer, def, c, at)
			case !c.Ungranted && !c.Stored && def.Reset.Kind != catalog.ResetRolling && def.Reset.Kind != catalog.ResetRate:
				// Granted usage gives its window a row: should the grant lapse,
				// later usage still counts there while the window lasts.
				b.keep(customer, def.Key, c)
			}
			add(def.Key, bucketed(def.Reset, w, at), units)
		}
	}
	return nil
}

// opening is the session or first-use window an item admitted at opens, or
// the one an earlier item of the batch opened around it.
func (b *UsageIntake) opening(customer uuid.UUID, def limits.Definition, c limits.Claim, at time.Time) limits.Window {
	for _, o := range b.opens[customer] {
		if o.key == def.Key && !at.Before(o.window.StartsAt) && (o.window.EndsAt == nil || at.Before(*o.window.EndsAt)) {
			return o.window
		}
	}
	w := limits.Opening(def.Reset, at)
	b.opens[customer] = append(b.opens[customer], limitOpening{key: def.Key, window: w, capacity: c.Capacity})
	b.opened[customer] = true
	return w
}

// keep records the row of a granted window that has none.
func (b *UsageIntake) keep(customer uuid.UUID, key string, c limits.Claim) {
	for _, o := range b.opens[customer] {
		if o.key == key && o.window.StartsAt.Equal(c.Window.StartsAt) {
			return
		}
	}
	b.opens[customer] = append(b.opens[customer], limitOpening{key: key, window: c.Window, capacity: c.Capacity})
}

// over reports a window that ended at or before now.
func over(w limits.Window, now time.Time) bool {
	return w.EndsAt != nil && !w.EndsAt.After(now)
}

// bucketed is where a limit counts an item in window w: a rolling or rate
// limit's bucket of the item's instant.
func bucketed(reset catalog.LimitReset, w limits.Window, at time.Time) limits.Window {
	if reset.Kind != catalog.ResetRolling && reset.Kind != catalog.ResetRate {
		return w
	}
	return limits.BucketAt(reset.WindowSeconds, at)
}

// failedLimitKey names a failed-usage window among limit usage: the
// customer's grace, or a delegated invoker's cutoff.
func failedLimitKey(currency, window string, delegated bool, invoker string) string {
	key := "failed:" + currency + ":" + window
	if delegated {
		key += ":" + invoker
	}
	return key
}

// chargeFailures charges each failed item what its grace does not forgive:
// the grace its windows have left in the event store, less what the
// transaction's earlier batches and the batch's earlier items took (the
// store takes none of the transaction's usage before it commits). A
// delegated invoker's failure, or one with no grace windows, is not charged.
func (b *UsageIntake) chargeFailures(ctx context.Context, now time.Time) error {
	var customers []billing.CustomerID
	for i, it := range b.items {
		if b.Results[i].Err == nil && it.Failed && !it.Delegated && len(it.FailedWindows) > 0 {
			customers = append(customers, billing.CustomerID(it.Customer.UUID()))
		}
	}
	if len(customers) == 0 {
		return nil
	}
	rows, err := b.settled.store.LimitUsage(ctx, customers, now.Add(-retention.UsageIngestWindow))
	if err != nil {
		return err
	}
	type window struct {
		customer uuid.UUID
		key      string
		start    time.Time
	}
	used := map[window]int64{}
	for _, r := range rows {
		if strings.HasPrefix(r.Key, "failed:") {
			used[window{r.CustomerID.UUID(), r.Key, r.WindowStartsAt}] += r.Units
		}
	}
	for _, e := range b.settled.events {
		for _, l := range e.Limits {
			if strings.HasPrefix(l.Key, "failed:") {
				used[window{e.CustomerID.UUID(), l.Key, l.WindowStartsAt}] += l.Units
			}
		}
	}
	for i, it := range b.items {
		res := &b.Results[i]
		if res.Err != nil || !it.Failed {
			continue
		}
		stated := *it.Amount
		res.Charges = []billing.UsageCharge{}
		var charge int64
		if !it.Delegated && len(it.FailedWindows) > 0 {
			periods, err := failedUsagePeriods(it.FailedWindows, admittedAt(it, now))
			if err != nil {
				return err
			}
			var left int64
			for j, p := range periods {
				w := window{it.Customer.UUID(), failedLimitKey(it.Currency, p.window.Key, false, ""), p.start}
				if remaining := max(p.window.Limit-used[w], 0); j == 0 || remaining < left {
					left = remaining
				}
			}
			charge = max(stated-left, 0)
			for _, p := range periods {
				used[window{it.Customer.UUID(), failedLimitKey(it.Currency, p.window.Key, false, ""), p.start}] += stated
			}
		}
		if charge > 0 {
			if it.FailedKey.IsZero() {
				return fmt.Errorf("a charged failure needs its ledger key")
			}
			b.failed = append(b.failed, failedCharge{item: i, amount: charge})
		}
		res.Amount, res.Forgiven = charge, stated-charge
	}
	return nil
}

// admittedAt is when an item's request was admitted: its windows' instant.
func admittedAt(it UsageItem, now time.Time) time.Time {
	if it.AdmittedAt != nil && it.AdmittedAt.Before(now) {
		return it.AdmittedAt.UTC()
	}
	return now
}

// event is an item as the event store keeps it.
func (b *UsageIntake) event(i int, it UsageItem, p pricedItem) *eventstore.UsageEvent {
	res := b.Results[i]
	outcome := eventstore.UsageSucceeded
	charged, forgiven := int64(0), int64(0)
	if it.Failed {
		outcome, charged, forgiven = eventstore.UsageFailed, res.Amount, res.Forgiven
	}
	admitted := admittedAt(it, b.now)
	invokerType := string(billing.InvokerTypeCustomer)
	if it.Delegated {
		invokerType = string(billing.InvokerTypeDelegated)
	}
	e := &eventstore.UsageEvent{EventID: it.EventID, CustomerID: billing.CustomerID(it.Customer.UUID()), Currency: it.Currency, EventType: it.EventType,
		Invoker: it.Invoker, InvokerType: invokerType, Outcome: outcome, Source: it.Source, SourceID: it.SourceID, LeaseID: it.LeaseID,
		N: uint64(max(it.N, 0)), OccurredAt: it.OccurredAt, AdmittedAt: admitted, IngestedAt: b.now, PeriodStartsAt: p.period,
		Dimensions: it.Dimensions, Metadata: it.metadata, Stated: it.Amount,
		Charged: charged, Forgiven: forgiven, Limits: p.limits}
	for _, c := range p.charges {
		e.Charges = append(e.Charges, eventstore.UsageCharge{Meter: c.meter, Cell: c.cell, Print: c.print, Quantity: c.quantity, Stated: c.stated,
			Amount: c.amount, Discount: c.discount})
	}
	return e
}

// CommitUsageTx, in the caller's transaction: queues the customers' usage for
// posting and holds its queue rows, records the card prints, adds each item's
// quantities and provisional price to its lease's running totals, opens the
// session and first-use windows and posts what failed items charge. The
// caller settles each lease number once, in this transaction, so each item is
// in the totals once. The batch's events and the balances it moved join its
// settlement, which the event store takes once the transaction commits.
func (s *MoneyService) CommitUsageTx(ctx context.Context, q *gen.Queries, b *UsageIntake) error {
	if b == nil {
		return nil
	}
	type mark struct {
		due    time.Time
		amount int64
	}
	marks := map[account]*mark{}
	var accounts []account
	var zone *time.Location
	var lease uuid.UUID
	var err error
	for i, e := range b.events {
		if e == nil {
			continue
		}
		lease = e.LeaseID
		a := account{e.CustomerID.UUID(), e.Currency}
		m := marks[a]
		if m == nil {
			m = &mark{}
			marks[a] = m
			accounts = append(accounts, a)
		}
		m.amount = addSat(m.amount, b.Results[i].Amount)
		if len(e.Charges) == 0 {
			continue
		}
		// Priced usage is pending until a posting takes it; a failed item's
		// charge posts below.
		if zone == nil {
			if zone, err = s.AccountingZone(ctx); err != nil {
				return err
			}
		}
		if due := dayAfter(zone, e.OccurredAt); m.due.IsZero() || due.Before(m.due) {
			m.due = due
		}
	}
	slices.SortFunc(accounts, func(x, y account) int {
		return cmp.Or(compareUUID(x.customer, y.customer), strings.Compare(x.currency, y.currency))
	})
	// Before the usage: a posting either waits for this transaction or
	// finished before any of it was settled.
	if err := b.queueUsage(ctx, q, accounts, func(a account) time.Time { return marks[a].due }); err != nil {
		return err
	}
	prints := make([]string, 0, len(b.prints))
	for p := range b.prints {
		prints = append(prints, p)
	}
	slices.Sort(prints)
	for _, p := range prints {
		raw, err := json.Marshal(b.prints[p])
		if err != nil {
			return err
		}
		if err := q.InsertUsageCardPrint(ctx, gen.InsertUsageCardPrintParams{MerchantID: b.merchant, Print: p, Card: raw, Now: b.now}); err != nil {
			return err
		}
	}
	if err := b.addTotals(ctx, q, lease); err != nil {
		return err
	}
	customers := make([]uuid.UUID, 0, len(b.opens))
	for c := range b.opens {
		customers = append(customers, c)
	}
	slices.SortFunc(customers, compareUUID)
	for _, c := range customers {
		opens := b.opens[c]
		slices.SortFunc(opens, func(x, y limitOpening) int {
			return cmp.Or(strings.Compare(x.key, y.key), x.window.StartsAt.Compare(y.window.StartsAt))
		})
		for _, o := range opens {
			if err := q.EnsureLimitWindow(ctx, gen.EnsureLimitWindowParams{MerchantID: b.merchant, CustomerID: c, LimitKey: o.key,
				WindowStartsAt: o.window.StartsAt, WindowEndsAt: o.window.EndsAt, Capacity: o.capacity, AsOf: b.now}); err != nil {
				return err
			}
		}
	}
	for _, f := range b.failed {
		it := b.items[f.item]
		if _, _, _, err := s.spendBalanceThenOwedTx(ctx, q, it.Customer, it.Invoker, it.Currency, it.FailedKey, f.amount, true); err != nil {
			return err
		}
	}
	for _, e := range b.events {
		if e != nil {
			b.settled.events = append(b.settled.events, *e)
		}
	}
	for _, a := range accounts {
		if c := (UsageAccount{Customer: a.customer, Currency: a.currency}); marks[a].amount > 0 && !slices.Contains(b.settled.changed, c) {
			b.settled.changed = append(b.settled.changed, c)
		}
	}
	return nil
}

// queueUsage queues each account's usage for posting, due when due says (a
// zero due queues nothing). Most accounts already have a queue row due no
// later: those are only held, by key share, so a posting (which locks the
// row for update) waits for this transaction, and intakes never wait on each
// other. The rest are written.
func (b *UsageIntake) queueUsage(ctx context.Context, q *gen.Queries, accounts []account, due func(account) time.Time) error {
	byCurrency := map[string][]uuid.UUID{}
	var currencies []string
	for _, a := range accounts {
		if due(a).IsZero() {
			continue
		}
		if _, ok := byCurrency[a.currency]; !ok {
			currencies = append(currencies, a.currency)
		}
		byCurrency[a.currency] = append(byCurrency[a.currency], a.customer)
	}
	slices.Sort(currencies)
	for _, currency := range currencies {
		held, err := q.ShareUsagePending(ctx, gen.ShareUsagePendingParams{MerchantID: b.merchant, Currency: currency, CustomerIds: byCurrency[currency]})
		if err != nil {
			return err
		}
		have := make(map[uuid.UUID]time.Time, len(held))
		for _, h := range held {
			have[h.CustomerID] = h.DueAt
		}
		write := gen.UpsertUsagePendingParams{MerchantID: b.merchant, Currency: currency, Now: b.now}
		for _, c := range byCurrency[currency] {
			d := due(account{c, currency})
			if h, ok := have[c]; ok && !d.Before(h) {
				continue
			}
			write.CustomerIds, write.DueAts = append(write.CustomerIds, c), append(write.DueAts, d)
		}
		if len(write.CustomerIds) == 0 {
			continue
		}
		if err := q.UpsertUsagePending(ctx, write); err != nil {
			return err
		}
	}
	return nil
}

// totalKey is a lease's running total: a customer's meter cell under one
// card print in a pricing period's lapse segment.
type totalKey struct {
	account
	period, end, lapse time.Time
	meter, cell, print string
}

// addTotals adds each item's quantities and provisional price to the lease's
// running totals, one row per key. Only the lease's syncs, which serialize
// on it, write its rows.
func (b *UsageIntake) addTotals(ctx context.Context, q *gen.Queries, lease uuid.UUID) error {
	type total struct{ quantity, stated, requests, amount int64 }
	sums := map[totalKey]*total{}
	var keys []totalKey
	for i, e := range b.events {
		if e == nil || b.items[i].Failed {
			continue
		}
		p := b.priced[i]
		for _, c := range p.charges {
			k := totalKey{account: account{e.CustomerID.UUID(), e.Currency}, period: p.period, end: p.end, lapse: p.lapse, meter: c.meter, cell: c.cell, print: c.print}
			t := sums[k]
			if t == nil {
				t = &total{}
				sums[k] = t
				keys = append(keys, k)
			}
			t.quantity, t.stated = addSat(t.quantity, c.quantity), addSat(t.stated, c.stated)
			t.requests, t.amount = t.requests+1, addSat(t.amount, c.amount)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	slices.SortFunc(keys, func(x, y totalKey) int {
		return cmp.Or(compareUUID(x.customer, y.customer), strings.Compare(x.currency, y.currency), x.period.Compare(y.period),
			strings.Compare(x.meter, y.meter), strings.Compare(x.cell, y.cell), strings.Compare(x.print, y.print), x.lapse.Compare(y.lapse))
	})
	arg := gen.AddUsageTotalsParams{MerchantID: b.merchant, LeaseID: lease, Now: b.now}
	for _, k := range keys {
		t := sums[k]
		arg.CustomerIds, arg.Currencies = append(arg.CustomerIds, k.customer), append(arg.Currencies, k.currency)
		arg.PeriodStartsAts, arg.PeriodEndsAts = append(arg.PeriodStartsAts, k.period), append(arg.PeriodEndsAts, k.end)
		arg.MeterKeys, arg.Cells, arg.Prints = append(arg.MeterKeys, k.meter), append(arg.Cells, k.cell), append(arg.Prints, k.print)
		arg.LapseAts = append(arg.LapseAts, k.lapse)
		arg.Quantities, arg.Stateds = append(arg.Quantities, t.quantity), append(arg.Stateds, t.stated)
		arg.Requests, arg.Amounts = append(arg.Requests, t.requests), append(arg.Amounts, t.amount)
	}
	return q.AddUsageTotals(ctx, arg)
}

// lotLapses are when each account's credit lots that hold units lapse, by
// through: where intake segments its usage.
func lotLapses(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, accounts []account, through time.Time) (map[account][]time.Time, error) {
	byCurrency := map[string][]uuid.UUID{}
	var currencies []string
	for _, a := range accounts {
		if _, ok := byCurrency[a.currency]; !ok {
			currencies = append(currencies, a.currency)
		}
		byCurrency[a.currency] = append(byCurrency[a.currency], a.customer)
	}
	slices.Sort(currencies)
	out := map[account][]time.Time{}
	for _, currency := range currencies {
		rows, err := q.ListLotLapses(ctx, gen.ListLotLapsesParams{MerchantID: merchantID, Currency: currency, CustomerIds: byCurrency[currency], Through: through})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			a := account{r.CustomerID, currency}
			out[a] = append(out[a], r.ExpiresAt.UTC())
		}
	}
	return out, nil
}

// lapseSegment is where usage at occurred counts in its period: the latest
// lapse at or before it, else the period's start. A posting before a lot's
// lapse takes the segments before it, exactly the usage that occurred before.
func lapseSegment(lapses []time.Time, period, occurred time.Time) time.Time {
	at := period
	for _, l := range lapses {
		if !l.After(occurred) && l.After(at) {
			at = l
		}
	}
	return at
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

// cardPrint names a rate card's terms: usage priced under two revisions of a
// card is priced card by card.
func cardPrint(card ratingCard) (string, error) {
	raw, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8]), nil
}

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
// on: the usage invoices' boundary and anchor.
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

// periodAnchorFor is a customer's period anchor and boundary in a currency.
func (s *MoneyService) periodAnchorFor(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, currency string) (periodAnchor, error) {
	a := account{customerID, currency}
	anchors, err := s.periodAnchors(ctx, q, merchantID, []account{a})
	return anchors[a], err
}

// periodAnchors are accounts' period anchors and boundaries: the merchant's
// terms are read once per currency, the customers' anchors in one read.
func (s *MoneyService) periodAnchors(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, accounts []account) (map[account]periodAnchor, error) {
	out := make(map[account]periodAnchor, len(accounts))
	byCurrency := map[string][]uuid.UUID{}
	var currencies []string
	for _, a := range accounts {
		if _, ok := byCurrency[a.currency]; !ok {
			currencies = append(currencies, a.currency)
		}
		byCurrency[a.currency] = append(byCurrency[a.currency], a.customer)
	}
	boundary := ""
	for _, currency := range currencies {
		customers := byCurrency[currency]
		cfg, cur, err := s.usageBillingConfig(ctx, currency)
		if err != nil {
			return nil, err
		}
		if cur != nil {
			rows, err := q.ListCustomersMoneySettings(ctx, gen.ListCustomersMoneySettingsParams{MerchantID: merchantID, CustomerIds: customers,
				RowLimit: int32(min(len(customers)*len(catalog.Currencies()), math.MaxInt32))}) // #nosec G115 -- clamped to int32
			if err != nil {
				return nil, err
			}
			have := map[uuid.UUID]gen.BillingMoneySetting{}
			for _, r := range rows {
				if r.Currency == currency {
					have[r.CustomerID] = r
				}
			}
			for _, c := range customers {
				row, ok := have[c]
				if !ok || row.UsageBillingAnchor == nil || row.UsageNextBillAt == nil {
					if row, err = s.ensureUsageBillingAccount(ctx, q, c, currency, cfg); err != nil {
						return nil, err
					}
				}
				out[account{c, currency}] = periodAnchor{anchor: *row.UsageBillingAnchor, boundary: BillingBoundaryAnniversary}
			}
			continue
		}
		if boundary == "" {
			if boundary, err = s.billingPeriodBoundary(ctx); err != nil && !errors.Is(err, merchantconfig.ErrNoSettings) {
				return nil, err
			}
		}
		if boundary != BillingBoundaryAnniversary {
			for _, c := range customers {
				out[account{c, currency}] = periodAnchor{boundary: boundary}
			}
			continue
		}
		rows, err := q.ListCustomerPeriodAnchors(ctx, gen.ListCustomerPeriodAnchorsParams{MerchantID: merchantID, Currency: currency, CustomerIds: customers})
		if err != nil {
			return nil, fmt.Errorf("customer period anchor: %w", err)
		}
		for _, r := range rows {
			out[account{r.CustomerID, currency}] = periodAnchor{anchor: r.PeriodAnchor, boundary: boundary}
		}
	}
	return out, nil
}

// UsageCurrencies answers what usage may be priced and spent in: a
// currency, or one of the units the merchant declares, read once.
type UsageCurrencies struct {
	s     *MoneyService
	units *currency.Registry
}

func (s *MoneyService) UsageCurrencies() *UsageCurrencies { return &UsageCurrencies{s: s} }

// Supported reports whether usage may be in code.
func (u *UsageCurrencies) Supported(ctx context.Context, code string) bool {
	if moneyutil.ValidateCurrency(code) == nil {
		return true
	}
	if moneyutil.ValidateBalanceCurrency(code) != nil {
		return false
	}
	if u.units == nil {
		reg, err := merchantconfig.NewStore(u.s.db).Registry(ctx)
		if err != nil {
			return false
		}
		u.units = &reg
	}
	_, ok := u.units.Unit(code)
	return ok
}
