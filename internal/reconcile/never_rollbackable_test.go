package reconcile

import "testing"

// A merchant unit's recorded scale is as immutable as the ledger it explains.
func TestUnitBooksNeverRollBack(t *testing.T) {
	for _, table := range []string{"ledger_units"} {
		if _, never := NeverRollbackableTables[table]; !never {
			t.Errorf("a rollback may restore %s", table)
		}
	}
}
