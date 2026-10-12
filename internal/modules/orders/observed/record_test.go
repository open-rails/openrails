package observed

import "testing"

func TestSnapshotComparisonPreservesIntegerPrecision(t *testing.T) {
	if sameJSON([]byte(`{"amount":9007199254740992}`), []byte(`{"amount":9007199254740993}`)) {
		t.Fatal("different financial facts rounded into the same identity")
	}
	if !sameJSON([]byte(`{"amount":9007199254740993,"currency":"USD"}`), []byte(`{"currency":"USD","amount":9007199254740993}`)) {
		t.Fatal("jsonb object ordering changed a financial identity")
	}
}
