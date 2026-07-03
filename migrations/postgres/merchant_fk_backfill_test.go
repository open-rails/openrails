package postgresmigrations

import (
	"strings"
	"testing"
)

// loadBaselineMigration reads the raw SQL of the consolidated baseline (001) from the embedded FS.
// Originally targeted migration 028; after squashing 001..029 the FK constraints live in 001.
func loadBaselineMigration(t *testing.T) string {
	t.Helper()
	b, err := FS.ReadFile("0001_schema.up.sql")
	if err != nil {
		t.Fatalf("read 0001_schema.up.sql: %v", err)
	}
	return string(b)
}

// TestMerchantFKBackfillConstraintsPresent asserts that the consolidated baseline (001)
// contains ON DELETE RESTRICT merchant FKs for every core table.
// Originally validated migration 028; after squashing 001..029 into a single
// baseline the check now targets 0001_schema.up.sql.
// This is a static SQL-text test (no live DB required); the integration test
// suite (scripts/test_integration.sh) verifies the constraints actually apply.
func TestMerchantFKBackfillConstraintsPresent(t *testing.T) {
	sql := collapseWS(loadBaselineMigration(t))

	type wantFK struct {
		table      string
		constraint string
	}
	cases := []wantFK{
		{"products", "products_merchant_fk"},
		{"prices", "prices_merchant_fk"},
		{"payment_methods", "payment_methods_merchant_fk"},
		{"checkout_sessions", "checkout_sessions_merchant_fk"},
		{"grants", "grants_merchant_fk"},
		// #709 FK doctrine: the money core + operator tables carry the same
		// merchants(id) ON DELETE RESTRICT FK (merchant removal is a tombstone
		// plus gated purge, never a row DELETE).
		{"payments", "payments_merchant_fk"},
		{"subscriptions", "subscriptions_merchant_fk"},
		{"invoices", "invoices_merchant_fk"},
		{"invoice_items", "invoice_items_merchant_fk"},
		{"invoice_payments", "invoice_payments_merchant_fk"},
		{"money_settings", "money_settings_merchant_fk"},
		{"ledger_accounts", "ledger_accounts_merchant_fk"},
		{"ledger_transfers", "ledger_transfers_merchant_fk"},
		{"usage_events", "usage_events_merchant_fk"},
		{"entitlements", "entitlements_merchant_fk"},
		{"rail_intents", "rail_intents_merchant_fk"},
		{"rail_customer_accounts", "rail_customer_accounts_merchant_fk"},
		{"reconciliation_runs", "reconciliation_runs_merchant_fk"},
		{"reconciliation_findings", "reconciliation_findings_merchant_fk"},
		{"reconciliation_state", "reconciliation_state_merchant_fk"},
		{"catalog_drift_events", "catalog_drift_events_merchant_fk"},
		{"custom_credit_types", "custom_credit_types_merchant_fk"},
		{"notification_queue", "notification_queue_merchant_fk"},
		{"tier_schedules", "tier_schedules_merchant_fk"},
		{"payer_spend_limits", "payer_spend_limits_merchant_fk"},
		{"invoker_spend_limits", "invoker_spend_limits_merchant_fk"},
		{"solana_subscriptions", "solana_subscriptions_merchant_fk"},
		{"merchant_deks", "merchant_deks_merchant_fk"},
		{"merchant_secrets", "merchant_secrets_merchant_fk"},
		{"merchant_exports", "merchant_exports_merchant_fk"},
	}

	for _, c := range cases {
		t.Run(c.table, func(t *testing.T) {
			// Constraint name must appear in an ADD CONSTRAINT statement.
			addConstraintStr := "add constraint " + c.constraint
			if !strings.Contains(sql, addConstraintStr) {
				t.Errorf("constraint %q not found as ADD CONSTRAINT in baseline migration", c.constraint)
			}
			// Must reference merchants(id).
			wantRef := "references openrails.merchants(id)"
			// Extract a bounded snippet: from the ADD CONSTRAINT clause to the next ";".
			// This prevents false positives from pg_dump comment headers and later FK clauses.
			idx := strings.Index(sql, addConstraintStr)
			if idx == -1 {
				return // already reported above
			}
			afterName := sql[idx:]
			semiIdx := strings.Index(afterName, ";")
			var snippet string
			if semiIdx >= 0 {
				snippet = afterName[:semiIdx+1]
			} else {
				snippet = afterName[:minLen(len(afterName), 300)]
			}
			if !strings.Contains(snippet, wantRef) {
				t.Errorf("constraint %q does not reference openrails.merchants(id), got: %q", c.constraint, snippet)
			}
			// Must be ON DELETE RESTRICT (not CASCADE).
			if !strings.Contains(snippet, "on delete restrict") {
				t.Errorf("constraint %q must use ON DELETE RESTRICT, got: %q", c.constraint, snippet)
			}
			// Must NOT be ON DELETE CASCADE.
			if strings.Contains(snippet, "on delete cascade") {
				t.Errorf("constraint %q must not use ON DELETE CASCADE, got: %q", c.constraint, snippet)
			}
		})
	}
}

// TestMerchantFKBackfillNoCascadeOnFinancialTables is a belt-and-suspenders
// guard: the MERCHANT FKs for core financial tables must use ON DELETE RESTRICT, not CASCADE.
// The consolidated baseline may have ON DELETE CASCADE for other (non-merchant) FK relationships,
// so we check each merchant FK constraint snippet individually.
func TestMerchantFKBackfillNoCascadeOnFinancialTables(t *testing.T) {
	sql := collapseWS(loadBaselineMigration(t))
	merchantFKConstraints := []string{
		"products_merchant_fk",
		"prices_merchant_fk",
		"payment_methods_merchant_fk",
		"checkout_sessions_merchant_fk",
		"grants_merchant_fk",
	}
	for _, name := range merchantFKConstraints {
		addStr := "add constraint " + name
		idx := strings.Index(sql, addStr)
		if idx == -1 {
			t.Errorf("merchant FK constraint %q not found as ADD CONSTRAINT in baseline", name)
			continue
		}
		afterName := sql[idx:]
		semiIdx := strings.Index(afterName, ";")
		var snippet string
		if semiIdx >= 0 {
			snippet = afterName[:semiIdx+1]
		} else {
			snippet = afterName[:minLen(len(afterName), 300)]
		}
		if strings.Contains(snippet, "on delete cascade") {
			t.Errorf("merchant FK constraint %q must not use ON DELETE CASCADE, got: %q", name, snippet)
		}
	}
}

// TestMerchantFKBackfillDoesNotTouchAlreadyCoveredTables checks that the baseline
// does not contain duplicate ADD CONSTRAINT for FKs that were added inline in table
// definitions. In the consolidated baseline all constraints should appear exactly once.
// We check for "add constraint <name>" specifically (not mere name occurrence in
// comments) to avoid false positives.
func TestMerchantFKBackfillDoesNotTouchAlreadyCoveredTables(t *testing.T) {
	sql := collapseWS(loadBaselineMigration(t))
	alreadyCovered := []string{
		"customers_merchant_id_fkey",
		"merchant_configurations_merchant_fk",
		"provider_accounts_merchant_fk",
		"external_provider_mutation_logs_merchant_fk",
		"provider_refresh_watermarks_merchant_fk",
	}
	for _, name := range alreadyCovered {
		addConstraint := "add constraint " + name
		count := strings.Count(sql, addConstraint)
		if count > 1 {
			t.Errorf("baseline must not contain duplicate ADD CONSTRAINT for FK %q (found %d)", name, count)
		}
	}
}

func minLen(a, b int) int {
	if a < b {
		return a
	}
	return b
}
