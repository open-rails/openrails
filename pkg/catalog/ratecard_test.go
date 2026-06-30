package catalog

import (
	"strings"
	"testing"
)

func productByKey(t *testing.T, m *Manifest, key string) Product {
	t.Helper()
	for _, g := range m.TierGroups {
		for _, p := range g.Products {
			if p.Key == key {
				return p
			}
		}
	}
	t.Fatalf("product %q not found", key)
	return Product{}
}

const doRateCardManifest = `
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
products:
  - key: droplet
    display_name: Droplet
    rate_cards:
      - meter: droplet-runtime
        price:
          model: per_unit
          currency: usd
          divide_by: 3_600
          round: up
          matrix:
            dimension: size_slug
            cells:
              s-1vcpu-1gb: {unit_amount: 8_930, maximum_amount: 6_000_000}
      - meter: public-egress
        allowance: {accrue_from: droplet-runtime, pool: customer, cap: 28d}
        price: {model: per_unit, currency: usd, unit_amount: 10_000, divide_by: 1_073_741_824, round: up}
`

func TestRateCard_DOStyleLoadsAndRates(t *testing.T) {
	m, err := Load(writeManifest(t, doRateCardManifest))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	droplet := productByKey(t, m, "droplet")
	cm, ok := droplet.RateCards[0].Price.ChargeModelForCell("s-1vcpu-1gb")
	if !ok {
		t.Fatal("ChargeModelForCell s-1vcpu-1gb missing")
	}
	// Full month (720h > 672h cap) bills exactly the $6/mo maximum.
	if got, _ := cm.Rate(720 * 3_600); got != 6_000_000 {
		t.Fatalf("capped month = %d, want 6000000", got)
	}
	// 30 seconds bills its true pro-rated cost (no floor): 30 * 8930/3600 -> 75.
	if got, _ := cm.Rate(30); got != 75 {
		t.Fatalf("sub-cent charge = %d, want 75 (no floor)", got)
	}
}

func TestRateUsage_MatrixAndBaseDispatch(t *testing.T) {
	m, err := Load(writeManifest(t, doRateCardManifest))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	droplet := productByKey(t, m, "droplet")
	// matrix card dispatches on the size_slug dimension value.
	if got, err := droplet.RateCards[0].RateUsage("s-1vcpu-1gb", 720*3_600); err != nil || got != 6_000_000 {
		t.Fatalf("matrix RateUsage = %d, %v; want 6000000", got, err)
	}
	// unknown dimension value is an error, not a silent zero.
	if _, err := droplet.RateCards[0].RateUsage("s-99vcpu", 3_600); err == nil {
		t.Fatal("want error for unknown matrix cell")
	}
	// base (non-matrix) per_unit card: 5 GiB egress -> 5 * $0.01 = 50_000.
	if got, err := droplet.RateCards[1].RateUsage("", 5*1_073_741_824); err != nil || got != 50_000 {
		t.Fatalf("base RateUsage = %d, %v; want 50000", got, err)
	}
}

// A usage-metered product loads with no tier_group and no billing cadence — it
// isn't a tier subscription, and its cap/allowance window is the invoice period
// (#642). The loader places it in its own synthetic singleton group.
func TestRateCard_UsageProductNeedsNoTierGroupOrCadence(t *testing.T) {
	body := `
version: 1
meters:
  - {key: m, event_type: e, value_property: $.v, aggregation: sum}
products:
  - key: p
    display_name: P
    rate_cards:
      - meter: m
        price: {model: per_unit, currency: usd, unit_amount: 1}
`
	m, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatalf("usage product without tier_group/billing_cadence should load: %v", err)
	}
	if len(m.TierGroups) != 1 || len(m.TierGroups[0].Products) != 1 {
		t.Fatalf("want one singleton group, got %+v", m.TierGroups)
	}
	if got := m.TierGroups[0].Key; got != "usage-p" {
		t.Fatalf("synthetic group key = %q, want usage-p", got)
	}
}

func TestRateCard_MatrixDimensionMustBeGroupByKey(t *testing.T) {
	body := `
version: 1
meters:
  - {key: m, event_type: e, value_property: $.v, aggregation: sum, group_by: {region: $.region}}
products:
  - key: p
    display_name: P
    rate_cards:
      - meter: m
        price:
          model: per_unit
          currency: usd
          matrix:
            dimension: size_slug
            cells: {a: {unit_amount: 1}}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "not a group_by key") {
		t.Fatalf("want matrix dimension error, got %v", err)
	}
}

func TestRateCard_OneMeterPerUsageCard(t *testing.T) {
	body := `
version: 1
meters:
  - {key: m, event_type: e, value_property: $.v, aggregation: sum}
products:
  - key: p
    display_name: P
    rate_cards:
      - meter: m
        price: {model: per_unit, currency: usd, unit_amount: 1}
      - meter: m
        price: {model: per_unit, currency: usd, unit_amount: 2}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "already rated") {
		t.Fatalf("want duplicate-meter error, got %v", err)
	}
}

func TestRateCard_AllowanceAccrueFromMustExist(t *testing.T) {
	body := `
version: 1
meters:
  - {key: egress, event_type: e, value_property: $.v, aggregation: sum}
products:
  - key: p
    display_name: P
    rate_cards:
      - meter: egress
        allowance: {accrue_from: ghost-meter, pool: customer}
        price: {model: per_unit, currency: usd, unit_amount: 1}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "accrue_from references unknown meter") {
		t.Fatalf("want accrue_from error, got %v", err)
	}
}

func TestMeter_NewStyleRejectsBadAggregation(t *testing.T) {
	body := `
version: 1
meters:
  - {key: m, event_type: e, value_property: $.v, aggregation: median}
products:
  - {key: p, display_name: P, prices: [{currency: usd, unit_amount: 1, duration: 30d}]}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "aggregation must be one of") {
		t.Fatalf("want aggregation error, got %v", err)
	}
}

const creditTopupManifest = `
version: 1
products:
  - key: image-credit-topup
    display_name: AI Image Credits
    tier_group: topups
    credit_purchase:
      credit_type: ai-image-gen
      unit: ai-image-credit
      currency: usd
      expires: 30d
      input_min: 5_000_000
      round: down
      price:
        model: tiered
        mode: graduated
        tiers:
          - {up_to: 2_000, unit_amount: 10_000}
          - {up_to: 10_000, unit_amount: 9_000}
          - {unit_amount: 8_000}
`

func TestCreditPurchase_GraduatedLoadsAndQuotes(t *testing.T) {
	m, err := Load(writeManifest(t, creditTopupManifest))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cp := productByKey(t, m, "image-credit-topup").CreditPurchase
	if cp == nil {
		t.Fatal("credit_purchase not parsed")
	}
	credits, spent, err := QuoteUnitsForSpend(20_000_000, cp.Price.ToChargeModel())
	if err != nil {
		t.Fatal(err)
	}
	if credits != 2_000 || spent != 20_000_000 {
		t.Fatalf("$20 -> (%d, %d), want (2000, 20000000)", credits, spent)
	}
}

func TestCreditPurchase_RejectsVolumeMode(t *testing.T) {
	body := `
version: 1
products:
  - key: topup
    display_name: Topup
    credit_purchase:
      credit_type: ai-image-gen
      currency: usd
      price:
        model: tiered
        mode: volume
        tiers:
          - {up_to: 2_000, unit_amount: 10_000}
          - {unit_amount: 9_000}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "graduated mode") {
		t.Fatalf("want volume-mode rejection, got %v", err)
	}
}

func TestCreditPurchase_RejectsFlatModel(t *testing.T) {
	body := `
version: 1
products:
  - key: topup
    display_name: Topup
    credit_purchase:
      credit_type: ai-image-gen
      currency: usd
      price: {model: flat, amount: 5_000_000}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "per_unit or graduated tiered") {
		t.Fatalf("want flat-model rejection, got %v", err)
	}
}
