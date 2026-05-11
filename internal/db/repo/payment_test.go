package repo

import "testing"

func TestEffectiveRefundTotalFromLinkedAmounts(t *testing.T) {
	tests := []struct {
		name    string
		amounts []int64
		want    int64
	}{
		{name: "no linked payments", want: 0},
		{name: "refund only", amounts: []int64{-500}, want: 500},
		{name: "partial recovery", amounts: []int64{-500, 300}, want: 200},
		{name: "full recovery", amounts: []int64{-500, 500}, want: 0},
		{name: "over recovery clamps to zero", amounts: []int64{-500, 700}, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveRefundTotalFromLinkedAmounts(tt.amounts); got != tt.want {
				t.Fatalf("expected %d, got %d", tt.want, got)
			}
		})
	}
}
