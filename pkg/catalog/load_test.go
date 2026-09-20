package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

const goodManifest = `
version: 1

products:
  - key: initiate
    display_name: Novice
    tier_group: membership
    tier_rank: 1
    entitlements: [tier:initiate]
    prices:
      - currency: usd
        unit_amount: 1200
        duration: 30d
        auto_renew: true
        psps: [stripe]
  - key: craftsman
    display_name: Craftsman
    tier_group: membership
    tier_rank: 2
    entitlements: [tier:craftsman]
    prices:
      - currency: usd
        unit_amount: 2900
        duration: 30d
        auto_renew: true
        psps: [stripe]
      - currency: usd
        unit_amount: 1500
        duration: 30d
        auto_renew: true
        archived: true
        psps: [stripe]
        psp_links:
          stripe:
            price_id: price_legacy123
`

func TestLoad_Good(t *testing.T) {
	m, err := Load(writeManifest(t, goodManifest))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Version != 1 {
		t.Fatalf("unexpected header: %+v", m)
	}
	if len(m.TierGroups) != 1 || len(m.TierGroups[0].Products) != 2 {
		t.Fatalf("unexpected structure: %+v", m.TierGroups)
	}
	craftsman := m.TierGroups[0].Products[1]
	if len(craftsman.Prices) != 2 {
		t.Fatalf("want 2 prices, got %d", len(craftsman.Prices))
	}
	// Duration defaults and normalization.
	if craftsman.Prices[0].Duration != "30d" {
		t.Fatalf("duration not normalized: %+v", craftsman.Prices[0])
	}
	// Historical prices are declared directly as archived.
	legacy := craftsman.Prices[1]
	if !legacy.Archived {
		t.Fatalf("historical price should be archived, got %v", legacy.Archived)
	}
	if got := legacy.PSPLinks["stripe"]["price_id"]; got != "price_legacy123" {
		t.Fatalf("psp_links.stripe.price_id not preserved: %q", got)
	}
	if got := craftsman.Prices[0].PSPs; len(got) != 1 || got[0] != "stripe" {
		t.Fatalf("psps not normalized: %v", got)
	}
}

func TestValidateRejectsTierGroupsOnly(t *testing.T) {
	m := &Manifest{
		Version:    SupportedVersion,
		TierGroups: []TierGroup{{Key: "old", Products: []Product{{Key: "p", DisplayName: "P"}}}},
	}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "not tier_groups") {
		t.Fatalf("want tier_groups-only rejection, got %v", err)
	}
}

func TestValidateProductsIsIdempotent(t *testing.T) {
	m, err := Load(writeManifest(t, goodManifest))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("second Validate: %v", err)
	}
}

func ranksByKey(m *Manifest) map[string]int {
	ranks := map[string]int{}
	for _, group := range m.TierGroups {
		for _, product := range group.Products {
			ranks[product.Key] = product.tierRank()
		}
	}
	return ranks
}

func TestLoad_RateCardCarriesTimeDenominatorInDivideBy(t *testing.T) {
	body := `
version: 1
meters:
  - {key: vm-seconds, aggregation: sum, value_property: $.seconds}
products:
  - key: vm
    display_name: VM
    rate_cards:
      - meter: vm-seconds
        payment_term: in_arrears
        price:
          model: per_unit
          currency: usd
          per_unit: {unit_amount: 500_000, divide_by: 3600}
`
	m, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := m.TierGroups[0].Products[0]
	if len(p.RateCards) != 1 {
		t.Fatalf("expected one rate card, got %+v", p.RateCards)
	}
	pu := p.RateCards[0].Price.PerUnit
	if pu == nil || pu.UnitAmount != 500_000 || pu.DivideBy != 3600 {
		t.Fatalf("per-unit denominator wrong: %+v", pu)
	}
}

// A subscription base fee and a usage rate card coexist on one product: the
// price row is the recurring term, the rate card is the arrears usage.
func TestLoad_BaseFeePriceRowCoexistsWithRateCard(t *testing.T) {
	body := `
version: 1
meters:
  - {key: api-calls, aggregation: sum, value_property: $.count}
products:
  - key: api
    display_name: API
    prices:
      - currency: usd
        unit_amount: 5_000_000
        duration: 30d
        auto_renew: true
        psps: []
    rate_cards:
      - meter: api-calls
        payment_term: in_arrears
        price:
          model: per_unit
          currency: usd
          per_unit: {unit_amount: 2_000}
`
	m, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := m.TierGroups[0].Products[0]
	if len(p.Prices) != 1 || p.Prices[0].UnitAmount != 5_000_000 {
		t.Fatalf("base-fee price row not preserved: %+v", p.Prices)
	}
	if len(p.RateCards) != 1 || p.RateCards[0].Meter != "api-calls" {
		t.Fatalf("expected the usage rate card: %+v", p.RateCards)
	}
}

func TestLoadRejectsDeferredCatalogFeatures(t *testing.T) {
	for _, field := range []string{"credit_balances", "usage_limits"} {
		_, err := Parse([]byte("version: 1\n" + field + ": []\nproducts: []\n"))
		if err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("accepted %s: %v", field, err)
		}
	}
	for _, field := range []string{"credits", "includes", "usage_limits"} {
		_, err := Parse([]byte("version: 1\nproducts:\n  - key: test\n    display_name: Test\n    " + field + ": []\n"))
		if err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("accepted %s: %v", field, err)
		}
	}
}

func TestLoadRefusesInvalidInputs(t *testing.T) {
	cases := []struct {
		name, body string
		errors     []string
	}{
		{"BadVersion", `version: 2
products: []`, []string{"unsupported"}},
		{"RejectsTierGroups", `version: 1
tier_groups: []`, []string{"unknown field"}},
		{"RejectsStatus", `version: 1
products:
  - key: p
    display_name: P
    status: archived`, []string{"unknown field"}},
		{"RejectsDefaultProviders", `version: 1
default_providers: [stripe]
products: []`, []string{"unknown field"}},
		{"RejectsProductProviders", `version: 1
products:
  - key: p
    display_name: P
    psps: [stripe]`, []string{"unknown field"}},
		{"ProviderLinksRequirePriceProvider", `version: 1
products:
  - key: p
    display_name: P
    prices:
      - currency: usd
        unit_amount: 1000
        duration: 30d
        psp_links:
          stripe:
            lookup_key: p-monthly`, []string{"requires psps to include"}},
		{"RejectsRetiredProviderKeysForTypedPrice", `version: 1
products:
  - key: topup
    display_name: Topup
    prices:
      - currency: usd
        unit_amount: 10_000
        providers: [stripe]`, []string{"unknown field \"providers\"", "providers: was renamed to psps:"}},
		{"RejectsRetiredProviderLinksKey", `version: 1
products:
  - key: p
    display_name: P
    prices:
      - currency: usd
        unit_amount: 1000
        duration: 30d
        psps: [stripe]
        provider_links:
          stripe:
            lookup_key: p-monthly`, []string{"unknown field \"provider_links\"", "provider_links: was renamed to psp_links:"}},
		{"DuplicatePriceByTerms", `version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: usd, unit_amount: 1000, duration: 30d, auto_renew: true}
      - {currency: usd, unit_amount: 1000, duration: 30d, auto_renew: true}`, []string{"duplicate price terms"}},
		{"RejectsSameTermsWithDifferentProviders", `version: 1
products:
  - key: p
    display_name: P
    prices:
      - {currency: usd, unit_amount: 23000000, duration: 30d, psps: [mobius, ccbill, solana]}
      - {currency: usd, unit_amount: 23000000, duration: 30d, psps: [solana], archived: true}`, []string{"duplicate price terms"}},
		{"DuplicateProductKey", `version: 1
products:
  - {key: p, display_name: P, tier_group: g1, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}
  - {key: p, display_name: P, tier_group: g2, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}`, []string{"duplicate product key"}},
		{"MissingTierRank", `version: 1
products:
  - {key: p1, display_name: P1, tier_group: g, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}
  - {key: p2, display_name: P2, tier_group: g, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}`, []string{"tier_rank is required"}},
		{"SolanaNonStablecoinRejected", `version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: eur, unit_amount: 1000, duration: 30d, psps: [solana]}`, []string{"solana requires a stablecoin"}},
		// One meter backs at most one usage rate card, whichever product declares it.
		{"RateCardsMayNotShareAMeter", `version: 1
meters:
  - {key: droplet-vcpu-seconds, aggregation: sum, value_property: $.seconds}
products:
  - key: basic-droplet
    display_name: Basic Droplet
    rate_cards:
      - meter: droplet-vcpu-seconds
        payment_term: in_arrears
        price: {model: per_unit, currency: usd, per_unit: {unit_amount: 7_000, divide_by: 3_600}}
  - key: premium-droplet
    display_name: Premium Droplet
    rate_cards:
      - meter: droplet-vcpu-seconds
        payment_term: in_arrears
        price: {model: per_unit, currency: usd, per_unit: {unit_amount: 14_000, divide_by: 3_600}}`, []string{"one meter per usage rate card"}},
		{"BadInterval", `version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: usd, unit_amount: 1000, duration: 1w}`, []string{"must use h or d"}},
		{"RejectsLegacyNamedInterval", `version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: usd, unit_amount: 1000, duration: month}`, []string{"whole h or d value"}},
		{"retired metered price", `version: 1
meters:
  - {key: api-calls, aggregation: sum, value_property: $.count}
products:
  - key: api
    display_name: API
    prices:
      - currency: usd
        unit_amount: 0
        psps: []
        metered: {meter: api-calls, rate: 2_000}`, []string{"unknown field \"metered\"", "the metered: price sugar was removed (or#893/#707)", "rate_cards:", "payment_term: in_arrears", "model: per_unit"}},
		{"retired meter kind", `version: 1
meters:
  - {key: api-calls, kind: counter}
products:
  - key: api
    display_name: API
    prices:
      - {currency: usd, unit_amount: 1_000, duration: 30d}`, []string{"unknown field \"kind\"", "meter kind: counter|gauge was removed (or#893/#707)", "aggregation: sum", "aggregation: count"}},
		{"missing aggregation", `version: 1
meters:
  - {key: api-calls}
products:
  - key: api
    display_name: API
    prices:
      - {currency: usd, unit_amount: 1_000, duration: 30d}`, []string{"aggregation is required"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeManifest(t, tc.body))
			if err == nil {
				t.Fatal("invalid catalog was accepted")
			}
			for _, want := range tc.errors {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error must contain %q, got %v", want, err)
				}
			}
		})
	}
}

func TestLoadTierRanks(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ranks      map[string]int
	}{
		{"single product rank omitted", `version: 1
products:
  - {key: p, display_name: P, tier_group: g, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}`, nil},
		{"negative tier prepended", `version: 1
products:
  - {key: free, display_name: Free, tier_group: g, tier_rank: -1, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}
  - {key: starter, display_name: Starter, tier_group: g, tier_rank: 0, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}
  - {key: pro, display_name: Pro, tier_group: g, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}`, map[string]int{"free": -1, "starter": 0, "pro": 1}},
		{"ranks renumbered", `version: 1
products:
  - {key: starter, display_name: Starter, tier_group: g, tier_rank: 10, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}
  - {key: pro, display_name: Pro, tier_group: g, tier_rank: 20, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}`, map[string]int{"starter": 10, "pro": 20}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Load(writeManifest(t, tc.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if tc.ranks == nil {
				if m.TierGroups[0].Products[0].TierRank != nil {
					t.Fatal("single-product rank must remain omitted")
				}
				return
			}
			ranks := ranksByKey(m)
			for key, want := range tc.ranks {
				if got, ok := ranks[key]; !ok || got != want {
					t.Errorf("rank[%q]=%v, want %v", key, got, want)
				}
			}
			if ranks["starter"] >= ranks["pro"] {
				t.Fatalf("upgrade/downgrade direction must survive renumbering or prepending: %v", ranks)
			}
		})
	}
}

func TestLoadAcceptsPriceTerms(t *testing.T) {
	for _, tc := range []struct {
		name, body, duration string
		prices               int
	}{
		{"AcceptsSameTermsWithDifferentTrials", `version: 1
products:
  - key: p
    display_name: P
    prices:
      - {currency: usd, unit_amount: 23000000, duration: 30d, auto_renew: true, psps: [mobius]}
      - {currency: usd, unit_amount: 23000000, duration: 30d, auto_renew: true, psps: [mobius], trial: {unit_amount: 100, duration: 7d}}`, "30d", 2},
		{"SolanaStablecoinAccepted", `version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: usdc, unit_amount: 1000, duration: 30d, auto_renew: true, psps: [solana]}`, "30d", 1},
		{"NormalizesWholeDayHourInterval", `version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: usd, unit_amount: 1000, duration: 24h, auto_renew: true}`, "1d", 1},
		{"AcceptsSubDayRecurringInterval", `version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: usd, unit_amount: 1000, duration: 1h, auto_renew: true}`, "1h", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Load(writeManifest(t, tc.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			prices := m.TierGroups[0].Products[0].Prices
			if len(prices) != tc.prices {
				t.Fatalf("price count=%d, want %d", len(prices), tc.prices)
			}
			for _, price := range prices {
				if price.Duration != tc.duration {
					t.Errorf("duration=%q, want %q", price.Duration, tc.duration)
				}
			}
		})
	}
}
