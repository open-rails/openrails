package postgresmigrations

import (
	"strings"
	"testing"
)

// Tenant-owned tables that the consolidated schema must scope by tenant_id
// (issue #223). Kept in sync with the tenant_isolation policies in 001.
var tenantOwnedTables = []string{
	"products",
	"prices",
	"catalog_drift_events",
	"payment_methods",
	"subscriptions",
	"entitlements",
	"payments",
	"admin_grants",
	"notification_queue",
	"processor_customers",
	"credit_types",
	"credit_transactions",
	"credit_blocks",
	"credit_balances",
	"checkout_sessions",
}

func loadSchema001(t *testing.T) string {
	t.Helper()
	b, err := FS.ReadFile("001_schema.up.sql")
	if err != nil {
		t.Fatalf("read 001_schema.up.sql: %v", err)
	}
	return string(b)
}

// #336: there is no default tenant. The consolidated schema creates the
// tenants table but seeds no rows — tenants (and their credit types) are
// provisioned explicitly by the control plane / bootstrap, never defaulted.
func TestConsolidatedSchemaHasNoDefaultTenant(t *testing.T) {
	schema := loadSchema001(t)

	if !strings.Contains(schema, "CREATE TABLE openrails.tenants") {
		t.Error("001 schema must create openrails.tenants")
	}
	if strings.Contains(schema, "INSERT INTO openrails.tenants") {
		t.Error("001 schema must not seed any tenant row")
	}
	for _, forbidden := range []string{
		"00000000-0000-0000-0000-000000000001", // the old default-tenant uuid
		"'default'",                            // the old default-tenant slug seed
	} {
		if strings.Contains(schema, forbidden) {
			t.Errorf("001 schema must not reference the removed default tenant (%q)", forbidden)
		}
	}
}

func TestConsolidatedSchemaTenantIDColumns(t *testing.T) {
	c := loadSchema001(t)

	if strings.Contains(c, "WHERE tenant_id IS NULL") {
		t.Error("consolidated schema must not contain tenant_id backfill logic")
	}
	for _, tbl := range tenantOwnedTables {
		wantTable := "CREATE TABLE openrails." + tbl
		if !strings.Contains(c, wantTable) {
			t.Errorf("001 schema missing tenant-owned table %q", tbl)
		}
		if !strings.Contains(c, "CREATE POLICY tenant_isolation ON openrails."+tbl) {
			t.Errorf("001 schema missing tenant_isolation policy for %q", tbl)
		}
	}
}

func TestConsolidatedSchemaUsesTenantSubjectUniques(t *testing.T) {
	c := loadSchema001(t)

	for _, forbidden := range []string{
		"uq_payment_methods_tenant_user_vault",
		"uq_subscriptions_tenant_user_product_lifecycle",
		"uq_entitlements_tenant_active",
		"uq_processor_customers_tenant_user_processor",
		" user_id text",
	} {
		if strings.Contains(c, forbidden) {
			t.Errorf("001 schema must not keep legacy user-scoped artifact %q", forbidden)
		}
	}
	for _, want := range []string{
		"uq_payment_methods_tenant_subject_vault",
		"uq_subscriptions_tenant_subject_product_lifecycle",
		"uq_entitlements_tenant_subject_active",
		"uq_payments_tenant_processor_transaction",
		"uq_processor_customers_tenant_subject_processor",
		"entitlements_tenant_subject_no_overlap",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("001 schema missing final tenant-subject invariant %q", want)
		}
	}
}
