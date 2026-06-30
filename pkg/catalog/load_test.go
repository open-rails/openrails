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
    tier_group: cozy
    tier_rank: 1
    entitlements: [tier:initiate]
    prices:
      - currency: usd
        unit_amount: 1200
        duration: 30d
        auto_renew: true
        providers: [stripe]
  - key: craftsman
    display_name: Craftsman
    tier_group: cozy
    tier_rank: 2
    entitlements: [tier:craftsman]
    prices:
      - currency: usd
        unit_amount: 2900
        duration: 30d
        auto_renew: true
        providers: [stripe]
      - currency: usd
        unit_amount: 1500
        duration: 30d
        auto_renew: true
        active: false
        providers: [stripe]
        provider_links:
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
	if legacy.Active == nil || *legacy.Active {
		t.Fatalf("historical price should be inactive, got %v", legacy.Active)
	}
	if got := legacy.ProviderLinks["stripe"]["price_id"]; got != "price_legacy123" {
		t.Fatalf("provider_links.stripe.price_id not preserved: %q", got)
	}
	if got := craftsman.Prices[0].Providers; len(got) != 1 || got[0] != "stripe" {
		t.Fatalf("providers not normalized: %v", got)
	}
}

func TestLoad_BadVersion(t *testing.T) {
	_, err := Load(writeManifest(t, "version: 2\nproducts: []\n"))
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("want unsupported version error, got %v", err)
	}
}

func TestLoad_RejectsTierGroups(t *testing.T) {
	_, err := Load(writeManifest(t, "version: 1\ntier_groups: []\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want tier_groups unknown-field error, got %v", err)
	}
}

func TestLoad_RejectsStatus(t *testing.T) {
	_, err := Load(writeManifest(t, "version: 1\nproducts:\n  - key: p\n    display_name: P\n    status: archived\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want status unknown-field error, got %v", err)
	}
}

func TestLoad_RejectsDefaultProviders(t *testing.T) {
	_, err := Load(writeManifest(t, "version: 1\ndefault_providers: [stripe]\nproducts: []\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want default_providers unknown-field error, got %v", err)
	}
}

func TestLoad_RejectsProductProviders(t *testing.T) {
	_, err := Load(writeManifest(t, "version: 1\nproducts:\n  - key: p\n    display_name: P\n    providers: [stripe]\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want product providers unknown-field error, got %v", err)
	}
}

func TestLoad_ProviderLinksRequirePriceProvider(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    prices:
      - currency: usd
        unit_amount: 1000
        duration: 30d
        provider_links:
          stripe:
            lookup_key: p-monthly
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "requires providers to include") {
		t.Fatalf("want provider_links/provider mismatch error, got %v", err)
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

func TestLoad_DuplicatePriceByTerms(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: usd, unit_amount: 1000, duration: 30d}
      - {currency: usd, unit_amount: 1000, duration: 30d}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "duplicate price terms") {
		t.Fatalf("want duplicate price terms error, got %v", err)
	}
}

// Two prices that differ only by provider share one unique_prices_product_amount_window
// key, so the DB can hold at most one. The loader must reject the manifest up
// front rather than let it collide on the unique key at apply time.
func TestLoad_RejectsSameTermsWithDifferentProviders(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    prices:
      - {currency: usd, unit_amount: 23000000, duration: 30d, providers: [mobius, ccbill, solana]}
      - {currency: usd, unit_amount: 23000000, duration: 30d, providers: [solana], active: false}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "duplicate price terms") {
		t.Fatalf("want duplicate price terms error, got %v", err)
	}
}

// Trial is part of price identity: two prices with the same recurring substance
// but different first phases (one with a trial, one without) are distinct rows
// under the unique key, so the loader must accept both.
func TestLoad_AcceptsSameTermsWithDifferentTrials(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    prices:
      - {currency: usd, unit_amount: 23000000, duration: 30d, auto_renew: true, providers: [mobius]}
      - {currency: usd, unit_amount: 23000000, duration: 30d, auto_renew: true, providers: [mobius], trial: {unit_amount: 100, duration: 7d}}
`
	if _, err := Load(writeManifest(t, body)); err != nil {
		t.Fatalf("same terms with different trials must be accepted: %v", err)
	}
}

func TestLoad_DuplicateProductKey(t *testing.T) {
	body := `
version: 1
products:
  - {key: p, display_name: P, tier_group: g1, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, duration: 30d}]}
  - {key: p, display_name: P, tier_group: g2, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, duration: 30d}]}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "duplicate product key") {
		t.Fatalf("want duplicate product key error, got %v", err)
	}
}

func TestLoad_MissingTierRank(t *testing.T) {
	body := `
version: 1
products:
  - {key: p1, display_name: P1, tier_group: g, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, duration: 30d}]}
  - {key: p2, display_name: P2, tier_group: g, prices: [{currency: usd, unit_amount: 1, duration: 30d}]}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "tier_rank is required") {
		t.Fatalf("want tier_rank error, got %v", err)
	}
}

func TestLoad_TierRankOptionalForSingleProduct(t *testing.T) {
	body := `
version: 1
products:
  - {key: p, display_name: P, tier_group: g, prices: [{currency: usd, unit_amount: 1, duration: 30d}]}
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
products:
  - {key: free, display_name: Free, tier_group: g, tier_rank: -1, prices: [{currency: usd, unit_amount: 1, duration: 30d}]}
  - {key: starter, display_name: Starter, tier_group: g, tier_rank: 0, prices: [{currency: usd, unit_amount: 1, duration: 30d}]}
  - {key: pro, display_name: Pro, tier_group: g, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, duration: 30d}]}
`
	m, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ranks := ranksByKey(m)
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
products:
  - {key: starter, display_name: Starter, tier_group: g, tier_rank: 10, prices: [{currency: usd, unit_amount: 1, duration: 30d}]}
  - {key: pro, display_name: Pro, tier_group: g, tier_rank: 20, prices: [{currency: usd, unit_amount: 1, duration: 30d}]}
`,
		},
		{
			name: "prepend-negative",
			body: `
version: 1
products:
  - {key: free, display_name: Free, tier_group: g, tier_rank: -1, prices: [{currency: usd, unit_amount: 1, duration: 30d}]}
  - {key: starter, display_name: Starter, tier_group: g, tier_rank: 0, prices: [{currency: usd, unit_amount: 1, duration: 30d}]}
  - {key: pro, display_name: Pro, tier_group: g, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, duration: 30d}]}
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := Load(writeManifest(t, tt.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			ranks := ranksByKey(m)
			if !(ranks["starter"] < ranks["pro"]) {
				t.Fatalf("starter should downgrade from pro and pro should upgrade from starter: %v", ranks)
			}
		})
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

func TestLoad_SolanaNonStablecoinRejected(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: eur, unit_amount: 1000, duration: 30d, providers: [solana]}
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
      ai-images:
        unit: local-stack/ai-image-credit
        amount: 100
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
	if len(m.TierGroups) != 1 || m.TierGroups[0].Key != "default" {
		t.Fatalf("flat products were not normalized: %+v", m.TierGroups)
	}
	p := m.TierGroups[0].Products[0]
	if p.Key != "premium" || p.UsageLimits[0] != "starter-spend" {
		t.Fatalf("product benefits not normalized: %+v", p)
	}
	if got := p.Credits["monthly-usd"].Unit; got != "usd" {
		t.Fatalf("credit currency alias did not populate unit: %q", got)
	}
	if got := p.Credits["ai-images"].Unit; got != "local-stack/ai-image-credit" {
		t.Fatalf("qualified custom credit unit not preserved: %q", got)
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

func TestLoad_MeteredRejectsProviders(t *testing.T) {
	body := `
version: 1
meters:
  - {key: api-calls, kind: counter}
products:
  - key: api
    display_name: API
    prices:
      - currency: usd
        unit_amount: 0
        providers: [stripe]
        metered: {meter: api-calls, rate: 2_000}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "must not declare external providers") {
		t.Fatalf("want metered provider error, got %v", err)
	}
}

func TestLoad_MeteredRejectsDuplicateMeterRates(t *testing.T) {
	body := `
version: 1
meters:
  - {key: droplet-vcpu-seconds, kind: counter}
products:
  - key: basic-droplet
    display_name: Basic Droplet
    tier_group: basic
    prices:
      - currency: usd
        unit_amount: 0
        metered: {meter: droplet-vcpu-seconds, rate: 7_000, per_units: 3_600}
  - key: premium-droplet
    display_name: Premium Droplet
    tier_group: premium
    prices:
      - currency: usd
        unit_amount: 0
        metered: {meter: droplet-vcpu-seconds, rate: 14_000, per_units: 3_600}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "used by multiple metered prices") {
		t.Fatalf("want duplicate metered meter error, got %v", err)
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
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: usdc, unit_amount: 1000, duration: 30d, providers: [solana]}
`
	if _, err := Load(writeManifest(t, body)); err != nil {
		t.Fatalf("usdc + solana should be accepted, got %v", err)
	}
}

func TestLoad_BadInterval(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: usd, unit_amount: 1000, duration: 1w}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "must use h or d") {
		t.Fatalf("want interval error, got %v", err)
	}
}

func TestLoad_RejectsLegacyNamedInterval(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: usd, unit_amount: 1000, duration: month}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "whole h or d value") {
		t.Fatalf("want duration interval error, got %v", err)
	}
}

func TestLoad_NormalizesWholeDayHourInterval(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: usd, unit_amount: 1000, duration: 24h}
`
	m, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := m.TierGroups[0].Products[0].Prices[0].Duration; got != "1d" {
		t.Fatalf("duration = %q, want 1d", got)
	}
}

func TestLoad_AcceptsSubDayRecurringInterval(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: usd, unit_amount: 1000, duration: 1h}
`
	m, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := m.TierGroups[0].Products[0].Prices[0].Duration; got != "1h" {
		t.Fatalf("duration = %q, want 1h", got)
	}
}
