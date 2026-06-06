package postgresmigrations

import (
	"strings"
	"testing"

	"github.com/open-rails/migratekit"
)

func loadMigration040(t *testing.T) string {
	t.Helper()
	migrations, err := migratekit.LoadFromFS(FS)
	if err != nil {
		t.Fatalf("LoadFromFS: %v", err)
	}
	for _, m := range migrations {
		if strings.HasPrefix(m.Name, "040_") {
			return m.Content
		}
	}
	t.Fatal("migration 040_* not found among loaded up migrations")
	return ""
}

func TestMigration040_Loads(t *testing.T) {
	_ = loadMigration040(t)
}

func TestMigration040_EnforcesNotNullOwnerID(t *testing.T) {
	c := loadMigration040(t)

	// HARDCUT / GREENFIELD: owner_id is created NOT NULL from the start, supplied
	// by the writer — no stand-in backfill, no later reconciliation.
	if !strings.Contains(c, "ADD COLUMN IF NOT EXISTS owner_id UUID NOT NULL") {
		t.Error("migration 040 must add a NOT NULL owner_id UUID column")
	}
	// No SHA-1 stand-in derivation / personal tenant-subject backfill in a hardcut.
	if strings.Contains(c, "6f1c9b3e2a445d7c8e109a2b3c4d5e6f") {
		t.Error("migration 040 must NOT carry the legacy personal tenant-subject namespace backfill")
	}
	if strings.Contains(c, "openrails:personal tenant-subject:") {
		t.Error("migration 040 must NOT carry the legacy stand-in derivation prefix")
	}
	if strings.Contains(c, "owner_id IS NULL") {
		t.Error("migration 040 must NOT backfill owner_id (greenfield hardcut)")
	}
}

func TestMigration040_ReplacesLegacyUserScopedUniques(t *testing.T) {
	c := loadMigration040(t)

	// The legacy user-scoped uniques must be DROPPED (replaced, not kept).
	for _, legacy := range []string{
		"DROP INDEX IF EXISTS billing.uniq_credit_hold_idem",
		"DROP INDEX IF EXISTS billing.uniq_credit_deposit_idem",
		"DROP INDEX IF EXISTS billing.uniq_credit_withdrawal_idem",
		"DROP CONSTRAINT IF EXISTS user_credit_balances_user_id_credit_type_id_key",
	} {
		if !strings.Contains(c, legacy) {
			t.Errorf("migration 040 must drop legacy user-scoped unique: %q", legacy)
		}
	}
	// And the owner+tenant-scoped replacements must be created.
	for _, idx := range []string{
		"uniq_credit_hold_idem_owner",
		"uniq_credit_deposit_idem_owner",
		"uniq_credit_withdrawal_idem_owner",
		"uq_user_credit_balances_owner_type",
	} {
		if !strings.Contains(c, idx) {
			t.Errorf("migration 040 must create owner+tenant-scoped unique %q", idx)
		}
	}
	// Owner+tenant-scoped uniques must key on (tenant_id, owner_id, credit_type_id, ...).
	if !strings.Contains(c, "(tenant_id, owner_id, credit_type_id, source, source_id)") {
		t.Error("migration 040 idempotency uniques must be scoped by (tenant_id, owner_id, credit_type_id, source, source_id)")
	}
}
