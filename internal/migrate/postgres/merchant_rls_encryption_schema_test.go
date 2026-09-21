package postgresmigrations

import (
	"strings"
	"testing"
)

func TestConsolidatedSchemaHasNoRLSOrManagedRoles(t *testing.T) {
	c := loadSchema001(t)
	for _, forbidden := range []string{"CREATE ROLE", "ALTER ROLE", "TO openrails_app", "CREATE POLICY", "ROW LEVEL SECURITY", "assert_cross_merchant_reader"} {
		if strings.Contains(c, forbidden) {
			t.Errorf("baseline must not manage runtime roles: %s", forbidden)
		}
	}
	for _, want := range []string{
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
func TestSchemaCoversTenantOwnedTables(t *testing.T) {
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
		if !s.merchantScoped[tbl] {
			t.Errorf("schema missing merchant scope for %q", tbl)
		}
	}
}
