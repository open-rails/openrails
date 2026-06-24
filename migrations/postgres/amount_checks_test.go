package postgresmigrations

import (
	"io/fs"
	"strings"
	"testing"
)

// TestAmountValueChecks asserts that migration 029 adds the non-negative CHECK
// constraint on prices.amount and explicitly does NOT add CHECKs on
// payments.amount or payments.list_amount (those columns legitimately hold
// negative values for refund rows — see reconciliation.sql:314-315 and
// payments.sql:110).
//
// This is a static test: it validates the migration file content, not a live
// database. Run `bash scripts/test_integration.sh ./migrations/postgres` to
// also verify apply-time correctness.
func TestAmountValueChecks(t *testing.T) {
	files, err := fs.Glob(FS, "029_*.sql")
	if err != nil {
		t.Fatalf("glob 029 migration: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("migration 029_amount_value_checks.up.sql not found in embedded FS")
	}

	content, err := fs.ReadFile(FS, files[0])
	if err != nil {
		t.Fatalf("read %s: %v", files[0], err)
	}
	sql := collapseWS(string(content))

	// The prices non-negative CHECK must be present.
	if !strings.Contains(sql, "prices_amount_nonneg_chk") {
		t.Errorf("migration %s: expected constraint prices_amount_nonneg_chk to be present", files[0])
	}
	if !strings.Contains(sql, "check (amount >= 0)") {
		t.Errorf("migration %s: expected predicate 'check (amount >= 0)' on prices.amount", files[0])
	}

	// payments.amount and payments.list_amount must NOT be constrained —
	// refund rows legitimately store negative amounts on those columns.
	if strings.Contains(sql, "payments_amount_nonneg_chk") || strings.Contains(sql, "payments_list_amount_nonneg_chk") {
		t.Errorf("migration %s: must NOT add a non-negative CHECK on payments.amount or payments.list_amount — refund rows store negative amounts on these columns", files[0])
	}
}
