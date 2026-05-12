package repo

import "testing"

func TestEffectiveRefundTotalFromLinkedRows(t *testing.T) {
	tests := []struct {
		name    string
		refunds []refundTotalRow
		want    int64
	}{
		{name: "no linked payments", want: 0},
		{name: "refund only", refunds: []refundTotalRow{{Amount: -500, Status: "completed"}}, want: 500},
		{name: "pending reservation counts", refunds: []refundTotalRow{{Amount: -500, Status: "pending"}}, want: 500},
		{name: "failed reservation ignored", refunds: []refundTotalRow{{Amount: -500, Status: "failed"}}, want: 0},
		{name: "partial recovery", refunds: []refundTotalRow{{Amount: -500}, {Amount: 300}}, want: 200},
		{name: "full recovery", refunds: []refundTotalRow{{Amount: -500}, {Amount: 500}}, want: 0},
		{name: "over recovery clamps to zero", refunds: []refundTotalRow{{Amount: -500}, {Amount: 700}}, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveRefundTotalFromLinkedRows(tt.refunds); got != tt.want {
				t.Fatalf("expected %d, got %d", tt.want, got)
			}
		})
	}
}
