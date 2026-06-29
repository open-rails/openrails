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
