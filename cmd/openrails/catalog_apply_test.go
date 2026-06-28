package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCatalogManifest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write catalog manifest: %v", err)
	}
	return path
}

func TestLoadCatalogTargetsRequiresMerchantPerCatalog(t *testing.T) {
	path := writeCatalogManifest(t, `version: 1
catalogs:
  - name: missing-merchant
    tier_groups:
      - slug: smoke
        display_name: Smoke
        products:
          - slug: smoke-basic
            display_name: Smoke Basic
            tier_rank: 1
            prices:
              - currency: usd
                unit_amount: 100
`)

	_, err := loadCatalogTargets(path)
	if err == nil || !strings.Contains(err.Error(), "catalog #1 merchant is required") {
		t.Fatalf("loadCatalogTargets err = %v, want merchant-required error", err)
	}
}

func TestLoadCatalogTargetsRejectsAuthAndMerchants(t *testing.T) {
	path := writeCatalogManifest(t, `version: 1
auth:
  orgs:
    - slug: doujins
merchants:
  - slug: doujins
    name: Doujins
catalogs:
  - merchant: doujins
    tier_groups: []
`)

	_, err := loadCatalogTargets(path)
	if err == nil || !strings.Contains(err.Error(), "auth") {
		t.Fatalf("loadCatalogTargets err = %v, want auth unknown-field error", err)
	}
}

func TestLoadCatalogTargetsParsesMultiMerchantCatalogs(t *testing.T) {
	path := writeCatalogManifest(t, `version: 1
catalogs:
  - merchant: doujins
    name: doujins-default
    tier_groups:
      - slug: smoke
        display_name: Smoke
        products:
          - slug: smoke-basic
            display_name: Smoke Basic
            tier_rank: 1
            prices:
              - currency: usd
                unit_amount: 100
  - merchant: cozy-art
    tier_groups:
      - slug: premium
        display_name: Premium
        products:
          - slug: premium-basic
            display_name: Premium Basic
            tier_rank: 1
            prices:
              - currency: usd
                unit_amount: 200
`)

	targets, err := loadCatalogTargets(path)
	if err != nil {
		t.Fatalf("loadCatalogTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("len(targets) = %d, want 2", len(targets))
	}
	if targets[0].Merchant != "doujins" || targets[1].Merchant != "cozy-art" {
		t.Fatalf("merchants = %q, %q", targets[0].Merchant, targets[1].Merchant)
	}
}
