package embedded

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
// catalog.Manifest.Validate per merchant) and asserts the full v1 product
// surface is present and valid. Mirrors TestExampleMerchantConfigManifestParses.
// This keeps the example from rotting: if a manifest-schema change breaks the
// example, this test fails.
func TestExampleCatalogManifestParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "catalog.example.yaml"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}

	targets, err := loadCatalogPushTargets(CatalogPushOptions{Manifest: raw})
	if err != nil {
		t.Fatalf("loadCatalogPushTargets (example failed to parse/validate): %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("len(targets) = %d, want 1", len(targets))
	}
	target := targets[0]
	if target.Merchant != "local-stack" {
		t.Fatalf("merchant = %q, want local-stack", target.Merchant)
	}
	m := target.Manifest

	// manifest.Validate already ran inside loadCatalogPushTargets; assert it is
	// idempotently happy on the normalized manifest too.
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest.Validate: %v", err)
	}

	// Flatten products across tier groups (Validate buckets them by tier_group).
	products := map[string]bool{}
	var (
		sawMeteredPrice bool
		sawIntro        bool
		sawCredits      bool
		sawUsageLimit   bool
		sawIncludes     bool
	)
	for _, g := range m.TierGroups {
		for _, p := range g.Products {
			products[p.Key] = true
			if len(p.UsageLimits) > 0 {
				sawUsageLimit = true
			}
			if len(p.Includes) > 0 {
				sawIncludes = true
			}
			if len(p.Credits) > 0 {
				sawCredits = true
			}
			for _, pr := range p.Prices {
				if pr.Metered != nil {
					sawMeteredPrice = true
				}
				if pr.Trial != nil {
					sawIntro = true
				}
			}
		}
	}

	// Every documented use case is represented by at least one product key.
	for _, key := range []string{
		"basic", "premium", // subscription tiers
		"creator-pro",                     // free-trial / intro pricing
		"ai-image-credits", "api-credits", // custom credit grants
		"movie-classic", "movie-premiere", // content marketplace (ownership)
		"movie-bundle",            // bundle via includes
		"claude-5x", "claude-20x", // usage-limit plan variants
		"compute-metered", "storage-metered", // metered billing
	} {
		if !products[key] {
			t.Errorf("example missing expected product key %q", key)
		}
	}

	// Advanced-feature surfaces are all exercised.
	if !sawIntro {
		t.Error("example has no price with intro/free-trial pricing")
	}
	if !sawCredits {
		t.Error("example has no product granting custom credits")
	}
	if !sawUsageLimit {
		t.Error("example has no product bound to a usage_limit")
	}
	if !sawIncludes {
		t.Error("example has no bundle product using includes")
	}
	if !sawMeteredPrice {
		t.Error("example has no metered price")
	}

	// Top-level meters and usage_limits are declared.
	if len(m.Meters) < 2 {
		t.Errorf("example declares %d meters, want >= 2 (counter + gauge)", len(m.Meters))
	}
	if len(m.UsageLimits) < 2 {
		t.Errorf("example declares %d usage_limits, want >= 2 (5x + 20x)", len(m.UsageLimits))
	}
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
	if targets[0].Manifest.TierGroups[0].Slug != "smoke" {
		t.Fatalf("tier group = %q, want smoke", targets[0].Manifest.TierGroups[0].Slug)
	}
}
