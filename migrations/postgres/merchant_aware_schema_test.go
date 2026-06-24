package postgresmigrations

import (
	"regexp"
	"strings"
	"testing"
)

// Merchant-owned tables that the consolidated schema must scope by merchant_id
// (issue #223). Kept in sync with the merchant_isolation policies in 001.
// Updated after squashing 001..029: removed tables that were dropped during the
// migration chain (budget_inflight_holds, budget_window_state, money_transactions,
// money_blocks, money_spend_limits, money_windows) and added tables added in
// later migrations (external_provider_mutation_logs, grants, ledger_accounts,
// ledger_transfers, provider_accounts, provider_refresh_watermarks, reconciliation_state).
var merchantOwnedTables = []string{
	"customers",
	"merchant_deks",
	"merchant_secrets",
	"merchant_exports",
	"merchant_credential_audit",
	"products",
	"prices",
	"entitlement_features",
	"product_entitlement_features",
	"payment_methods",
	"subscriptions",
	"payments",
	"checkout_sessions",
	"usdc_funding_sessions",
	"solana_subscriptions",
	"entitlements",
	"invoices",
	"invoice_items",
	"invoice_payments",
	"notification_queue",
	"payment_blocklist",
	"rail_customers",
	"external_provider_mutation_logs",
	"grants",
	"ledger_accounts",
	"ledger_transfers",
	"provider_accounts",
	"provider_intents",
	"provider_refresh_watermarks",
	"reconciliation_state",
	// #472: money ledger replaced the credit_* tables. money_balances dropped (#491).
	"invoker_spend_limits",
	"payer_spend_limits",
	"tier_schedules",
	"merchant_configurations",
	"usage_events",
	"catalog_drift_events",
	"reconciliation_runs",
	"reconciliation_findings",
	"money_settings",
	"custom_credit_types",
}

func loadSchema001(t *testing.T) string {
	t.Helper()
	b, err := FS.ReadFile("001_schema.up.sql")
	if err != nil {
		t.Fatalf("read 001_schema.up.sql: %v", err)
	}
	return string(b)
}

// #336: there is no default merchant. The consolidated schema creates the
// merchants table but seeds no rows — merchants (and their credit types) are
// provisioned explicitly by the control plane / bootstrap, never defaulted.
func TestConsolidatedSchemaHasNoDefaultMerchant(t *testing.T) {
	schema := loadSchema001(t)

	if !strings.Contains(schema, "CREATE TABLE openrails.merchants") {
		t.Error("001 schema must create openrails.merchants")
	}
	if strings.Contains(schema, "INSERT INTO openrails.merchants") {
		t.Error("001 schema must not seed any merchant row")
	}
}

func TestConsolidatedSchemaMerchantIDColumns(t *testing.T) {
	c := loadSchema001(t)

	if strings.Contains(c, "WHERE merchant_id IS NULL") {
		t.Error("consolidated schema must not contain merchant_id backfill logic")
	}
	if strings.Contains(c, "merchant_id uuid NOT NULL DEFAULT") {
		t.Error("consolidated schema must not default merchant_id")
	}
	if strings.Contains(c, "current_setting('app.merchant_id'::text, true), ''::text))::uuid,") {
		t.Error("consolidated schema must not default merchant_id from app.merchant_id")
	}
	for _, tbl := range merchantOwnedTables {
		wantTable := "CREATE TABLE openrails." + tbl
		if !strings.Contains(c, wantTable) {
			t.Errorf("001 schema missing merchant-owned table %q", tbl)
		}
		if !strings.Contains(tableBlock(t, c, tbl), "merchant_id uuid NOT NULL") {
			t.Errorf("001 schema merchant-owned table %q must declare merchant_id uuid NOT NULL", tbl)
		}
		if !strings.Contains(c, "ALTER TABLE ONLY openrails."+tbl+" FORCE ROW LEVEL SECURITY") {
			t.Errorf("001 schema missing FORCE ROW LEVEL SECURITY for %q", tbl)
		}
		if !strings.Contains(c, "ALTER TABLE openrails."+tbl+" ENABLE ROW LEVEL SECURITY") {
			t.Errorf("001 schema missing ENABLE ROW LEVEL SECURITY for %q", tbl)
		}
		if !strings.Contains(c, "CREATE POLICY merchant_isolation ON openrails."+tbl) {
			t.Errorf("001 schema missing merchant_isolation policy for %q", tbl)
		}
	}
}

func TestConsolidatedSchemaClassifiesGlobalTables(t *testing.T) {
	c := loadSchema001(t)
	for _, want := range []string{
		"COMMENT ON TABLE openrails.merchants IS 'Merchant / billing-namespace directory",
		"GLOBAL (control-plane) table",
		"COMMENT ON TABLE openrails.probe_verdicts IS 'Cached NMI test-mode probe verdicts",
		"RLS-exempt by design: instance-level credential state, not tenant data",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("001 schema missing global/control-plane table classification %q", want)
		}
	}
}

func TestConsolidatedSchemaHasNoMerchantSettingsTable(t *testing.T) {
	c := loadSchema001(t)
	if strings.Contains(c, "CREATE TABLE openrails.merchant_settings") {
		t.Error("merchant_settings is not a live table; merchant_configurations is the RLS-protected merchant settings table")
	}
	if !strings.Contains(c, "CREATE TABLE openrails.merchant_configurations") {
		t.Error("001 schema missing merchant_configurations")
	}
	if !strings.Contains(c, "CREATE POLICY merchant_isolation ON openrails.merchant_configurations") {
		t.Error("merchant_configurations must remain RLS protected")
	}
}

func TestEveryMerchantIDTableRequiresRLS(t *testing.T) {
	c := loadSchema001(t)
	covered := make(map[string]bool, len(merchantOwnedTables))
	for _, tbl := range merchantOwnedTables {
		covered[tbl] = true
	}
	for tbl, body := range createTableBlocks(c) {
		if !strings.Contains(body, "merchant_id") {
			continue
		}
		if !covered[tbl] {
			t.Errorf("table %q has merchant_id but is not listed in merchantOwnedTables", tbl)
			continue
		}
		for _, want := range []string{
			"ALTER TABLE ONLY openrails." + tbl + " FORCE ROW LEVEL SECURITY",
			"ALTER TABLE openrails." + tbl + " ENABLE ROW LEVEL SECURITY",
			"CREATE POLICY merchant_isolation ON openrails." + tbl,
		} {
			if !strings.Contains(c, want) {
				t.Errorf("merchant_id table %q missing %q", tbl, want)
			}
		}
	}
	for _, tbl := range merchantOwnedTables {
		if _, ok := createTableBlocks(c)[tbl]; !ok {
			t.Errorf("merchantOwnedTables includes missing table %q", tbl)
		}
	}
}

func createTableBlocks(schema string) map[string]string {
	re := regexp.MustCompile(`(?s)CREATE TABLE openrails\.([a-z0-9_]+) \((.*?)\n\);`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(schema, -1) {
		out[m[1]] = m[2]
	}
	return out
}

func tableBlock(t *testing.T, schema, table string) string {
	t.Helper()
	block, ok := createTableBlocks(schema)[table]
	if !ok {
		t.Fatalf("missing CREATE TABLE block for %s", table)
	}
	return block
}

func TestConsolidatedSchemaUsesCustomerUniques(t *testing.T) {
	c := loadSchema001(t)

	for _, forbidden := range []string{
		"uq_payment_methods_tenant_user_vault",
		"uq_subscriptions_tenant_user_product_lifecycle",
		"uq_entitlements_tenant_active",
		"uq_rail_customers_tenant_user_rail",
		" user_id text",
	} {
		if strings.Contains(c, forbidden) {
			t.Errorf("001 schema must not keep legacy user-scoped artifact %q", forbidden)
		}
	}
	for _, want := range []string{
		"uq_payment_methods_customer_vault",
		"uq_subscriptions_customer_product_lifecycle",
		"uq_entitlements_customer_active",
		"uq_payments_merchant_rail_transaction",
		"uq_rail_customers_customer_rail",
		"entitlements_customer_no_overlap",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("001 schema missing final customer invariant %q", want)
		}
	}
}

func TestConsolidatedSchemaUsesMerchantOwnerOrg(t *testing.T) {
	c := loadSchema001(t)

	// owner_org_id was renamed to permission_group_id in migration 023 (#567).
	// The consolidated baseline reflects the final schema: permission_group_id.
	for _, want := range []string{
		"permission_group_id text",
		"idx_merchants_permission_group_id",
		"COMMENT ON COLUMN openrails.merchants.permission_group_id",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("001 schema missing merchant permission group invariant %q", want)
		}
	}
	for _, forbidden := range []string{
		"owner_tenant_id",
		"idx_merchants_owner_tenant_id",
		// owner_org_id as a column definition (not as a historical reference in comments)
		"owner_org_id text",
		"UNIQUE INDEX idx_merchants_owner_org_id",
		"UNIQUE (owner_org_id)",
	} {
		if strings.Contains(c, forbidden) {
			t.Errorf("001 schema must not keep legacy owner artifact %q", forbidden)
		}
	}
}
