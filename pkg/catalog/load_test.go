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
default_providers: [stripe]

tier_groups:
  - slug: cozy
    display_name: Cozy Art Plans
    products:
      - slug: initiate
        display_name: Novice
        tier_rank: 1
        entitlements: [tier:initiate]
        prices:
          - currency: usd
            unit_amount: 1200
            interval: month
      - slug: craftsman
        display_name: Craftsman
        tier_rank: 2
        entitlements: [tier:craftsman]
        prices:
          - currency: usd
            unit_amount: 2900
            interval: month
          - currency: usd
            unit_amount: 1500
            interval: month
            stripe_price_id: price_legacy123
            legacy_import: true
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
	// Interval defaults and normalization.
	if craftsman.Prices[0].Interval != "month" || craftsman.Prices[0].IntervalCount != 1 {
		t.Fatalf("interval not normalized: %+v", craftsman.Prices[0])
	}
	// legacy_import folds to archived status.
	legacy := craftsman.Prices[1]
	if legacy.Status != StatusArchived {
		t.Fatalf("legacy import should be archived, got %q", legacy.Status)
	}
	// stripe_price_id shorthand folds into provider_links.stripe.price_id.
	if got := legacy.ProviderLinks["stripe"]["price_id"]; got != "price_legacy123" {
		t.Fatalf("stripe_price_id not folded: %q", got)
	}
	// default_providers inherited.
	if got := m.providersFor(craftsman, craftsman.Prices[0]); len(got) != 1 || got[0] != "stripe" {
		t.Fatalf("providers not inherited: %v", got)
	}
}

func TestLoad_BadVersion(t *testing.T) {
	_, err := Load(writeManifest(t, "version: 2\ntier_groups: []\n"))
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("want unsupported version error, got %v", err)
	}
}

func TestLoad_DuplicatePriceByTerms(t *testing.T) {
	body := `
version: 1
tier_groups:
  - slug: g
    display_name: G
    products:
      - slug: p
        display_name: P
        tier_rank: 1
        prices:
          - {currency: usd, unit_amount: 1000, interval: month}
          - {currency: usd, unit_amount: 1000, interval: month}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "duplicate price terms") {
		t.Fatalf("want duplicate price terms error, got %v", err)
	}
}

func TestLoad_DuplicateProductSlug(t *testing.T) {
	body := `
version: 1
tier_groups:
  - slug: g1
    display_name: G1
    products:
      - {slug: p, display_name: P, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, interval: month}]}
  - slug: g2
    display_name: G2
    products:
      - {slug: p, display_name: P, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, interval: month}]}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "duplicate product slug") {
		t.Fatalf("want duplicate product slug error, got %v", err)
	}
}

func TestLoad_MissingTierRank(t *testing.T) {
	body := `
version: 1
tier_groups:
  - slug: g
    display_name: G
    products:
      - {slug: p1, display_name: P1, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, interval: month}]}
      - {slug: p2, display_name: P2, prices: [{currency: usd, unit_amount: 1, interval: month}]}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "tier_rank is required") {
		t.Fatalf("want tier_rank error, got %v", err)
	}
}

func TestLoad_TierRankOptionalForSingleProduct(t *testing.T) {
	body := `
version: 1
tier_groups:
  - slug: g
    display_name: G
    products:
      - {slug: p, display_name: P, prices: [{currency: usd, unit_amount: 1, interval: month}]}
`
	m, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.TierGroups[0].Products[0].TierRank != nil {
		t.Fatalf("single-product tier_rank should stay omitted")
	}
}

func TestLoad_TierRankAllowsZeroAndNegative(t *testing.T) {
	body := `
version: 1
tier_groups:
  - slug: g
    display_name: G
    products:
      - {slug: free, display_name: Free, tier_rank: -1, prices: [{currency: usd, unit_amount: 1, interval: month}]}
      - {slug: starter, display_name: Starter, tier_rank: 0, prices: [{currency: usd, unit_amount: 1, interval: month}]}
      - {slug: pro, display_name: Pro, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, interval: month}]}
`
	m, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ranks := ranksBySlug(m)
	if ranks["free"] != -1 || ranks["starter"] != 0 || ranks["pro"] != 1 {
		t.Fatalf("unexpected ranks: %v", ranks)
	}
}

func TestLoad_TierRankDirectionSurvivesRenumberAndNegativePrepend(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "renumber",
			body: `
version: 1
tier_groups:
  - slug: g
    display_name: G
    products:
      - {slug: starter, display_name: Starter, tier_rank: 10, prices: [{currency: usd, unit_amount: 1, interval: month}]}
      - {slug: pro, display_name: Pro, tier_rank: 20, prices: [{currency: usd, unit_amount: 1, interval: month}]}
`,
		},
		{
			name: "prepend-negative",
			body: `
version: 1
tier_groups:
  - slug: g
    display_name: G
    products:
      - {slug: free, display_name: Free, tier_rank: -1, prices: [{currency: usd, unit_amount: 1, interval: month}]}
      - {slug: starter, display_name: Starter, tier_rank: 0, prices: [{currency: usd, unit_amount: 1, interval: month}]}
      - {slug: pro, display_name: Pro, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, interval: month}]}
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := Load(writeManifest(t, tt.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			ranks := ranksBySlug(m)
			if !(ranks["starter"] < ranks["pro"]) {
				t.Fatalf("starter should downgrade from pro and pro should upgrade from starter: %v", ranks)
			}
		})
	}
}

func ranksBySlug(m *Manifest) map[string]int {
	ranks := map[string]int{}
	for _, group := range m.TierGroups {
		for _, product := range group.Products {
			ranks[product.Slug] = product.tierRank()
		}
	}
	return ranks
}

func TestLoad_SolanaNonStablecoinRejected(t *testing.T) {
	body := `
version: 1
tier_groups:
  - slug: g
    display_name: G
    products:
      - slug: p
        display_name: P
        tier_rank: 1
        providers: [solana]
        prices:
          - {currency: eur, unit_amount: 1000, interval: month}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "solana requires a stablecoin") {
		t.Fatalf("want solana eligibility error, got %v", err)
	}
}

func TestLoad_FlatProductsBenefitsUsageLimitsAndMetered(t *testing.T) {
	body := `
version: 1
usage_limits:
  - key: starter-spend
    measure: billable_spend
    windows:
      - {window: 5h, amount: 10_000_000}
meters:
  - key: api-calls
    kind: counter
products:
  - key: premium
    display_name: Premium
    entitlements: [premium]
    usage_limits: [starter-spend]
    credits:
      monthly-usd:
        currency: usd
        amount: 25_000_000
        expires: 30d
        cadence: per_renewal
    prices:
      - currency: usd
        unit_amount: 0
        providers: []
        metered: {meter: api-calls, rate: 200_000, per_units: 1_000_000}
`
	m, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.TierGroups) != 1 || m.TierGroups[0].Slug != "default" {
		t.Fatalf("flat products were not normalized: %+v", m.TierGroups)
	}
	p := m.TierGroups[0].Products[0]
	if p.Slug != "premium" || p.UsageLimits[0] != "starter-spend" {
		t.Fatalf("product benefits not normalized: %+v", p)
	}
	if got := p.Credits["monthly-usd"].Unit; got != "usd" {
		t.Fatalf("credit currency alias did not populate unit: %q", got)
	}
	if p.Prices[0].Metered.PerUnits != 1_000_000 {
		t.Fatalf("metered per_units not preserved: %+v", p.Prices[0].Metered)
	}
}

func TestLoad_MeteredGaugeRequiresPer(t *testing.T) {
	body := `
version: 1
meters:
  - {key: storage-mb, kind: gauge}
products:
  - key: storage
    display_name: Storage
    prices:
      - currency: usd
        unit_amount: 0
        providers: []
        metered: {meter: storage-mb, rate: 100}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "requires per") {
		t.Fatalf("want gauge per error, got %v", err)
	}
}

func TestLoad_MeteredRejectsInheritedProviders(t *testing.T) {
	body := `
version: 1
default_providers: [stripe]
meters:
  - {key: api-calls, kind: counter}
products:
  - key: api
    display_name: API
    prices:
      - currency: usd
        unit_amount: 0
        metered: {meter: api-calls, rate: 2_000}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "must not declare external providers") {
		t.Fatalf("want metered provider error, got %v", err)
	}
}

func TestLoad_UsageLimitReferenceMustExist(t *testing.T) {
	body := `
version: 1
products:
  - key: api
    display_name: API
    usage_limits: [missing]
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "unknown usage_limit") {
		t.Fatalf("want usage_limit reference error, got %v", err)
	}
}

func TestLoad_SolanaStablecoinAccepted(t *testing.T) {
	body := `
version: 1
tier_groups:
  - slug: g
    display_name: G
    products:
      - slug: p
        display_name: P
        tier_rank: 1
        providers: [solana]
        prices:
          - {currency: usdc, unit_amount: 1000, interval: month}
`
	if _, err := Load(writeManifest(t, body)); err != nil {
		t.Fatalf("usdc + solana should be accepted, got %v", err)
	}
}

func TestLoad_BadInterval(t *testing.T) {
	body := `
version: 1
tier_groups:
  - slug: g
    display_name: G
    products:
      - slug: p
        display_name: P
        tier_rank: 1
        prices:
          - {currency: usd, unit_amount: 1000, interval: week}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "interval must be month or year") {
		t.Fatalf("want interval error, got %v", err)
	}
}
