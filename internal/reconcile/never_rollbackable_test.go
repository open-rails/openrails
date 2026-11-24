package reconcile

import "testing"

// A merchant unit's books are as immutable as the ledger they explain.
func TestUnitBooksNeverRollBack(t *testing.T) {
	for _, table := range []string{"credit_grant_reversals", "ledger_units"} {
		if _, never := NeverRollbackableTables[table]; !never {
			t.Errorf("a rollback may restore %s", table)
		}
	}
}
