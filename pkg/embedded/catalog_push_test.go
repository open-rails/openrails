package embedded

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-rails/openrails/pkg/catalog"
)

func writeCatalogPushManifest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write catalog manifest: %v", err)
	}
	return path
}

// TestExampleCatalogManifestParses loads the canonical config/catalog.example.yaml
// through the real publish path (loadCatalogPushTargets, which runs
// catalog.Manifest.Validate per merchant) and asserts the multi-merchant example
// is valid and exercises both the legacy surfaces (usage_limits, credit grants,
// subscription prices) and the #638/#639 rate-card model (aggregation meters,
// matrix pricing with a monthly cap, allowances, and a variable credit top-up).
// This keeps the example from rotting: a manifest-schema change that breaks it
// fails here.
func TestExampleCatalogManifestParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "catalog.example.yaml"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}

	targets, err := loadCatalogPushTargets(CatalogPushOptions{Manifest: raw})
	if err != nil {
		t.Fatalf("loadCatalogPushTargets (example failed to parse/validate): %v", err)
	}

	byMerchant := map[string]*catalog.Manifest{}
	for _, tg := range targets {
		if err := tg.Manifest.Validate(); err != nil { // idempotent re-validate
			t.Fatalf("merchant %q manifest.Validate: %v", tg.Merchant, err)
		}
		byMerchant[tg.Merchant] = tg.Manifest
	}
	for _, want := range []string{"anthropic", "digital-ocean", "cozy-creator", "tensorhub"} {
		if byMerchant[want] == nil {
			t.Fatalf("example missing merchant %q (got %d merchants)", want, len(targets))
		}
	}

	// anthropic: legacy usage_limits + subscription prices still validate (additive).
	if len(byMerchant["anthropic"].UsageLimits) < 1 {
		t.Error("anthropic should declare usage_limits")
	}
	// tensorhub: legacy credit grants still validate (additive).
	if len(flattenProducts(byMerchant["tensorhub"])) < 2 {
		t.Error("tensorhub should declare prepaid products")
	}

	// ---- digital-ocean: the #638 rate-card model ----
	do := byMerchant["digital-ocean"]
	sawAggMeter := false
	for _, mt := range do.Meters {
		if mt.Aggregation != "" {
			sawAggMeter = true
		}
	}
	if !sawAggMeter {
		t.Error("digital-ocean has no rate-card meter (aggregation)")
	}
	doProducts := flattenProducts(do)
	droplet, ok := doProducts["droplet"]
	if !ok {
		t.Fatal("digital-ocean missing droplet product")
	}
	if droplet.BillingCadence == "" {
		t.Error("droplet product missing billing_cadence")
	}
	var dropletPrice *catalog.RatePrice
	for i := range droplet.RateCards {
		if droplet.RateCards[i].Price.Matrix != nil {
			dropletPrice = &droplet.RateCards[i].Price
		}
	}
	if dropletPrice == nil {
		t.Fatal("droplet has no matrix price")
	}
	// The s-1vcpu-1gb SKU pro-rates per second and caps at its $6/mo price:
	// a full 720h month exceeds the 672h cap, so it bills exactly the maximum.
	cm, ok := dropletPrice.ChargeModelForCell("s-1vcpu-1gb")
	if !ok {
		t.Fatal("droplet matrix missing s-1vcpu-1gb cell")
	}
	cost, err := cm.Rate(720 * 3600)
	if err != nil {
		t.Fatal(err)
	}
	if cost != 6_000_000 {
		t.Errorf("full-month s-1vcpu-1gb droplet = %d micros, want 6000000 (capped)", cost)
	}
	sawAllowance := false
	for _, p := range doProducts {
		for _, rc := range p.RateCards {
			if rc.Allowance != nil {
				sawAllowance = true
			}
		}
	}
	if !sawAllowance {
		t.Error("digital-ocean has no allowance (pooled bandwidth)")
	}

	// ---- cozy-creator: 4 membership tiers + variable credit top-up (#639/#640) ----
	cozyProducts := flattenProducts(byMerchant["cozy-creator"])
	for _, key := range []string{"novice", "craftsman", "expert", "grandmaster"} {
		if _, ok := cozyProducts[key]; !ok {
			t.Errorf("cozy-creator missing membership tier %q", key)
		}
	}
	topup, ok := cozyProducts["image-credit-topup"]
	if !ok || topup.CreditPurchase == nil {
		t.Fatal("cozy-creator missing image-credit-topup credit_purchase")
	}
	cp := topup.CreditPurchase
	if cp.Price.Model != catalog.ModelTiered || cp.Price.Mode != catalog.TierModeGraduated {
		t.Errorf("credit top-up must be graduated tiered, got %s/%s", cp.Price.Model, cp.Price.Mode)
	}
	// $20 buys exactly 2,000 credits at the $0.01/credit base band (bidirectional quote).
	credits, spent, err := catalog.QuoteUnitsForSpend(20_000_000, cp.Price.ToChargeModel())
	if err != nil {
		t.Fatal(err)
	}
	if credits != 2_000 || spent != 20_000_000 {
		t.Errorf("$20 top-up -> (%d credits, %d spent), want (2000, 20000000)", credits, spent)
	}
}

func flattenProducts(m *catalog.Manifest) map[string]catalog.Product {
	out := map[string]catalog.Product{}
	for _, g := range m.TierGroups {
		for _, p := range g.Products {
			out[p.Key] = p
		}
	}
	return out
}

func TestLoadCatalogPushTargetsRequiresMerchantPerCatalog(t *testing.T) {
	path := writeCatalogPushManifest(t, `version: 1
catalogs:
  - products:
      - key: smoke-basic
        display_name: Smoke Basic
        tier_group: smoke
        tier_rank: 1
        prices:
          - currency: usd
            unit_amount: 100
`)

	_, err := loadCatalogPushTargets(CatalogPushOptions{File: path})
	if err == nil || !strings.Contains(err.Error(), "catalog #1 merchant is required") {
		t.Fatalf("loadCatalogPushTargets err = %v, want merchant-required error", err)
	}
}

func TestLoadCatalogPushTargetsRejectsAuthAndMerchants(t *testing.T) {
	path := writeCatalogPushManifest(t, `version: 1
auth:
  permission_groups:
    - slug: doujins
merchants:
  - slug: doujins
    name: Doujins
catalogs:
  - merchant: doujins
    products: []
`)

	_, err := loadCatalogPushTargets(CatalogPushOptions{File: path})
	if err == nil || !strings.Contains(err.Error(), "auth") {
		t.Fatalf("loadCatalogPushTargets err = %v, want auth unknown-field error", err)
	}
}

func TestLoadCatalogPushTargetsParsesMultiMerchantCatalogs(t *testing.T) {
	raw := []byte(`version: 1
catalogs:
  - merchant: doujins
    products:
      - key: smoke-basic
        display_name: Smoke Basic
        tier_group: smoke
        tier_rank: 1
        prices:
          - currency: usd
            unit_amount: 100
  - merchant: cozy-art
    products:
      - key: premium-basic
        display_name: Premium Basic
        tier_group: premium
        tier_rank: 1
        prices:
          - currency: usd
            unit_amount: 200
`)

	targets, err := loadCatalogPushTargets(CatalogPushOptions{Manifest: raw})
	if err != nil {
		t.Fatalf("loadCatalogPushTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("len(targets) = %d, want 2", len(targets))
	}
	if targets[0].Merchant != "doujins" || targets[1].Merchant != "cozy-art" {
		t.Fatalf("merchants = %q, %q", targets[0].Merchant, targets[1].Merchant)
	}
	if targets[0].Manifest.TierGroups[0].Key != "smoke" {
		t.Fatalf("tier group = %q, want smoke", targets[0].Manifest.TierGroups[0].Key)
	}
}
