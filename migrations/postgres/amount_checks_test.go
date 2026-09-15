package postgresmigrations

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// TestAmountValueChecks asserts that the consolidated baseline contains the
// non-negative CHECK constraint on prices.amount and explicitly does NOT
// contain CHECKs on payments.amount or payments.list_amount (those columns
// legitimately hold negative values for refund rows — see reconciliation
// comments and payments schema).
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

	assertNoPaymentsAmountCheck(t)
}

// assertNoPaymentsAmountCheck is MONEY-9's real enforcement (or#865).
//
// It used to match two literal constraint names in the baseline only, so the
// SAME constraint added in a LATER migration, or added without an explicit name
// (Postgres then generates payments_amount_check), passed silently. Both holes
// are the likely ways this actually breaks — nobody re-edits a squashed baseline
// to add a CHECK; they write migration 007.
//
// Now: every migration file, matched on the PREDICATE rather than the name.
func assertNoPaymentsAmountCheck(t *testing.T) {
	t.Helper()
	all, err := fs.Glob(FS, "*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no migrations found in the embedded FS: this guard would pass vacuously")
	}

	// Any CHECK predicate that bounds payments.amount / payments.list_amount
	// below, in any paren form, named or not.
	cols := []string{"amount", "list_amount"}
	ops := []string{">= 0", "> -1", ">= 0::bigint", ">= (0)"}

	for _, name := range all {
		raw, rerr := fs.ReadFile(FS, name)
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		sql := collapseWS(string(raw))
		// Only inspect statements that touch the payments table at all.
		if !strings.Contains(sql, "payments") {
			continue
		}
		for _, col := range cols {
			// Named form, whatever the suffix (…_nonneg_chk, …_check, …_chk).
			// Anchored so invoice_payments_amount_positive_chk — a DIFFERENT
			// table, legitimately positive-only — is not a false positive.
			named := regexp.MustCompile(`(^|[^a-z_])payments_` + col + `_[a-z_]*(chk|check)\b`)
			if loc := named.FindStringIndex(sql); loc != nil {
				t.Errorf("%s: names a CHECK constraint on payments.%s (%q). Refund rows store NEGATIVE "+
					"amounts on that column — a non-negative CHECK would reject every refund. MONEY-9.",
					name, col, sql[loc[0]:min(loc[1]+24, len(sql))])
			}
			// Unnamed / inline form: check (amount >= 0) inside the payments DDL.
			for _, op := range ops {
				for _, form := range []string{
					"check (" + col + " " + op + ")",
					"check ((" + col + " " + op + "))",
					"check (payments." + col + " " + op + ")",
				} {
					if strings.Contains(sql, form) && paymentsTableSection(sql, form) {
						t.Errorf("%s: %q applies to payments.%s. Refund rows store NEGATIVE amounts there. MONEY-9.",
							name, form, col)
					}
				}
			}
		}
	}
}

// paymentsTableSection reports whether the nearest preceding "table" keyword
// before pred belongs to openrails.payments — enough to tell a CHECK inside the
// payments DDL from an identically worded one on prices/invoices.
func paymentsTableSection(sql, pred string) bool {
	i := strings.Index(sql, pred)
	if i < 0 {
		return false
	}
	head := sql[:i]
	j := strings.LastIndex(head, "table ")
	if j < 0 {
		return false
	}
	decl := head[j:min(j+48, len(head))]
	return strings.Contains(decl, "payments") && !strings.Contains(decl, "invoice_payments")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
