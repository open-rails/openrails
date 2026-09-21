package postgresmigrations

import (
	"strings"
	"testing"
)

func TestConsolidatedSchemaEnablesRLSWithoutManagingRoles(t *testing.T) {
	c := loadSchema001(t)
	for _, forbidden := range []string{"CREATE ROLE", "ALTER ROLE", "TO openrails_app"} {
		if strings.Contains(c, forbidden) {
			t.Errorf("baseline must not manage runtime roles: %s", forbidden)
		}
	}
	for _, want := range []string{
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
		"billing_policies",
		"billing_policy_bindings",
		"payments",
		"customers",
		"merchant_secrets",
		"host_outbox",
		"psps", // renamed from rail_merchant_accounts in 0003: RLS state follows the rename
	} {
		if missing := s.missingRLS(tbl); len(missing) > 0 {
			t.Errorf("schema missing RLS %v for %q", missing, tbl)
		}
	}
}
