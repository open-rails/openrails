package postgresmigrations

import (
	"io/fs"
	"strings"
	"testing"
)

// TestAmountValueChecks asserts that the consolidated baseline (001) contains
// the non-negative CHECK constraint on prices.amount and explicitly does NOT
// contain CHECKs on payments.amount or payments.list_amount (those columns
// legitimately hold negative values for refund rows — see reconciliation
// comments and payments schema).
//
// Originally validated migration 029; after squashing 001..029 into a single
// baseline the check now targets 0001_schema.up.sql.
//
// This is a static test: it validates the migration file content, not a live
// database. Run `bash scripts/test_integration.sh ./migrations/postgres` to
// also verify apply-time correctness.
func TestAmountValueChecks(t *testing.T) {
	files, err := fs.Glob(FS, "0001_*.sql")
	if err != nil {
		t.Fatalf("glob 001 migration: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("migration 0001_schema.up.sql not found in embedded FS")
	}

	content, err := fs.ReadFile(FS, files[0])
	if err != nil {
		t.Fatalf("read %s: %v", files[0], err)
	}
	sql := collapseWS(string(content))

	// The prices non-negative CHECK must be present in the baseline.
	// pg_dump normalizes CHECK predicates to double-paren form: CHECK ((amount >= 0)).
	if !strings.Contains(sql, "prices_amount_nonneg_chk") {
		t.Errorf("baseline %s: expected constraint prices_amount_nonneg_chk to be present", files[0])
	}
	// Accept both single-paren (hand-written) and double-paren (pg_dump normalized) forms.
	if !strings.Contains(sql, "check (amount >= 0)") && !strings.Contains(sql, "check ((amount >= 0))") {
		t.Errorf("baseline %s: expected predicate 'check (amount >= 0)' on prices.amount", files[0])
	}

	// payments.amount and payments.list_amount must NOT be constrained —
	// refund rows legitimately store negative amounts on those columns.
	if strings.Contains(sql, "payments_amount_nonneg_chk") || strings.Contains(sql, "payments_list_amount_nonneg_chk") {
		t.Errorf("baseline %s: must NOT have a non-negative CHECK on payments.amount or payments.list_amount — refund rows store negative amounts on these columns", files[0])
	}
}
