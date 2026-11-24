package metrics

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/modules/money"
)

// Usage measures read the event store's usage history, priced as a
// customer's usage reports price it: each 15-minute bucket's share of its
// period's price, so the days and months it folds into sum to what posts.
// They compile to no SQL; their groups are folded here.

func storeFamily(f Family) bool { return f == FamUsage || f == FamUsageCharged }

// usageCharges are the charges a priced history row counts in.
var usageCharges = []string{"charged", "forgiven"}

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
	rows, err := money.NewMoneyService(s.db).UsageHistory(ctx, eventstore.HistoryFilter{From: plan.From, To: plan.To})
	if err != nil {
		return err
	}
	distinct := map[string]map[uuid.UUID]bool{}
	for _, r := range rows {
		var bucket time.Time
		if plan.HasTime {
			bucket = truncGrain(r.BucketAt, plan.Grain)
		}
		values := map[string]string{"currency": r.Currency, "customer": r.CustomerID.String(), "invoker": r.Invoker,
			"outcome": string(r.Outcome), "rate_card": r.EventType}
		for _, charge := range usageCharges {
			values["charge"] = charge
			amount := r.Amount
			if charge == "forgiven" {
				amount = r.Forgiven
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
					if r.Meter == "" {
						v.n += r.Requests
					}
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
	from, to := edges[0], edges[0]
	for _, e := range edges {
		from, to = earliest(from, e), latest(to, e)
	}
	rows, err := money.NewMoneyService(s.db).UsageHistory(ctx, eventstore.HistoryFilter{From: from.AddDate(0, 0, -depletionBurnDays), To: to})
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
			if r.BucketAt.Before(e.AddDate(0, 0, -depletionBurnDays)) || !r.BucketAt.Before(e) {
				continue
			}
			k := key{e, r.CustomerID.UUID(), r.Currency}
			if _, ok := sums[k]; !ok {
				order = append(order, k)
			}
			sums[k] += r.Amount
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
