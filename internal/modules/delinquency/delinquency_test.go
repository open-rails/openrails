package delinquency

import "testing"

// The default floor is one unit of the debt's currency, whatever its scale.
func TestDefaultAmountFloorIsOneUnit(t *testing.T) {
	for code, want := range map[string]int64{"USD": 1_000_000, "JPY": 10_000, "SOL": 1_000_000_000} {
		if got := DefaultAmountFloor(code); got != want {
			t.Errorf("DefaultAmountFloor(%s) = %d, want %d", code, got, want)
		}
	}
}
