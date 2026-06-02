package postgresmigrations

import (
	"strings"
	"testing"

	"github.com/open-rails/migratekit"
)

func loadMigration050(t *testing.T) string {
	t.Helper()
	migrations, err := migratekit.LoadFromFS(FS)
	if err != nil {
		t.Fatalf("LoadFromFS: %v", err)
	}
	for _, m := range migrations {
		if strings.HasPrefix(m.Name, "050_") {
			return m.Content
		}
	}
	t.Fatal("migration 050_* not found among loaded up migrations")
	return ""
}

func TestMigration050_Loads(t *testing.T) { _ = loadMigration050(t) }

func TestMigration050_EnablesRLSAndAppRole(t *testing.T) {
	c := loadMigration050(t)
	for _, want := range []string{
		// Unprivileged, non-bypass app role.
		"CREATE ROLE openrails_app NOLOGIN NOBYPASSRLS",
		"GRANT USAGE ON SCHEMA billing TO openrails_app",
		// RLS enabled + forced and a tenant policy keyed off the GUC.
		"ENABLE ROW LEVEL SECURITY",
		"FORCE  ROW LEVEL SECURITY",
		"CREATE POLICY tenant_isolation",
		"current_setting('app.tenant_id', true)",
		"WITH CHECK",
		// DEK table for envelope encryption.
		"CREATE TABLE IF NOT EXISTS billing.tenant_deks",
		"wrapped_dek  BYTEA",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("migration 050 missing %q", want)
		}
	}
}

func TestMigration050_CoversAllTenantOwnedTables(t *testing.T) {
	c := loadMigration050(t)
	// The RLS table list MUST match migration 039's tenant-owned tables.
	tables := []string{
		"products", "prices", "catalog_drift_events", "payment_methods",
		"subscriptions", "entitlements", "payments", "admin_grants",
		"notification_queue", "processor_customers", "credit_types",
		"credit_transactions", "credit_blocks", "user_credit_balances",
		"checkout_sessions", "manual_rebill_attempts",
	}
	for _, tbl := range tables {
		if !strings.Contains(c, "'"+tbl+"'") {
			t.Errorf("migration 050 RLS table list missing %q", tbl)
		}
	}
}
