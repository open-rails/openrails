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

func TestMigration040_IsAdditiveAndBackwardCompatible(t *testing.T) {
	c := loadMigration040(t)

	// Adds owner_id as a NULLABLE column on the three credit tables.
	if !strings.Contains(c, "ADD COLUMN IF NOT EXISTS owner_id UUID") {
		t.Error("migration 040 must add a nullable owner_id UUID column")
	}
	// Never enforces NOT NULL yet (additive rollout).
	if strings.Contains(c, "SET NOT NULL") {
		t.Error("migration 040 must NOT enforce NOT NULL on owner_id yet")
	}
	// Must not drop or rewrite existing constraints/indexes in the up path.
	if strings.Contains(strings.ToUpper(c), "DROP INDEX") {
		t.Error("migration 040 (up) must not drop existing indexes")
	}
	if strings.Contains(strings.ToUpper(c), "DROP CONSTRAINT") {
		t.Error("migration 040 (up) must not drop existing constraints")
	}
	// Backfills NULL owner_id rows deterministically.
	if !strings.Contains(c, "WHERE tbl.owner_id IS NULL") {
		t.Error("migration 040 must backfill rows whose owner_id is NULL")
	}
}

func TestMigration040_UsesDeterministicPersonalOrgDerivation(t *testing.T) {
	c := loadMigration040(t)
	// The derivation must use the documented namespace + prefix, matching
	// pkg/identity.PersonalOrgID byte-for-byte.
	if !strings.Contains(c, "6f1c9b3e2a445d7c8e109a2b3c4d5e6f") {
		t.Error("migration 040 must use the fixed personal-org namespace")
	}
	if !strings.Contains(c, "openrails:personal-org:") {
		t.Error("migration 040 must use the documented derivation prefix")
	}
	// UUIDv5 version (5) + variant (10xx) bit-forcing must be present.
	if !strings.Contains(c, "| 80") || !strings.Contains(c, "| 128") {
		t.Error("migration 040 must force UUIDv5 version+variant bits")
	}
}

func TestMigration040_AddsOwnerScopedIdempotencyUniques(t *testing.T) {
	c := loadMigration040(t)
	for _, idx := range []string{
		"uniq_credit_hold_idem_owner",
		"uniq_credit_deposit_idem_owner",
		"uniq_credit_withdrawal_idem_owner",
		"uq_user_credit_balances_owner_type",
	} {
		if !strings.Contains(c, idx) {
			t.Errorf("migration 040 must add owner-scoped unique %q alongside the existing user-scoped one", idx)
		}
	}
}
