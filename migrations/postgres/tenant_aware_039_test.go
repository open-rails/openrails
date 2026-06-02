package postgresmigrations

import (
	"strings"
	"testing"

	"github.com/open-rails/migratekit"
)

// Tenant-owned tables that migration 039 must add a tenant_id column to
// (issue #223). Kept in sync with the FOREACH list inside the migration.
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
	"user_credit_balances",
	"checkout_sessions",
	"manual_rebill_attempts",
}

func loadMigration039(t *testing.T) string {
	t.Helper()
	migrations, err := migratekit.LoadFromFS(FS)
	if err != nil {
		t.Fatalf("LoadFromFS: %v", err)
	}
	for _, m := range migrations {
		if strings.HasPrefix(m.Name, "039_") {
			return m.Content
		}
	}
	t.Fatal("migration 039_* not found among loaded up migrations")
	return ""
}

func TestMigration039_Loads(t *testing.T) {
	// The migration must be picked up by the same loader the app uses.
	_ = loadMigration039(t)
}

func TestMigration039_CreatesTenantsTableAndSeedsDefault(t *testing.T) {
	c := loadMigration039(t)

	if !strings.Contains(c, "CREATE TABLE IF NOT EXISTS billing.tenants") {
		t.Error("migration 039 must create billing.tenants")
	}
	// Seeds exactly one default tenant with the well-known deterministic id.
	if !strings.Contains(c, "00000000-0000-0000-0000-000000000001") {
		t.Error("migration 039 must seed the well-known default tenant id")
	}
	if !strings.Contains(c, "'default'") {
		t.Error("migration 039 must seed the 'default' tenant slug")
	}
	if !strings.Contains(c, "ON CONFLICT (slug) DO NOTHING") {
		t.Error("default tenant seed must be idempotent (ON CONFLICT DO NOTHING)")
	}
}

func TestMigration039_IsAdditiveAndBackwardCompatible(t *testing.T) {
	c := loadMigration039(t)

	// Adds tenant_id as a NULLABLE column and backfills it — never NOT NULL yet.
	if !strings.Contains(c, "ADD COLUMN IF NOT EXISTS tenant_id UUID") {
		t.Error("migration 039 must add a nullable tenant_id UUID column")
	}
	if strings.Contains(c, "SET NOT NULL") {
		t.Error("migration 039 must NOT enforce NOT NULL on tenant_id yet (additive rollout)")
	}
	// Must not drop or rewrite existing constraints/indexes in a breaking way.
	if strings.Contains(strings.ToUpper(c), "DROP INDEX") {
		t.Error("migration 039 (up) must not drop existing indexes")
	}
	if strings.Contains(strings.ToUpper(c), "DROP CONSTRAINT") {
		t.Error("migration 039 (up) must not drop existing constraints")
	}
	// Backfills existing rows to the default tenant.
	if !strings.Contains(c, "SET tenant_id = ") || !strings.Contains(c, "WHERE tenant_id IS NULL") {
		t.Error("migration 039 must backfill existing rows to the default tenant")
	}
}

func TestMigration039_CoversAllTenantOwnedTables(t *testing.T) {
	c := loadMigration039(t)
	for _, tbl := range tenantOwnedTables {
		if !strings.Contains(c, "'"+tbl+"'") {
			t.Errorf("migration 039 must list tenant-owned table %q for tenant_id", tbl)
		}
	}
}
