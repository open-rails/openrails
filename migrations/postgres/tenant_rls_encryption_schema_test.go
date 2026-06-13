package postgresmigrations

import (
	"strings"
	"testing"
)

func TestConsolidatedSchemaEnablesRLSAndAppRole(t *testing.T) {
	c := loadSchema001(t)
	for _, want := range []string{
		"CREATE ROLE openrails_app NOLOGIN NOBYPASSRLS",
		"GRANT USAGE ON SCHEMA openrails TO openrails_app",
		"ENABLE ROW LEVEL SECURITY",
		"FORCE ROW LEVEL SECURITY",
		"CREATE POLICY tenant_isolation",
		"current_setting('app.tenant_id'::text, true)",
		"WITH CHECK",
		"CREATE TABLE openrails.tenant_deks",
		"wrapped_dek bytea NOT NULL",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("001 schema missing %q", want)
		}
	}
}

func TestConsolidatedSchemaCoversTenantOwnedRLSTables(t *testing.T) {
	c := loadSchema001(t)
	tables := append([]string{}, tenantOwnedTables...)
	tables = append(tables,
		"credit_account_settings",
		"credit_spend_limits",
		"entitlement_features",
		"product_entitlement_features",
		"product_access_grants",
		"usage_events",
		"invoices",
		"tier_policies",
		"payment_blocklist",
		"budget_reservations",
	)
	for _, tbl := range tables {
		if !strings.Contains(c, "CREATE POLICY tenant_isolation ON openrails."+tbl) {
			t.Errorf("001 schema missing RLS policy for %q", tbl)
		}
	}
}
