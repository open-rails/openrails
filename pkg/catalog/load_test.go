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
      - {slug: p, display_name: P, prices: [{currency: usd, unit_amount: 1, interval: month}]}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "tier_rank must be positive") {
		t.Fatalf("want tier_rank error, got %v", err)
	}
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
