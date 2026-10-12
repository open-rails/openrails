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

func TestPurchaseQuantityMatchesProviderUnit(t *testing.T) {
	for _, tt := range []struct {
		name     string
		purchase *int32
		provider *int
		want     bool
	}{
		{"both implicit", nil, nil, true},
		{"purchase implicit", nil, new(1), true},
		{"provider implicit", new(int32(1)), nil, true},
		{"both explicit", new(int32(2)), new(2), true},
		{"different quantity", nil, new(2), false},
		{"different explicit quantity", new(int32(2)), new(1), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameQuantity(tt.purchase, tt.provider); got != tt.want {
				t.Fatalf("sameQuantity = %v, want %v", got, tt.want)
			}
		})
	}
}
