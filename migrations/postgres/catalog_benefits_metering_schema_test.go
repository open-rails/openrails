package postgresmigrations

import (
	"strings"
	"testing"
)

func TestCatalogBenefitsMeteringMigration(t *testing.T) {
	b, err := FS.ReadFile("034_catalog_benefits_metering.up.sql")
	if err != nil {
		t.Fatalf("read 034_catalog_benefits_metering.up.sql: %v", err)
	}
	sql := string(b)

	for _, want := range []string{
		"CREATE TABLE openrails.catalog_usage_limits",
		"CREATE TABLE openrails.product_usage_limit_bindings",
		"CREATE TABLE openrails.catalog_meters",
		"CREATE TABLE openrails.catalog_price_metered",
		"CREATE TABLE openrails.product_includes",
		"catalog_meters_kind_check CHECK",
		"catalog_price_metered_rate_positive CHECK",
		"COMMENT ON TABLE openrails.catalog_meters IS '#599 billing meter registry",
		"COMMENT ON TABLE openrails.product_usage_limit_bindings IS '#594 materialized product-derived usage-limit bindings",
		"COMMENT ON TABLE openrails.product_includes IS '#611 catalog bundle includes",
		"ENABLE ROW LEVEL SECURITY",
	} {
		if !strings.Contains(sql, want) && !strings.Contains(readMigration(t, "035_catalog_product_includes.up.sql"), want) {
			t.Errorf("catalog benefit/metering migration missing %q", want)
		}
	}

	for _, forbidden := range []string{
		"ALTER TABLE openrails.products ADD COLUMN",
		"ALTER TABLE openrails.prices ADD COLUMN",
		"ALTER TABLE openrails.usage_events ADD COLUMN",
		"CREATE TABLE openrails.usage_limit_counters",
	} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("catalog benefit/metering migration should stay sidecar/no-hot-counter; found %q", forbidden)
		}
	}
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	b, err := FS.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
