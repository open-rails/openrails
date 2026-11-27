package metrics

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/calendarutil"
)

// Usage measures read the event store's usage history by day of the
// merchant's accounting zone, at the prices intake took (less a volume
// tier's discount), so the days and the months they fold into sum to what
// posts. History names no invoker: a plan grouped or filtered by invoker
// reads raw usage events, kept eventstore.EventsKept (ValidateIn refuses a
// range that starts before them).
// They compile to no SQL; their groups are folded here.

func storeFamily(f Family) bool { return f == FamUsage || f == FamUsageCharged }

// usageCharges are the charges a priced history row counts in.
var usageCharges = []string{"charged", "forgiven"}

// usagePart is one outcome's share of a usage row: its events (event rows
// only), what it charged and what grace forgave.
type usagePart struct {
	outcome                  string
	units, charged, forgiven int64
}

// usageParts splits a row by outcome: failed usage is counted beside the
// rest, never a row of its own.
func usageParts(r eventstore.UsageBucket) [2]usagePart {
	ok := usagePart{outcome: "succeeded", charged: r.Amount - r.Discount}
	if r.Meter == "" {
		ok.units = r.Requests - r.FailedRequests
	}
	return [2]usagePart{ok, {outcome: "failed", units: r.FailedRequests, charged: r.FailedAmount, forgiven: r.Forgiven}}
}

// byInvoker reports a plan that groups or filters usage by invoker.
func (p *Plan) byInvoker() bool {
	_, filtered := p.Filters["invoker"]
	return filtered || slices.Contains(p.Dims, "invoker")
}

// runUsage folds the plan's usage measures into groups.
func (s *Service) runUsage(ctx context.Context, plan *Plan, groups map[string]*group) error {
	var leaves []*Measure
	seen := map[string]bool{}
	for _, m := range plan.Measures {
		for _, leaf := range m.components() {
			if storeFamily(leaf.Family) && !seen[leaf.Name] {
				seen[leaf.Name] = true
				leaves = append(leaves, leaf)
			}
		}
	}
	if len(leaves) == 0 {
		return nil
	}
	svc := money.NewMoneyService(s.db)
	var rows []eventstore.UsageBucket
	var err error
	if plan.byInvoker() {
		rows, err = svc.UsageEvents(ctx, eventstore.HistoryFilter{From: plan.From, To: plan.To, Zone: plan.zone()})
	} else {
		rows, err = svc.UsageHistory(ctx, eventstore.HistoryFilter{From: plan.From, To: plan.To})
	}
	if err != nil {
		return err
	}
	// Usage a merchant's unit forgave at posting moves from charged to
	// forgiven, by rate card with no invoker: a posting knows none.
	if seen["usage_charged"] {
		forgiven, err := svc.UsageForgiveness(ctx, plan.From, plan.To)
		if err != nil {
			return err
		}
		for _, r := range forgiven {
			var bucket time.Time
			if plan.HasTime {
				bucket = calendarutil.BucketStart(r.BucketAt, plan.Grain, plan.zone())
			}
			values := map[string]string{"currency": r.Currency, "customer": r.CustomerID.String(), "invoker": r.Invoker,
				"outcome": "succeeded", "rate_card": r.EventType}
			for _, charge := range usageCharges {
				values["charge"] = charge
				dims, ok := usageDimsOf(plan, values)
				if !ok {
					continue
				}
				g := ensureGroup(groups, bucket, dims)
				v := g.vals["usage_charged"]
				if charge == "forgiven" {
					v.n += r.Forgiven
				} else {
					v.n += r.Amount
				}
				g.vals["usage_charged"] = v
			}
		}
	}
	distinct := map[string]map[uuid.UUID]bool{}
	for _, r := range rows {
		var bucket time.Time
		if plan.HasTime {
			bucket = calendarutil.BucketStart(r.BucketAt, plan.Grain, plan.zone())
		}
		for _, part := range usageParts(r) {
			values := map[string]string{"currency": r.Currency, "customer": r.CustomerID.String(), "invoker": r.Invoker,
				"outcome": part.outcome, "rate_card": r.EventType}
			for _, charge := range usageCharges {
				values["charge"] = charge
				amount := part.charged
				if charge == "forgiven" {
					amount = part.forgiven
				}
				for _, m := range leaves {
					if m.Family == FamUsage && charge != usageCharges[0] {
						continue
					}
					dims, ok := usageDimsOf(plan, values)
					if !ok {
						continue
					}
					g := ensureGroup(groups, bucket, dims)
					v := g.vals[m.Name]
					switch m.Name {
					case "usage_charged":
						v.n += amount
					case "usage_units":
						v.n += part.units
					case "active_customers":
						k := groupKey(bucket, dims)
						if distinct[k] == nil {
							distinct[k] = map[uuid.UUID]bool{}
						}
						if !distinct[k][r.CustomerID.UUID()] {
							distinct[k][r.CustomerID.UUID()] = true
							v.n++
						}
					}
					g.vals[m.Name] = v
				}
			}
		}
	}
	return nil
}

// usageDimsOf is a history row's dimension values in the plan's order, and
// whether its filters admit it.
func usageDimsOf(plan *Plan, values map[string]string) ([]string, bool) {
	for dim, allowed := range plan.Filters {
		v, known := values[dim]
		if !known {
			continue
		}
		ok := false
		for _, a := range allowed {
			if a == v {
				ok = true
				break
			}
		}
		if !ok {
			return nil, false
		}
	}
	dims := make([]string, len(plan.Dims))
	for i, d := range plan.Dims {
		dims[i] = values[d]
	}
	return dims, true
}

// burn is what a customer's usage in a currency cost in the 7 days before a
// depletion edge.
type burn struct {
	edge     time.Time
	customer uuid.UUID
	currency string
	amount   int64
}

// depletionBurns are the burns the plan's depletion measure reads; none
// without one, or without an event store.
func (s *Service) depletionBurns(ctx context.Context, plan *Plan) ([]burn, error) {
	var leaves []*Measure
	for _, m := range plan.Measures {
		for _, leaf := range m.components() {
			if leaf.Family == FamDepletion {
				leaves = append(leaves, leaf)
			}
		}
	}
	if len(leaves) == 0 || s.db.EventStore() == nil {
		return nil, nil
	}
	edges := snapshotEdges(plan, leaves)
	if len(edges) == 0 {
		return nil, nil
	}
	// The window is the edge's last seven days of the merchant's zone, which
	// history is read by.
	back := func(e time.Time) time.Time {
		l := e.In(plan.zone())
		return calendarutil.DayStart(l.Year(), l.Month(), l.Day()-depletionBurnDays, plan.zone()).UTC()
	}
	from, to := edges[0], edges[0]
	for _, e := range edges {
		from, to = earliest(from, e), latest(to, e)
	}
	rows, err := money.NewMoneyService(s.db).UsageHistory(ctx, eventstore.HistoryFilter{From: back(from), To: to})
	if err != nil {
		return nil, err
	}
	type key struct {
		edge     time.Time
		customer uuid.UUID
		currency string
	}
	sums := map[key]int64{}
	var order []key
	for _, e := range edges {
		for _, r := range rows {
			if r.BucketAt.Before(back(e)) || !r.BucketAt.Before(e) {
				continue
			}
			k := key{e, r.CustomerID.UUID(), r.Currency}
			if _, ok := sums[k]; !ok {
				order = append(order, k)
			}
			sums[k] += r.Cost()
		}
	}
	out := make([]burn, 0, len(order))
	for _, k := range order {
		if sums[k] > 0 {
			out = append(out, burn{edge: k.edge, customer: k.customer, currency: k.currency, amount: sums[k]})
		}
	}
	return out, nil
}

// depletionBurnDays is the trailing burn's window.
const depletionBurnDays = 7

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
