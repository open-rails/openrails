package nmi

import "testing"

func TestSaleReversed(t *testing.T) {
	t.Parallel()
	sale := TransactionAction{Type: "sale", Success: "1", Amount: "9.99"}
	for _, c := range []struct {
		name      string
		condition string
		actions   []TransactionAction
		want      bool
	}{
		{"settled sale", "complete", []TransactionAction{sale}, false},
		{"canceled", "canceled", []TransactionAction{sale}, true},
		{"approved void", "pendingsettlement", []TransactionAction{sale, {Type: "void", Success: "1"}}, true},
		{"refused void", "pendingsettlement", []TransactionAction{sale, {Type: "void", Success: "0"}}, false},
		{"full refund", "complete", []TransactionAction{sale, {Type: "refund", Success: "1", Amount: "5.00"}, {Type: "refund", Success: "1", Amount: "4.99"}}, true},
		{"partial refund", "complete", []TransactionAction{sale, {Type: "refund", Success: "1", Amount: "5.00"}}, false},
		{"declined sale", "failed", []TransactionAction{{Type: "sale", Success: "0", Amount: "9.99"}}, false},
	} {
		if got := SaleReversed(c.condition, c.actions); got != c.want {
			t.Errorf("%s: SaleReversed = %v, want %v", c.name, got, c.want)
		}
	}
}
