package postgresmigrations

import (
	"strings"
	"testing"

	"github.com/open-rails/migratekit"
)

// collapseWS lowercases and collapses runs of whitespace to single spaces, so
// substring assertions below are insensitive to the migration file's column
// alignment.
func collapseWS(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

func loadSchemaMigration(t *testing.T) string {
	t.Helper()
	migrations, err := migratekit.LoadFromFS(FS)
	if err != nil {
		t.Fatalf("LoadFromFS: %v", err)
	}
	for _, m := range migrations {
		if strings.HasPrefix(m.Name, "001_") {
			return m.Content
		}
	}
	t.Fatal("migration 001_* not found among loaded up migrations")
	return ""
}

// TestCatalogDedupConstraints guarantees the catalog cannot hold duplicate
// products or prices — the property that makes "a row only exists if it
// meaningfully differs" true at the database layer rather than by convention.
//
//   - A product's identity is its merchant plus key (UNIQUE).
//   - A price's identity is its financial substance: (product, amount, currency,
//     access window, auto_renew, trial terms). That is EXACTLY the content key the
//     provider adapters reuse as the Stripe lookup_key, the NMI plan_id, and the
//     Solana plan PDA — so the local uniqueness and the provider de-dup key are one
//     and the same.
//
// These two constraints are what make duplicate products/prices (and therefore
// duplicate provider objects) structurally impossible. If either is ever dropped,
// OpenRails could hold two rows for one logical product/price and push duplicate
// objects to Stripe/NMI/Solana. This test fails the build to prevent that
// regression.
func TestCatalogDedupConstraints(t *testing.T) {
	schema := collapseWS(loadSchemaMigration(t))

	for _, want := range []string{
		// Product identity = merchant + key.
		"constraint products_merchant_key_key unique (merchant_id, key)",
		// Price identity = financial substance (the provider/content de-dup key).
		"constraint unique_prices_product_amount_window",
		"unique nulls not distinct (product_id, amount, currency, access_duration_hours, auto_renew, trial_unit_amount, trial_duration_hours)",
	} {
		if !strings.Contains(schema, want) {
			t.Errorf("catalog de-dup constraint missing from schema migration: %q", want)
		}
	}
}

func TestSubscriptionPriceProductConstraint(t *testing.T) {
	schema := collapseWS(loadSchemaMigration(t))

	for _, want := range []string{
		"create unique index uq_prices_id_product_merchant on openrails.prices using btree (id, product_id, merchant_id)",
		"constraint subscriptions_price_product_merchant_fkey foreign key (price_id, product_id, merchant_id) references openrails.prices(id, product_id, merchant_id)",
	} {
		if !strings.Contains(schema, want) {
			t.Errorf("subscription price/product constraint missing from schema migration: %q", want)
		}
	}
}
