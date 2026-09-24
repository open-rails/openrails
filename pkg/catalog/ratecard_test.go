package catalog

import (
	"strings"
	"testing"
)

const dropletCatalog = `
version: 1
meters:
  - key: droplet-runtime
    event_type: droplet.usage
    value_property: $.seconds
    aggregation: sum
    group_by: {size_slug: $.size_slug}
  - key: public-egress
    event_type: network.egress
    value_property: $.bytes
    aggregation: sum
  - {key: api-calls, aggregation: count}
products:
  - key: droplet
    display_name: Droplet
    rate_cards:
      - meter: droplet-runtime
        payment_term: IN_ARREARS
        price:
          model: per_unit
          currency: usd
          per_unit:
            divide_by: 3_600
            round: up
            matrix:
              dimension: size_slug
              cells:
                s-1vcpu-1gb: {unit_amount: 8_930, maximum_amount: 6_000_000}
      - meter: public-egress
        allowance: {accrue_from: droplet-runtime, cap: 28d}
        price:
          model: per_unit
          currency: usd
          per_unit: {unit_amount: 10_000, divide_by: 1_073_741_824, round: up}
  - key: volume
    display_name: Volume
    rate_cards:
      - meter: api-calls
        price: {model: per_unit, currency: usd, per_unit: {unit_amount: 2_000}}
  - key: api
    display_name: API
    tier_group: plans
    prices:
      - {currency: usd, unit_amount: 5_000_000, duration: 30d, auto_renew: true}
`

func TestRateCardsLoadAndRate(t *testing.T) {
	m := parse(t, dropletCatalog)
	droplet := product(t, m, "droplet")
	if droplet.RateCards[0].PaymentTerm != PaymentInArrears {
		t.Fatalf("payment_term not normalized: %q", droplet.RateCards[0].PaymentTerm)
	}
	runtime, egress := droplet.RateCards[0], droplet.RateCards[1]
	for _, tc := range []struct {
		name string
		card RateCard
		dim  string
		qty  int64
		want int64
	}{
		{"full month hits the monthly maximum", runtime, "s-1vcpu-1gb", 720 * 3_600, 6_000_000},
		{"30s bills its prorated cost with no floor", runtime, "s-1vcpu-1gb", 30, 75},
		{"zero usage is free", runtime, "s-1vcpu-1gb", 0, 0},
		{"base card ignores dimension: 5 GiB", egress, "ignored", 5 * 1_073_741_824, 50_000},
		{"partial GiB rounds up", egress, "", 1, 1},
	} {
		got, err := tc.card.RateUsage(tc.dim, tc.qty)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %d, %v; want %d", tc.name, got, err, tc.want)
		}
	}
	if _, err := runtime.RateUsage("s-99vcpu", 3_600); err == nil {
		t.Fatal("unknown matrix cell must error, not rate zero")
	}

	// Usage products need no tier_group or cadence and each gets its own
	// singleton group, so two of them never share tier exclusivity or ranks.
	groups := map[string]int{}
	for _, g := range m.TierGroups {
		groups[g.Key] = len(g.Products)
	}
	if groups["usage-droplet"] != 1 || groups["usage-volume"] != 1 || groups["plans"] != 1 {
		t.Fatalf("groups: %v", groups)
	}
}

func TestRateCardsRefuseInvalidModels(t *testing.T) {
	meters := "version: 1\nmeters:\n  - {key: m, event_type: e, value_property: $.v, aggregation: sum, group_by: {region: $.region}}\n  - {key: n, aggregation: count}\nproducts:\n"
	card := func(yaml string) string {
		return meters + "  - key: p\n    display_name: P\n    rate_cards:\n      - " + yaml + "\n"
	}
	perUnit := "price: {model: per_unit, currency: usd, per_unit: {unit_amount: 1}}"
	for _, tc := range []struct{ name, body, want string }{
		{"matrix dimension not grouped", card("meter: m\n        price: {model: per_unit, currency: usd, per_unit: {matrix: {dimension: size_slug, cells: {a: {unit_amount: 1}}}}}"), "not a group_by key"},
		{"accrue_from unknown", card("meter: m\n        allowance: {accrue_from: ghost}\n        " + perUnit), "accrue_from references unknown meter"},
		{"unknown meter", card("meter: ghost\n        " + perUnit), "references unknown meter"},
		{"usage card without meter", card(perUnit), "requires a meter"},
		{"flat card with meter", card("meter: m\n        price: {model: flat, currency: usd, flat: {amount: 1}}"), "must not reference a meter"},
		{"bad payment term", card("meter: m\n        payment_term: monthly\n        " + perUnit), "payment_term must be"},
		{"rate card with tier group", meters + "  - key: p\n    display_name: P\n    tier_group: g\n    rate_cards:\n      - meter: m\n        " + perUnit + "\n", "must not set tier_group"},
		{"meter shared by two cards", card("meter: m\n        "+perUnit) + "  - key: q\n    display_name: Q\n    rate_cards:\n      - meter: m\n        " + perUnit + "\n", "one meter per usage rate card"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}
