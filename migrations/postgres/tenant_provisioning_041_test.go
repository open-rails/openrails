package postgresmigrations

import (
	"strings"
	"testing"

	"github.com/open-rails/migratekit"
)

func loadMigration041(t *testing.T) string {
	t.Helper()
	migrations, err := migratekit.LoadFromFS(FS)
	if err != nil {
		t.Fatalf("LoadFromFS: %v", err)
	}
	for _, m := range migrations {
		if strings.HasPrefix(m.Name, "041_") {
			return m.Content
		}
	}
	t.Fatal("migration 041_* not found among loaded up migrations")
	return ""
}

func TestMigration041_Loads(t *testing.T) {
	_ = loadMigration041(t)
}

func TestMigration041_AddsProvisioningColumnsAndTables(t *testing.T) {
	c := loadMigration041(t)

	for _, want := range []string{
		"ADD COLUMN IF NOT EXISTS billing_tier",
		"ADD COLUMN IF NOT EXISTS webhook_host",
		"ADD COLUMN IF NOT EXISTS webhook_path",
		"CREATE TABLE IF NOT EXISTS billing.tenant_secrets",
		"CREATE TABLE IF NOT EXISTS billing.tenant_credential_audit",
		"CREATE TABLE IF NOT EXISTS billing.tenant_exports",
		"uq_tenants_webhook_host",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("migration 041 missing %q", want)
		}
	}
}

func TestMigration041_IsAdditiveAndIdempotent(t *testing.T) {
	c := loadMigration041(t)
	// Additive column adds must be guarded so re-running is safe.
	if strings.Contains(c, "ADD COLUMN ") && !strings.Contains(c, "ADD COLUMN IF NOT EXISTS") {
		t.Error("migration 041 must use ADD COLUMN IF NOT EXISTS for additive safety")
	}
	// Must not drop or rewrite existing tenant columns/constraints.
	for _, forbidden := range []string{"DROP TABLE billing.tenants", "DROP COLUMN", "ALTER COLUMN status"} {
		if strings.Contains(c, forbidden) {
			t.Errorf("migration 041 must be additive; found forbidden %q", forbidden)
		}
	}
}
