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
		"CREATE POLICY merchant_isolation",
		"current_setting('app.merchant_id'::text, true)",
		"WITH CHECK",
		"CREATE TABLE openrails.merchant_deks",
		"wrapped_dek bytea NOT NULL",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("001 schema missing %q", want)
		}
	}
}

// Named sentinels on top of the derived guard in merchant_aware_schema_test.go:
// these tables must always be merchant-isolated, whatever the derivation says.
func TestSchemaCoversTenantOwnedRLSTables(t *testing.T) {
	s := deriveSchemaTables(t, loadAllSchema(t))
	for _, tbl := range []string{
		"usage_events",
		"invoices",
		"payer_spend_limits",
		"payments",
		"customers",
		"merchant_secrets",
		"payment_settlement_events",
		"psps", // renamed from rail_merchant_accounts in 0003: RLS state follows the rename
	} {
		if missing := s.missingRLS(tbl); len(missing) > 0 {
			t.Errorf("schema missing RLS %v for %q", missing, tbl)
		}
	}
}
