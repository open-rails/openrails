package limits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Definition is a declared limit as stored.
type Definition struct {
	Key       string
	Unit      string
	Reset     catalog.LimitReset
	Overrun   catalog.Overrun
	RateCards []catalog.LimitRateCard
	Revision  int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Row is a stored limit's columns.
type Row struct {
	Key           string
	Unit          string
	ResetKind     string
	WindowSeconds *int32
	ResetAnchor   *string
	Overrun       string
	RateCards     []byte
	Revision      int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Decode reads a stored limit.
func (r Row) Decode() (Definition, error) {
	d := Definition{Key: r.Key, Unit: r.Unit, Overrun: catalog.Overrun(r.Overrun), Revision: r.Revision, CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}
	d.Reset.Kind = catalog.ResetKind(r.ResetKind)
	if r.WindowSeconds != nil {
		d.Reset.WindowSeconds = int(*r.WindowSeconds)
	}
	if r.ResetAnchor != nil {
		d.Reset.Anchor = catalog.ResetAnchor(*r.ResetAnchor)
	}
	d.RateCards = []catalog.LimitRateCard{}
	if err := json.Unmarshal(r.RateCards, &d.RateCards); err != nil {
		return d, fmt.Errorf("limit %q rate cards: %w", r.Key, err)
	}
	return d, nil
}

// ErrNotFound is a limit the catalog does not declare.
var ErrNotFound = apperr.New(http.StatusNotFound, billing.CodeLimitNotFound, "the catalog declares no such limit")

func notFound(key string) error {
	return apperr.New(http.StatusNotFound, billing.CodeLimitNotFound, fmt.Sprintf("the catalog declares no limit %q", key))
}

// Definitions are the merchant's declared limits, by key.
func Definitions(ctx context.Context, q *gen.Queries, merchantID uuid.UUID) ([]Definition, error) {
	rows, err := q.ListCatalogLimits(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	out := make([]Definition, 0, len(rows))
	for _, r := range rows {
		d, err := Row(r).Decode()
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// Get is one declared limit, or ErrNotFound.
func Get(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, key string) (Definition, error) {
	row, err := q.GetCatalogLimit(ctx, gen.GetCatalogLimitParams{MerchantID: merchantID, LimitKey: key})
	if errors.Is(err, pgx.ErrNoRows) {
		return Definition{}, notFound(key)
	}
	if err != nil {
		return Definition{}, err
	}
	return Row(row).Decode()
}

// PeriodFunc is a customer's pricing period for a unit (a currency, or a
// named unit's merchant boundary) at an instant.
type PeriodFunc func(ctx context.Context, unit string, at time.Time) (start, end time.Time, err error)

// Customer reads one customer's limits.
type Customer struct {
	Q          *gen.Queries
	MerchantID uuid.UUID
	CustomerID uuid.UUID
	Period     PeriodFunc
	Now        time.Time
}

// capacities are the customer's base capacity per limit: an override, else
// the sum its live products give.
func (c Customer) capacities(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	products, err := c.Q.ListCustomerProductLimitCapacities(ctx, gen.ListCustomerProductLimitCapacitiesParams{MerchantID: c.MerchantID, CustomerID: c.CustomerID, At: c.Now})
	if err != nil {
		return nil, err
	}
	for _, p := range products {
		out[p.LimitKey] = p.Capacity
	}
	overrides, err := c.Q.ListCustomerLimitOverrides(ctx, gen.ListCustomerLimitOverridesParams{MerchantID: c.MerchantID, CustomerIds: []uuid.UUID{c.CustomerID}})
	if err != nil {
		return nil, err
	}
	for _, o := range overrides {
		out[o.LimitKey] = o.Capacity
	}
	return out, nil
}

// Granted are the limits some live product or override gives the customer
// capacity in: the ones it is subject to.
func (c Customer) Granted(ctx context.Context) (map[string]bool, error) {
	capacities, err := c.capacities(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(capacities))
	for key := range capacities {
		out[key] = true
	}
	return out, nil
}

// latest is each limit's newest window.
func (c Customer) latest(ctx context.Context) (map[string]Stored, error) {
	rows, err := c.Q.ListLatestLimitWindows(ctx, gen.ListLatestLimitWindowsParams{MerchantID: c.MerchantID, CustomerID: c.CustomerID})
	if err != nil {
		return nil, err
	}
	out := make(map[string]Stored, len(rows))
	for _, r := range rows {
		out[r.LimitKey] = Stored{StartsAt: r.WindowStartsAt.UTC(), EndsAt: r.WindowEndsAt, Settled: r.Settled, Adjusted: r.Adjusted, UpdatedAt: r.UpdatedAt}
	}
	return out, nil
}

// List is the customer's standing on every declared limit it is subject
// to, by key.
func (c Customer) List(ctx context.Context) ([]billing.CustomerLimit, error) {
	defs, err := Definitions(ctx, c.Q, c.MerchantID)
	if err != nil {
		return nil, err
	}
	return c.read(ctx, defs, true)
}

// One is the customer's standing on one limit, capacity 0 when no product
// or override gives it one.
func (c Customer) One(ctx context.Context, def Definition) (billing.CustomerLimit, error) {
	out, err := c.read(ctx, []Definition{def}, false)
	if err != nil {
		return billing.CustomerLimit{}, err
	}
	return out[0], nil
}

// read is the customer's lines for defs; subject keeps only the limits a
// product or override gives it capacity in.
func (c Customer) read(ctx context.Context, defs []Definition, subject bool) ([]billing.CustomerLimit, error) {
	c.Now = c.Now.UTC()
	capacities, err := c.capacities(ctx)
	if err != nil {
		return nil, err
	}
	latest, err := c.latest(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]billing.CustomerLimit, 0, len(defs))
	for _, def := range defs {
		capacity, granted := capacities[def.Key]
		if subject && !granted {
			continue
		}
		var newest *Stored
		if s, ok := latest[def.Key]; ok {
			newest = &s
		}
		line, err := c.standing(ctx, def, capacity, newest)
		if err != nil {
			return nil, err
		}
		out = append(out, line)
	}
	return out, nil
}

// window is where the limit's current window is.
func (c Customer) window(ctx context.Context, def Definition, newest *Stored) (Window, error) {
	anchors := Anchors{Period: func(at time.Time) (time.Time, time.Time, error) {
		if c.Period == nil {
			return time.Time{}, time.Time{}, fmt.Errorf("limit %q: no pricing period", def.Key)
		}
		unit := ""
		if catalog.LimitUnitCurrency(def.Unit) {
			unit = def.Unit
		}
		return c.Period(ctx, unit, at)
	}}
	if def.Reset.Kind == catalog.ResetFixed && def.Reset.Anchor == catalog.AnchorSubscription {
		started, err := c.Q.GetLimitSubscriptionAnchor(ctx, gen.GetLimitSubscriptionAnchorParams{LimitKey: def.Key, MerchantID: c.MerchantID, CustomerID: c.CustomerID})
		switch {
		case err == nil:
			anchors.Subscription = ptr(started.UTC())
		case !errors.Is(err, pgx.ErrNoRows):
			return Window{}, err
		}
	}
	return Locate(def.Reset, newest, anchors, c.Now)
}

// stored is the row of window w, if it has one.
func (c Customer) stored(ctx context.Context, key string, w Window, newest *Stored) (*Stored, error) {
	if newest != nil && newest.StartsAt.Equal(w.StartsAt) {
		return newest, nil
	}
	if newest == nil || newest.StartsAt.Before(w.StartsAt) {
		return nil, nil
	}
	r, err := c.Q.GetLimitWindow(ctx, gen.GetLimitWindowParams{MerchantID: c.MerchantID, CustomerID: c.CustomerID, LimitKey: key, WindowStartsAt: w.StartsAt})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Stored{StartsAt: r.WindowStartsAt.UTC(), EndsAt: r.WindowEndsAt, Settled: r.Settled, Adjusted: r.Adjusted, UpdatedAt: r.UpdatedAt}, nil
}

// standing is the customer's line for one limit: base capacity plus the
// window's adjustments, settled usage and held units.
func (c Customer) standing(ctx context.Context, def Definition, base int64, newest *Stored) (billing.CustomerLimit, error) {
	line := billing.CustomerLimit{CustomerID: billing.CustomerID(c.CustomerID), Limit: def.Key, Unit: def.Unit, Capacity: base}
	w, err := c.window(ctx, def, newest)
	if err != nil {
		return line, err
	}
	switch def.Reset.Kind {
	case catalog.ResetRolling:
		from := RollingFrom(def.Reset.WindowSeconds, c.Now)
		rows, err := c.Q.ListLimitWindowsFrom(ctx, gen.ListLimitWindowsFromParams{MerchantID: c.MerchantID, CustomerID: c.CustomerID, LimitKey: def.Key, WindowStartsAt: from})
		if err != nil {
			return line, err
		}
		line.WindowStartsAt = ptr(from)
		bucket := BucketWidth(def.Reset.WindowSeconds)
		for _, r := range rows {
			line.Capacity += r.Adjusted
			line.Used += r.Settled
			if r.Settled != 0 && line.ResetsAt == nil {
				line.ResetsAt = ptr(r.WindowStartsAt.UTC().Add(bucket + time.Duration(def.Reset.WindowSeconds)*time.Second))
			}
		}
	case catalog.ResetRate:
		row, err := c.stored(ctx, def.Key, w, newest)
		if err != nil {
			return line, err
		}
		if row != nil {
			line.Used = RateLevel(row.Settled, base, def.Reset.WindowSeconds, row.UpdatedAt, c.Now)
		}
		line.ResetsAt = RateRefilledAt(line.Used, base, def.Reset.WindowSeconds, c.Now)
	default:
		if !w.Open {
			break
		}
		row, err := c.stored(ctx, def.Key, w, newest)
		if err != nil {
			return line, err
		}
		if row != nil {
			line.Capacity += row.Adjusted
			line.Used = row.Settled
		}
		if def.Reset.Kind != catalog.ResetLifetime {
			line.WindowStartsAt, line.ResetsAt = ptr(w.StartsAt), w.EndsAt
		}
	}
	if w.Open || def.Reset.Kind == catalog.ResetRolling {
		from := w.StartsAt
		if def.Reset.Kind == catalog.ResetRolling {
			from = *line.WindowStartsAt
		}
		if line.Held, err = c.holder().Held(ctx, def.Key, from, w.StartsAt, w.EndsAt != nil); err != nil {
			return line, err
		}
	}
	line.Remaining = subSat(subSat(line.Capacity, line.Used), line.Held)
	return line, nil
}

func (c Customer) holder() Holder {
	return Holder{Q: c.Q, MerchantID: c.MerchantID, CustomerID: c.CustomerID, Now: c.Now}
}

// Applies reports whether def counts a request in currency of eventType
// ("" for a host-stated request): a limit in the currency counts every one,
// a named-unit limit those its rate cards read. Only a customer some product
// or override gives capacity in is subject to it.
func Applies(def Definition, meters map[string]Meter, currency, eventType string) bool {
	if catalog.LimitUnitCurrency(def.Unit) {
		return def.Unit == currency
	}
	return eventType != "" && Counts(def, meters, eventType)
}

// Claims are what a request makes of the declared limits the customer is
// subject to: every limit in its currency claims amount, and every limit
// whose rate cards read its usage claims the units the usage counts. A
// host-stated request with no usage claims no named-unit limit; its capture
// counts there.
func (c Customer) Claims(ctx context.Context, defs []Definition, meters map[string]Meter, currency string, amount int64, usage *Usage) ([]Claim, error) {
	eventType := ""
	if usage != nil {
		eventType = usage.EventType
	}
	claims, err := c.claims(ctx, defs, meters, currency, eventType)
	if err != nil {
		return nil, err
	}
	for i := range claims {
		claims[i].Cost = amount
		if claims[i].Unit != currency {
			def, _ := find(defs, claims[i].Key)
			if claims[i].Cost, err = Units(def, meters, *usage); err != nil {
				return nil, err
			}
		}
	}
	return claims, nil
}

// ClaimsFor are the claims a request of eventType in currency makes, with
// no cost: a hold renewed for its invokers counts what it counted.
func (c Customer) ClaimsFor(ctx context.Context, defs []Definition, meters map[string]Meter, currency, eventType string) ([]Claim, error) {
	return c.claims(ctx, defs, meters, currency, eventType)
}

func (c Customer) claims(ctx context.Context, defs []Definition, meters map[string]Meter, currency, eventType string) ([]Claim, error) {
	c.Now = c.Now.UTC()
	var applicable []Definition
	for _, def := range defs {
		if Applies(def, meters, currency, eventType) {
			applicable = append(applicable, def)
		}
	}
	if len(applicable) == 0 {
		return nil, nil
	}
	capacities, err := c.capacities(ctx)
	if err != nil {
		return nil, err
	}
	latest, err := c.latest(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Claim, 0, len(applicable))
	for _, def := range applicable {
		capacity, granted := capacities[def.Key]
		if !granted {
			continue
		}
		var newest *Stored
		if s, ok := latest[def.Key]; ok {
			newest = &s
		}
		w, err := c.window(ctx, def, newest)
		if err != nil {
			return nil, err
		}
		claim := Claim{Key: def.Key, Unit: def.Unit, Reset: def.Reset, Window: w, Capacity: capacity, Overrun: def.Overrun}
		if !w.Open {
			claim.Window, claim.Opens = Opening(def.Reset, c.Now), true
		}
		out = append(out, claim)
	}
	return out, nil
}

// Windows are the current window and base capacity of each declared limit
// the customer is subject to, by key: a session or first-use window not yet
// open is the one a use now would open (Opens).
func (c Customer) Windows(ctx context.Context, defs []Definition) (map[string]Claim, error) {
	c.Now = c.Now.UTC()
	capacities, err := c.capacities(ctx)
	if err != nil {
		return nil, err
	}
	latest, err := c.latest(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Claim, len(defs))
	for _, def := range defs {
		capacity, granted := capacities[def.Key]
		if !granted {
			continue
		}
		var newest *Stored
		if s, ok := latest[def.Key]; ok {
			newest = &s
		}
		w, err := c.window(ctx, def, newest)
		if err != nil {
			return nil, err
		}
		claim := Claim{Key: def.Key, Unit: def.Unit, Reset: def.Reset, Window: w, Capacity: capacity, Overrun: def.Overrun}
		if !w.Open {
			claim.Window, claim.Opens = Opening(def.Reset, c.Now), true
		}
		out[def.Key] = claim
	}
	return out, nil
}

func find(defs []Definition, key string) (Definition, bool) {
	for _, d := range defs {
		if d.Key == key {
			return d, true
		}
	}
	return Definition{}, false
}

// MaxAdjustment bounds one adjustment's units.
const MaxAdjustment = int64(1) << 53

// Adjust adds amount to the capacity of the customer's current window of
// def, opening a session or first-use window not yet open, and answers the
// customer's line after and the window it adjusted. The caller holds the
// customer's spend lock, which serializes window openings.
func (c Customer) Adjust(ctx context.Context, def Definition, amount int64) (billing.CustomerLimit, time.Time, error) {
	c.Now = c.Now.UTC()
	if amount == 0 || amount > MaxAdjustment || amount < -MaxAdjustment {
		return billing.CustomerLimit{}, time.Time{}, apperr.Invalidf("amount must be nonzero and at most %d either way", MaxAdjustment).WithParam("amount")
	}
	if def.Reset.Kind == catalog.ResetRate {
		return billing.CustomerLimit{}, time.Time{}, apperr.Invalidf("limit %q refills continuously and takes no adjustment; set an override instead", def.Key).WithParam("limit")
	}
	capacities, err := c.capacities(ctx)
	if err != nil {
		return billing.CustomerLimit{}, time.Time{}, err
	}
	latest, err := c.latest(ctx)
	if err != nil {
		return billing.CustomerLimit{}, time.Time{}, err
	}
	var newest *Stored
	if s, ok := latest[def.Key]; ok {
		newest = &s
	}
	w, err := c.window(ctx, def, newest)
	if err != nil {
		return billing.CustomerLimit{}, time.Time{}, err
	}
	if !w.Open {
		w = Opening(def.Reset, c.Now)
	}
	if err := c.Q.AdjustLimitWindow(ctx, gen.AdjustLimitWindowParams{
		MerchantID: c.MerchantID, CustomerID: c.CustomerID, LimitKey: def.Key,
		WindowStartsAt: w.StartsAt, WindowEndsAt: w.EndsAt, Capacity: capacities[def.Key], Amount: amount,
	}); err != nil {
		return billing.CustomerLimit{}, time.Time{}, err
	}
	line, err := c.One(ctx, def)
	return line, w.StartsAt, err
}

// SetOverrides writes a customer's overrides: each replaces what its
// products give the limit, and Inherit clears it. Every limit is declared.
func SetOverrides(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, overrides []billing.LimitOverride) error {
	for _, o := range overrides {
		if o.Inherit {
			if err := q.DeleteCustomerLimitOverride(ctx, gen.DeleteCustomerLimitOverrideParams{MerchantID: merchantID, CustomerID: customerID, LimitKey: o.Limit}); err != nil {
				return err
			}
			continue
		}
		if err := q.UpsertCustomerLimitOverride(ctx, gen.UpsertCustomerLimitOverrideParams{MerchantID: merchantID, CustomerID: customerID, LimitKey: o.Limit, Capacity: o.Capacity}); err != nil {
			return err
		}
	}
	return nil
}

// Overrides are the customers' overrides, by limit.
func Overrides(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, customers []uuid.UUID) (map[uuid.UUID][]billing.LimitOverride, error) {
	rows, err := q.ListCustomerLimitOverrides(ctx, gen.ListCustomerLimitOverridesParams{MerchantID: merchantID, CustomerIds: customers})
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID][]billing.LimitOverride{}
	for _, r := range rows {
		out[r.CustomerID] = append(out[r.CustomerID], billing.LimitOverride{Limit: r.LimitKey, Capacity: r.Capacity})
	}
	return out, nil
}
