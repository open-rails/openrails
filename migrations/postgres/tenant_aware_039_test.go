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

func TestMigration039_EnforcesNotNullTenantID(t *testing.T) {
	c := loadMigration039(t)

	// HARDCUT / GREENFIELD: tenant_id is created NOT NULL from the start (defaulted
	// to the default tenant), with no backfill-of-existing-rows logic.
	if !strings.Contains(c, "ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL") {
		t.Error("migration 039 must add a NOT NULL tenant_id UUID column")
	}
	// No backfill of pre-existing rows in a greenfield hardcut.
	if strings.Contains(c, "WHERE tenant_id IS NULL") {
		t.Error("migration 039 must NOT backfill existing rows (greenfield hardcut)")
	}
}

func TestMigration039_ReplacesLegacyUniquesWithTenantScoped(t *testing.T) {
	c := loadMigration039(t)

	// The legacy non-tenant-scoped uniques must be DROPPED (replaced, not kept
	// alongside).
	for _, legacy := range []string{
		"DROP INDEX IF EXISTS billing.uq_payment_methods_user_vault",
		"DROP INDEX IF EXISTS billing.idx_subscriptions_user_product_lifecycle_owner",
		"DROP INDEX IF EXISTS billing.uniq_entitlements_active",
		"DROP CONSTRAINT IF EXISTS payments_processor_transaction_unique",
		"DROP CONSTRAINT IF EXISTS entitlements_no_overlap",
	} {
		if !strings.Contains(c, legacy) {
			t.Errorf("migration 039 must drop legacy unique: %q", legacy)
		}
	}
	// And the tenant-scoped replacements must be created.
	for _, repl := range []string{
		"uq_payment_methods_tenant_user_vault",
		"uq_subscriptions_tenant_user_product_lifecycle",
		"uq_entitlements_tenant_active",
		"uq_payments_tenant_processor_transaction",
		"uq_processor_customers_tenant_user_processor",
	} {
		if !strings.Contains(c, repl) {
			t.Errorf("migration 039 must create tenant-scoped unique: %q", repl)
		}
	}
	// The tenant-scoped no-overlap exclusion must include tenant_id.
	if !strings.Contains(c, "tenant_id   WITH =") {
		t.Error("migration 039 must scope the entitlements no-overlap exclusion by tenant_id")
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
