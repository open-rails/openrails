package leases

import (
	"cmp"
	"context"
	"fmt"
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
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// A view is where one customer stands on every limit its requests in one
// currency may claim, read after the sync committed and leaving out the
// syncing lease's own in-flight report, which its gate counts itself. It
// reserves nothing and takes no lock. Usage is read only through
// UsageReader.
//
// Views are read once the sync's transaction committed, in one read-only
// snapshot taken before any event store read: they cover every item whose
// receipt the sync answers (its events were inserted before the commit),
// and an item another sync settles meanwhile is counted at worst twice
// (its events and its in-flight line), never not at all. What other gates
// settled since is what the gate measures as others' additions.

type viewKey struct {
	customer uuid.UUID
	currency string
}

// viewBuild is one view as the sync builds it.
type viewBuild struct {
	key   viewKey
	cap   money.AdmissionCapacity
	view  billing.View
	lines map[string]bool
}

// viewing is what one sync's views read, in their snapshot.
type viewing struct {
	run      *syncRun
	ctx      context.Context
	q        *gen.Queries
	money    *money.MoneyService
	admitter *admission.Admitter
	usage    *storeUsage
	defs     []limits.Definition
	meters   map[string]limits.Meter
	builds   map[viewKey]*viewBuild
	order    []viewKey
	rates    map[viewKey]int64
}

// views answers each ViewParams with its limit set (null once the sync is
// past its work budget), each customer and currency named with its view, and
// each currency and event type named with its pricing, after the sync
// committed. A request it cannot answer (malformed, out of the credential's
// scope, or one whose customer's view or policy fails to resolve) is refused
// alone. A window found at capacity is recorded after the snapshot.
func (r *syncRun) views(ctx context.Context) error {
	if len(r.p.Views) == 0 {
		return nil
	}
	// What the views may be asked in is read before the snapshot.
	currencies := r.Money.UsageCurrencies()
	r.viewable = map[string]bool{}
	for _, vp := range r.p.Views {
		code := money.NormalizeCurrency(string(vp.Currency))
		r.viewable[code] = currencies.Supported(ctx, code)
	}
	var reached []reachedWindow
	err := r.DB.ReadSnapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// The snapshot is taken here, before any event store read.
		if _, err := gen.New(tx).SetConfig(ctx, gen.SetConfigParams{Setting: db.MerchantGUC, Value: r.merchant.String(), IsLocal: true}); err != nil {
			return err
		}
		d := r.DB.NewWithPgxTx(tx)
		v := &viewing{run: r, ctx: ctx, q: gen.New(tx), money: r.Money.WithDatabase(d), admitter: r.Admitter.Bind(d),
			builds: map[viewKey]*viewBuild{}}
		v.usage = &storeUsage{v: v, store: r.DB.EventStore()}
		if err := v.answer(); err != nil {
			return err
		}
		reached = v.usage.reached
		return nil
	})
	if err != nil {
		return err
	}
	q := r.DB.Gen(ctx)
	for _, w := range reached {
		h := limits.Holder{Q: q, MerchantID: r.merchant, CustomerID: w.customer, Now: r.now}
		if err := h.Reach(ctx, w.claim, w.standing, nil); err != nil {
			return err
		}
	}
	return nil
}

func (v *viewing) answer() error {
	r, ctx := v.run, v.ctx
	var err error
	if v.defs, err = limits.Definitions(ctx, v.q, r.merchant); err != nil {
		return err
	}
	if len(v.defs) > 0 {
		if v.meters, err = limits.Meters(ctx, v.q, r.merchant); err != nil {
			return err
		}
	}
	var keys []viewKey
	for _, vp := range r.p.Views {
		if r.checkView(vp) == nil {
			if k := (viewKey{vp.CustomerID.UUID(), money.NormalizeCurrency(string(vp.Currency))}); !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	if err := v.usage.prefetch(ctx, keys); err != nil {
		return err
	}
	if err := v.loadRates(keys); err != nil {
		return err
	}
	type pricingKey struct{ currency, eventType string }
	var priced []pricingKey
	failed := map[viewKey]error{}
	refuse := func(i int, err error) {
		status, details := Details(err)
		if _, coded := asCoded(err); !coded {
			details.Message = "the view could not be resolved"
		}
		r.result.RefusedViews = append(r.result.RefusedViews, billing.ViewRefusal{Index: i, Status: status, Error: &details})
	}
	for i, vp := range r.p.Views {
		if time.Since(r.began) > workBudget {
			// The rest are answered null and asked again.
			break
		}
		if err := r.checkView(vp); err != nil {
			refuse(i, prefixParam(err, fmt.Sprintf("views[%d]", i)))
			continue
		}
		k := viewKey{vp.CustomerID.UUID(), money.NormalizeCurrency(string(vp.Currency))}
		if err := failed[k]; err != nil {
			refuse(i, err)
			continue
		}
		b, err := v.build(k)
		if err != nil {
			if !isolated(err) {
				return err
			}
			failed[k] = err
			refuse(i, err)
			continue
		}
		eventType := strings.TrimSpace(deref(vp.EventType))
		set, err := v.limitSet(b, vp, eventType)
		if err != nil {
			if !isolated(err) {
				return err
			}
			refuse(i, err)
			continue
		}
		r.result.LimitSets[i] = set
		if p := (pricingKey{k.currency, eventType}); eventType != "" && !slices.Contains(priced, p) {
			priced = append(priced, p)
		}
	}
	for _, k := range v.order {
		r.result.Views = append(r.result.Views, v.builds[k].view)
	}
	for _, p := range priced {
		out, err := v.pricing(p.currency, p.eventType)
		if err != nil {
			return err
		}
		r.result.Pricing = append(r.result.Pricing, out)
	}
	return nil
}

// rates are what the viewed customers' running charges accrue per hour, by
// currency.
func (v *viewing) loadRates(keys []viewKey) error {
	var customers []uuid.UUID
	for _, k := range keys {
		if !slices.Contains(customers, k.customer) {
			customers = append(customers, k.customer)
		}
	}
	if len(customers) == 0 {
		return nil
	}
	rows, err := v.q.ListRunningRatesOf(v.ctx, gen.ListRunningRatesOfParams{MerchantID: v.run.merchant, CustomerIds: customers})
	if err != nil {
		return err
	}
	v.rates = map[viewKey]int64{}
	for _, r := range rows {
		v.rates[viewKey{r.CustomerID, r.Currency}] = r.RatePerHour
	}
	return nil
}

// checkView refuses a view request no request of its attributes could be
// decided on: a currency, or a unit the merchant declares.
func (r *syncRun) checkView(vp billing.ViewParams) error {
	switch {
	case vp.CustomerID.IsZero():
		return invalid("customer_id", "customer_id is required")
	case r.Allows != nil && !r.Allows(vp.CustomerID):
		return apperr.New(http.StatusForbidden, billing.CodeServiceCredentialCustomerScopeDenied, "the credential's customer scope does not include the customer").WithParam("customer_id")
	case vp.InvokerType != "" && vp.InvokerType != billing.InvokerTypeCustomer && vp.InvokerType != billing.InvokerTypeDelegated:
		return invalid("invoker_type", `invoker_type must be "customer" or "delegated"`)
	case !r.viewable[money.NormalizeCurrency(string(vp.Currency))]:
		return apperr.New(http.StatusBadRequest, billing.CodeCurrencyUnsupported, "currency is not supported").WithParam("currency")
	}
	return nil
}

// build is the view of one customer and currency: balance, and every
// declared limit the customer is granted in the currency or a named unit.
func (v *viewing) build(k viewKey) (*viewBuild, error) {
	if b := v.builds[k]; b != nil {
		return b, nil
	}
	r, ctx := v.run, v.ctx
	capacity, err := v.usage.Capacity(ctx, k.customer, k.currency)
	if err != nil {
		return nil, err
	}
	b := &viewBuild{key: k, cap: capacity, lines: map[string]bool{}, view: billing.View{CustomerID: billing.CustomerID(k.customer),
		Currency: catalog.Currency(k.currency), AsOf: r.now, Limits: []billing.ViewLimit{{Limit: catalog.BalanceLimit, Unit: k.currency,
			Overrun: catalog.OverrunRequireEstimate, Capacity: capacity.Balance, Held: capacity.Unposted(), Remaining: admission.Available(capacity),
			DrainPerHour: v.rates[k]}}}}
	b.lines[catalog.BalanceLimit] = true
	if len(v.defs) > 0 {
		reader := limits.Customer{Q: v.q, Events: r.DB.EventStore(), MerchantID: r.merchant, CustomerID: k.customer, Now: r.now, ExceptLease: &r.lease.ID,
			Period: func(ctx context.Context, unit string, at time.Time) (time.Time, time.Time, error) {
				return v.money.PricingPeriod(ctx, v.q, r.merchant, k.customer, unit, at)
			}}
		claims, err := reader.Windows(ctx, v.defs)
		if err != nil {
			return nil, err
		}
		for _, def := range v.defs {
			c, ok := claims[def.Key]
			if !ok || c.Ungranted || syntax.LimitUnitCurrency(def.Unit) && def.Unit != k.currency {
				continue
			}
			next, err := reader.Next(ctx, def, c.Window)
			if err != nil {
				return nil, err
			}
			if c.Opens {
				next = nil
			}
			line, err := v.line(k.customer, c, next)
			if err != nil {
				return nil, err
			}
			b.view.Limits = append(b.view.Limits, line)
			b.lines[def.Key] = true
		}
	}
	v.builds[k] = b
	v.order = append(v.order, k)
	return b, nil
}

// line is where one claim's limit stands, as a view line.
func (v *viewing) line(customer uuid.UUID, c limits.Claim, next *limits.Window) (billing.ViewLimit, error) {
	s, resets, err := v.usage.Limit(v.ctx, customer, c)
	if err != nil {
		return billing.ViewLimit{}, err
	}
	reset := c.Reset
	out := billing.ViewLimit{Limit: c.Key, Unit: c.Unit, Reset: &reset, Overrun: cmp.Or(c.Overrun, catalog.OverrunRequireEstimate),
		Capacity: s.Capacity, Used: s.Settled, Held: s.Held, Remaining: s.Remaining(), ResetsAt: resets}
	// A window that ends is the gate's to switch at its end; one that never
	// ends, or a session not yet open, has none.
	if c.Window.EndsAt != nil && !c.Opens && c.Reset.Kind != catalog.ResetRolling {
		start, end := c.Window.StartsAt, *c.Window.EndsAt
		out.WindowStartsAt, out.WindowEndsAt = &start, &end
	}
	if next != nil {
		out.Next = &billing.ViewWindow{Remaining: c.Capacity}
		if !next.StartsAt.IsZero() {
			start := next.StartsAt
			out.Next.StartsAt, out.Next.EndsAt = &start, next.EndsAt
		}
	}
	return out, nil
}

// limitSet resolves what a request with the ViewParams' attributes claims,
// adding its spend windows to the view.
func (v *viewing) limitSet(b *viewBuild, vp billing.ViewParams, eventType string) (*billing.LimitSet, error) {
	r := v.run
	req := admission.AdmitRequest{CustomerID: identity.CustomerID(vp.CustomerID), Invoker: strings.TrimSpace(vp.Invoker),
		InvokerType: string(cmp.Or(vp.InvokerType, billing.InvokerTypeCustomer)), TrustLevel: strings.TrimSpace(deref(vp.TrustLevel)),
		Roles: vp.Roles, Resource: strings.TrimSpace(deref(vp.Resource)), Currency: b.key.currency}
	set, err := v.admitter.ResolveSet(v.ctx, v.q, req, eventType, b.cap, r.now)
	if err != nil {
		return nil, err
	}
	out := &billing.LimitSet{CustomerID: vp.CustomerID, Currency: catalog.Currency(b.key.currency), Limits: []string{catalog.BalanceLimit}, Capacities: []billing.LimitCapacity{}}
	if set.Refusal != nil {
		out.Refusal = &billing.Refusal{DenyCode: set.Refusal.DenyCode}
		if set.Refusal.RetryAfterSeconds > 0 {
			retry := set.Refusal.RetryAfterSeconds
			out.Refusal.RetryAfterSeconds = &retry
		}
		return out, nil
	}
	out.CreditLine, out.BalanceDenyCode = set.CreditLine, set.BalanceRefusal()
	for _, c := range set.Spend {
		if !b.lines[c.Key] {
			end := *c.Window.EndsAt
			next := limits.Window{StartsAt: end, EndsAt: new(end.Add(end.Sub(c.Window.StartsAt)))}
			line, err := v.line(b.key.customer, c, &next)
			if err != nil {
				return nil, err
			}
			b.view.Limits = append(b.view.Limits, line)
			b.lines[c.Key] = true
		}
		out.Limits = append(out.Limits, c.Key)
		out.Capacities = append(out.Capacities, billing.LimitCapacity{Limit: c.Key, Capacity: c.Capacity})
	}
	for _, c := range set.Limits {
		if b.lines[c.Key] {
			out.Limits = append(out.Limits, c.Key)
		}
	}
	return out, nil
}

// pricing is how a gate costs a request of eventType in currency: how each
// named-unit limit counts it, and the money cards in force with their price
// bounds.
func (v *viewing) pricing(currency, eventType string) (billing.ViewPricing, error) {
	pinned, err := v.money.CardsInForce(v.ctx, v.q, eventType, currency)
	if err != nil {
		return billing.ViewPricing{}, err
	}
	cards, bounds, err := money.ViewCards(pinned)
	if err != nil {
		return billing.ViewPricing{}, err
	}
	out := billing.ViewPricing{Currency: catalog.Currency(currency), EventType: eventType, Counts: []billing.ViewCount{}, RateCards: cards, PriceBounds: bounds}
	for _, def := range v.defs {
		if syntax.LimitUnitCurrency(def.Unit) || !limits.Counts(def, v.meters, eventType) {
			continue
		}
		for _, c := range def.Counts {
			m := v.meters[c.Meter]
			out.Counts = append(out.Counts, billing.ViewCount{Limit: def.Key, Meter: c.Meter, EventType: m.EventType, ValueProperty: m.ValueProperty,
				Aggregation: m.Aggregation, GroupBy: m.GroupBy, Filter: c.Filter, Weight: c.Weight, Per: c.Per})
		}
	}
	return out, nil
}
