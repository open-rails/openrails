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

// Usage is taken in, not settled: each item becomes one event in the event
// store, carrying its priced meter cells (under the card print in force or
// pinned, in its pricing period), a provisional amount (the marginal price at
// the period position intake read: what limits and the per-minute rate
// count) and the limit windows that count it, located when it was admitted.
// Intake then marks the customer's usage unposted; the month-end close or the
// next balance movement posts it. A failed item is not priced: what its grace
// leaves is charged at once, with its own ledger legs.
//
// Intake is two steps in the caller's transaction: PrepareUsageTx validates,
// prices and locates, writing nothing; then CommitUsageTx inserts the events
// (skipping those an earlier attempt inserted), waits until they read back,
// and writes the PostgreSQL side. It is append-only and takes no customer's
// lock, except to change what that lock guards: to open a session or
// first-use window (two first uses in different seconds would open two), or
// to charge a failed item's grace (a balance movement). Concurrent intakes of
// one customer may price from the same period position; postings price the
// period's total, so only provisional amounts can differ. An event's identity
// never depends on a clock read per attempt: a capture occurred when its
// request was admitted, and an item an earlier attempt inserted keeps the
// instants that attempt stamped.

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
	// Resent is an item an earlier attempt inserted.
	Resent bool
}

// UsageIntake is a prepared batch of usage, ready to commit.
type UsageIntake struct {
	store    *eventstore.Store
	merchant uuid.UUID
	items    []UsageItem
	Results  []UsageResult
	events   []*eventstore.UsageEvent
	existing map[uuid.UUID]eventstore.Existing
	prints   map[string]ratingCard
	opens    map[uuid.UUID][]limitOpening
	// opened are the customers whose items open a window.
	opened  map[uuid.UUID]bool
	failed  []failedCharge
	changed []UsageAccount
	now     time.Time
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

// PrepareUsageTx validates and prices a batch, locates its limit windows and
// reads its failed items' grace. It locks only the customers whose items
// open a window or charge a failure, in id order, and locates their items
// again under the lock. It writes nothing. An item that cannot be priced is
// refused alone.
func (s *MoneyService) PrepareUsageTx(ctx context.Context, q *gen.Queries, store *eventstore.Store, items []UsageItem) (*UsageIntake, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if store == nil {
		return nil, eventstore.Required("usage")
	}
	merchantID := mid.UUID()
	now := instant(s.now())
	b := &UsageIntake{store: store, merchant: merchantID, items: items, Results: make([]UsageResult, len(items)),
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
	var keys []eventstore.UsageKey
	for i, it := range items {
		if b.Results[i].Err != nil {
			continue
		}
		keys = append(keys, eventstore.UsageKey{CustomerID: billing.CustomerID(it.Customer.UUID()), EventID: it.EventID})
		a := account{it.Customer.UUID(), it.Currency}
		if !seen[a] {
			seen[a] = true
			accounts = append(accounts, a)
		}
	}
	slices.SortFunc(accounts, func(x, y account) int {
		return cmp.Or(compareUUID(x.customer, y.customer), strings.Compare(x.currency, y.currency))
	})
	ensured := map[uuid.UUID]bool{}
	anchors := map[account]periodAnchor{}
	l := ledger.New(q, merchantID)
	for _, a := range accounts {
		if !ensured[a.customer] {
			if err := ensureCustomer(ctx, q, merchantID, a.customer); err != nil {
				return nil, err
			}
			ensured[a.customer] = true
		}
		if _, err := l.EnsureCustomerBalance(ctx, a.customer, a.currency); err != nil {
			return nil, err
		}
		anchor, err := s.periodAnchorFor(ctx, q, merchantID, a.customer, a.currency)
		if err != nil {
			return nil, err
		}
		anchors[a] = anchor
	}
	// What an earlier attempt inserted is not inserted again, and keeps the
	// instants it was taken in with: its pricing period and the session and
	// first-use windows it opened.
	if b.existing, err = store.ExistingUsage(ctx, keys); err != nil {
		return nil, err
	}
	for i := range items {
		if prior, ok := b.existing[items[i].EventID]; ok && b.Results[i].Err == nil {
			admitted := prior.AdmittedAt
			items[i].OccurredAt, items[i].AdmittedAt = prior.OccurredAt, &admitted
		}
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

	priced, err := s.priceItems(ctx, q, b, defs, meters, granted, anchors)
	if err != nil {
		return nil, err
	}
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

// pricedItem is an item's pricing: its period, and what each named-unit
// limit that reads it counts.
type pricedItem struct {
	period  time.Time
	units   map[string]int64
	charges []itemCharge
	limits  []eventstore.UsageLimit
}

type itemCharge struct {
	meter, cell, print string
	model              catalog.Model
	quantity, stated   int64
	amount             int64
}

// priceItems resolves each item's cards and quantities and prices them
// provisionally: each group sharing a period, meter cell and print is
// charged R(Q + Σq) - R(Q) at the period position Q intake read, never below
// zero, shared out by quantity.
func (s *MoneyService) priceItems(ctx context.Context, q *gen.Queries, b *UsageIntake, defs []limits.Definition, meters map[string]limits.Meter,
	granted map[uuid.UUID]map[string]bool, anchors map[account]periodAnchor) ([]pricedItem, error) {
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
	var customers []billing.CustomerID
	for i, it := range b.items {
		if b.Results[i].Err != nil {
			continue
		}
		period, _, err := anchors[account{it.Customer.UUID(), it.Currency}].period(it.OccurredAt)
		if err != nil {
			return nil, err
		}
		priced[i] = pricedItem{period: period, units: map[string]int64{}}
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
			customers = append(customers, billing.CustomerID(it.Customer.UUID()))
			if earliest.IsZero() || period.Before(earliest) {
				earliest = period
			}
		}
	}
	// The period positions, as the event store has them.
	position := map[account]map[cellKey]int64{}
	if len(customers) > 0 {
		rows, err := b.store.PeriodQuantities(ctx, customers, "", earliest)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			a := account{r.CustomerID.UUID(), r.Currency}
			if position[a] == nil {
				position[a] = map[cellKey]int64{}
			}
			k := cellKey{r.PeriodStartsAt, r.Meter, r.Cell}
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
		shares := make([]int64, len(g.members))
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
			shares = share(max(amount, 0), quantities)
		}
		position[g.account][g.key] = addSat(before, g.total)
		for j, m := range g.members {
			p := &priced[m.item]
			p.charges = append(p.charges, itemCharge{meter: g.key.meter, cell: g.key.cell, print: g.print, model: g.card.Price.Model,
				quantity: m.quantity, stated: m.stated, amount: shares[j]})
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
				p.limits = append(p.limits, eventstore.UsageLimit{Key: failedLimitKey(it.Currency, fp.window.Key, it.Delegated, it.Invoker),
					WindowStartsAt: fp.start, WindowEndsAt: &end, Units: *it.Amount})
			}
			res.Units = map[string]int64{}
			continue
		}
		amount := res.Amount
		res.Units = map[string]int64{catalog.BalanceLimit: amount}
		add := func(key string, w limits.Window, units int64) {
			if units == 0 {
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
			reader := limits.Customer{Q: q, Events: b.store, MerchantID: b.merchant, CustomerID: customer, Now: readAt,
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
// the grace its windows have left in the event store, less what earlier
// items of the batch took. A delegated invoker's failure, or one with no
// grace windows, is not charged; an item an earlier attempt inserted is
// charged what it was.
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
	rows, err := b.store.LimitUsage(ctx, customers, now.Add(-retention.UsageIngestWindow))
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
	for i, it := range b.items {
		res := &b.Results[i]
		if res.Err != nil || !it.Failed {
			continue
		}
		stated := *it.Amount
		res.Charges = []billing.UsageCharge{}
		if prior, ok := b.existing[it.EventID]; ok {
			res.Amount, res.Forgiven, res.Resent = prior.Charged, prior.Forgiven, true
			if prior.Charged > 0 {
				b.failed = append(b.failed, failedCharge{item: i, amount: prior.Charged})
			}
			continue
		}
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
	amount := res.Amount
	if it.Failed {
		outcome, charged, forgiven, amount = eventstore.UsageFailed, res.Amount, res.Forgiven, res.Amount
	}
	admitted := admittedAt(it, b.now)
	invokerType := string(billing.InvokerTypeCustomer)
	if it.Delegated {
		invokerType = string(billing.InvokerTypeDelegated)
	}
	period := p.period
	if prior, ok := b.existing[it.EventID]; ok {
		period = prior.PeriodStartsAt
	}
	e := &eventstore.UsageEvent{EventID: it.EventID, CustomerID: billing.CustomerID(it.Customer.UUID()), Currency: it.Currency, EventType: it.EventType,
		Invoker: it.Invoker, InvokerType: invokerType, Outcome: outcome, Source: it.Source, SourceID: it.SourceID, LeaseID: it.LeaseID,
		N: uint64(max(it.N, 0)), OccurredAt: it.OccurredAt, AdmittedAt: admitted, IngestedAt: b.now, PeriodStartsAt: period,
		Dimensions: it.Dimensions, Metadata: it.metadata, Amount: amount, Stated: it.Amount,
		Charged: charged, Forgiven: forgiven, Limits: p.limits}
	for _, c := range p.charges {
		e.Charges = append(e.Charges, eventstore.UsageCharge{Meter: c.meter, Cell: c.cell, Print: c.print, Quantity: c.quantity, Stated: c.stated})
	}
	return e
}

// CommitUsageTx inserts the batch's events the store does not hold and waits
// until they read back, then, in the caller's transaction: marks the
// customers' usage unposted, records the card prints, opens the session and
// first-use windows and posts what failed items charge.
// Should the transaction not commit, a resend finds the events and redoes
// only this side, which is idempotent. Changed names the balances the usage
// moved, for the caller to watch (watchUsageTx, WatchUsage).
func (s *MoneyService) CommitUsageTx(ctx context.Context, q *gen.Queries, b *UsageIntake) error {
	if b == nil {
		return nil
	}
	var fresh []eventstore.UsageEvent
	for i, e := range b.events {
		if e == nil {
			continue
		}
		if _, ok := b.existing[e.EventID]; ok {
			b.Results[i].Resent = true
			continue
		}
		fresh = append(fresh, *e)
	}
	if err := b.store.InsertUsage(ctx, fresh); err != nil {
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
	type mark struct {
		since, due time.Time
		amount     int64
	}
	marks := map[account]*mark{}
	var accounts []account
	var zone *time.Location
	var err error
	for i, e := range b.events {
		if e == nil {
			continue
		}
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
		if zone == nil {
			if zone, err = s.accountingZone(ctx); err != nil {
				return err
			}
		}
		due := monthAfter(zone, e.OccurredAt)
		if m.since.IsZero() || e.PeriodStartsAt.Before(m.since) {
			m.since = e.PeriodStartsAt
		}
		if m.due.IsZero() || due.Before(m.due) {
			m.due = due
		}
	}
	slices.SortFunc(accounts, func(x, y account) int {
		return cmp.Or(compareUUID(x.customer, y.customer), strings.Compare(x.currency, y.currency))
	})
	if err := b.markUnposted(ctx, q, accounts, func(a account) (time.Time, time.Time) {
		m := marks[a]
		return m.since, m.due
	}); err != nil {
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
	b.changed = b.changed[:0]
	for _, a := range accounts {
		if marks[a].amount > 0 {
			b.changed = append(b.changed, UsageAccount{Customer: a.customer, Currency: a.currency})
		}
	}
	return nil
}

// Changed are the balances the committed usage took from: one that crosses
// a line of its balance watch queues a look, and one that takes arrears to
// the threshold asks for a usage order.
func (b *UsageIntake) Changed() []UsageAccount {
	if b == nil {
		return nil
	}
	return b.changed
}

// markUnposted marks each account's usage unposted from its period (mark;
// a zero since marks nothing). Most accounts already carry a mark that
// covers it: those are only held, by key share, so a posting (which locks
// the mark for update) waits for this transaction or has already read its
// events, and intakes never wait on each other. The rest are written.
func (b *UsageIntake) markUnposted(ctx context.Context, q *gen.Queries, accounts []account, mark func(account) (since, due time.Time)) error {
	byCurrency := map[string][]uuid.UUID{}
	var currencies []string
	for _, a := range accounts {
		if since, _ := mark(a); since.IsZero() {
			continue
		}
		if _, ok := byCurrency[a.currency]; !ok {
			currencies = append(currencies, a.currency)
		}
		byCurrency[a.currency] = append(byCurrency[a.currency], a.customer)
	}
	slices.Sort(currencies)
	for _, currency := range currencies {
		held, err := q.ShareUsageUnposted(ctx, gen.ShareUsageUnpostedParams{MerchantID: b.merchant, Currency: currency, CustomerIds: byCurrency[currency]})
		if err != nil {
			return err
		}
		have := make(map[uuid.UUID]gen.ShareUsageUnpostedRow, len(held))
		for _, h := range held {
			have[h.CustomerID] = h
		}
		for _, c := range byCurrency[currency] {
			since, due := mark(account{c, currency})
			due = later(due, since.Add(time.Microsecond))
			if h, ok := have[c]; ok && !since.Before(h.SinceAt) && !due.Before(h.DueAt) {
				continue
			}
			if err := q.UpsertUsageUnposted(ctx, gen.UpsertUsageUnpostedParams{MerchantID: b.merchant, CustomerID: c, Currency: currency,
				SinceAt: since, DueAt: due, Now: b.now}); err != nil {
				return err
			}
		}
	}
	return nil
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

// periodAnchorFor is a customer's period anchor and boundary in a currency.
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
