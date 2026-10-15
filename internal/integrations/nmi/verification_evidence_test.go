package nmi

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestQualifyVerification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*TransactionReport)
		code    int
		invalid bool
	}{
		{name: "approved", code: 100},
		{name: "vault omitted", code: 100, change: func(r *TransactionReport) { r.Transactions[0].CustomerVaultID = "" }},
		{name: "not indexed", change: func(r *TransactionReport) { r.Transactions = nil }},
		{name: "duplicate order", invalid: true, change: func(r *TransactionReport) { r.Transactions = append(r.Transactions, r.Transactions[0]) }},
		{name: "wrong order", invalid: true, change: func(r *TransactionReport) { r.Transactions[0].OrderID = "other" }},
		{name: "wrong vault", invalid: true, change: func(r *TransactionReport) { r.Transactions[0].CustomerVaultID = "other" }},
		{name: "missing transaction", invalid: true, change: func(r *TransactionReport) { r.Transactions[0].TransactionID = "" }},
		{name: "sale is not validation", invalid: true, change: func(r *TransactionReport) { r.Transactions[0].Actions[0].ActionType = "sale" }},
		{name: "nonzero", invalid: true, change: func(r *TransactionReport) { r.Transactions[0].Actions[0].Amount = "0.001" }},
		{name: "pending", change: func(r *TransactionReport) { r.Transactions[0].Condition = "pending" }},
		{name: "decline", code: 200, change: func(r *TransactionReport) {
			r.Transactions[0].Condition = "failed"
			r.Transactions[0].Actions[0].Success = "0"
			r.Transactions[0].Actions[0].ResponseCode = "200"
		}},
		{name: "error is not decline", change: func(r *TransactionReport) {
			r.Transactions[0].Actions[0].Success = "0"
			r.Transactions[0].Actions[0].ResponseCode = "300"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := TransactionReport{Transactions: []QueryTransaction{{TransactionID: "v-1", OrderID: "accepted", CustomerVaultID: "card", Condition: "complete", Actions: []QueryAction{{ActionType: "validate", Amount: "0.00", Success: "1", ResponseCode: "100"}}}}}
			if tc.change != nil {
				tc.change(&r)
			}
			got, err := qualifyVerification(r, "accepted", "card")
			if tc.invalid {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.code, got.ResponseCode)
		})
	}
}
