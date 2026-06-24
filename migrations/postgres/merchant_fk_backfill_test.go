package postgresmigrations

import (
	"strings"
	"testing"
)

// load028Migration reads the raw SQL of migration 028 from the embedded FS.
func load028Migration(t *testing.T) string {
	t.Helper()
	b, err := FS.ReadFile("028_merchant_fk_backfill.up.sql")
	if err != nil {
		t.Fatalf("read 028_merchant_fk_backfill.up.sql: %v", err)
	}
	return string(b)
}

// TestMerchantFKBackfillConstraintsPresent asserts that migration 028 adds an
// ON DELETE RESTRICT merchant FK for every core table that lacked one.
// This is a static SQL-text test (no live DB required); the integration test
// suite (scripts/test_integration.sh) verifies the constraints actually apply.
func TestMerchantFKBackfillConstraintsPresent(t *testing.T) {
	sql := collapseWS(load028Migration(t))

	type wantFK struct {
		table      string
		constraint string
	}
	cases := []wantFK{
		{"products", "products_merchant_fk"},
		{"prices", "prices_merchant_fk"},
		{"entitlement_features", "entitlement_features_merchant_fk"},
		{"product_entitlement_features", "product_entitlement_features_merchant_fk"},
		{"payment_methods", "payment_methods_merchant_fk"},
		{"checkout_sessions", "checkout_sessions_merchant_fk"},
		{"grants", "grants_merchant_fk"},
	}

	for _, c := range cases {
		t.Run(c.table, func(t *testing.T) {
			// Constraint name must appear.
			if !strings.Contains(sql, c.constraint) {
				t.Errorf("constraint %q not found in 028 migration", c.constraint)
			}
			// Must reference merchants(id).
			wantRef := "references openrails.merchants(id)"
			// Check in the vicinity of the constraint name.
			idx := strings.Index(sql, c.constraint)
			if idx == -1 {
				return // already reported above
			}
			snippet := sql[idx:]
			if !strings.Contains(snippet, wantRef) {
				t.Errorf("constraint %q does not reference openrails.merchants(id)", c.constraint)
			}
			// Must be ON DELETE RESTRICT (not CASCADE).
			if !strings.Contains(snippet, "on delete restrict") {
				t.Errorf("constraint %q must use ON DELETE RESTRICT, got: %q", c.constraint, snippet[:minLen(len(snippet), 200)])
			}
			// Must NOT be ON DELETE CASCADE.
			if strings.Contains(snippet, "on delete cascade") {
				t.Errorf("constraint %q must not use ON DELETE CASCADE", c.constraint)
			}
		})
	}
}

// TestMerchantFKBackfillNoCascadeOnFinancialTables is a belt-and-suspenders
// guard: the entire 028 migration must not contain ON DELETE CASCADE anywhere.
func TestMerchantFKBackfillNoCascadeOnFinancialTables(t *testing.T) {
	sql := collapseWS(load028Migration(t))
	if strings.Contains(sql, "on delete cascade") {
		t.Error("028 migration must not use ON DELETE CASCADE for any merchant FK")
	}
}

// TestMerchantFKBackfillDoesNotTouchAlreadyCoveredTables checks that 028 does
// not re-add FKs for tables already covered by earlier migrations.
// We check for "add constraint <name>" specifically (not mere name occurrence in
// comments) to avoid false positives from the documentation block at the top of
// the migration.
func TestMerchantFKBackfillDoesNotTouchAlreadyCoveredTables(t *testing.T) {
	sql := collapseWS(load028Migration(t))
	alreadyCovered := []string{
		"customers_merchant_id_fkey",
		"merchant_configurations_merchant_fk",
		"provider_accounts_merchant_fk",
		"external_provider_mutation_logs_merchant_fk",
		"provider_refresh_watermarks_merchant_fk",
	}
	for _, name := range alreadyCovered {
		addConstraint := "add constraint " + name
		if strings.Contains(sql, addConstraint) {
			t.Errorf("028 migration must not re-add already-existing FK %q", name)
		}
	}
}

func minLen(a, b int) int {
	if a < b {
		return a
	}
	return b
}
