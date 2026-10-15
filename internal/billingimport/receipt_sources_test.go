package billingimport

import (
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/stretchr/testify/require"
)

func TestDeclaredReceiptSources(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*billing.DeclaredBilling)
		invalid bool
	}{
		{"valid", func(*billing.DeclaredBilling) {}, false},
		{"another subscription", func(b *billing.DeclaredBilling) {
			b.Subscriptions = append(b.Subscriptions, billing.DeclaredSubscription{SourceID: "other", Rail: "nmi", RailSubscriptionID: "other"})
			b.Transactions[1].RailSubscriptionID = "other"
		}, true},
		{"no original link", func(b *billing.DeclaredBilling) { b.Transactions[1].OriginalTransactionID = "" }, true},
		{"original has no amount", func(b *billing.DeclaredBilling) { b.Transactions[0].Amount = 0 }, true},
		{"failed refund", func(b *billing.DeclaredBilling) {
			b.Transactions[1].Success = false
			b.Transactions[1].OriginalTransactionID = ""
		}, false},
		{"synthetic refund", func(b *billing.DeclaredBilling) { b.Transactions[1].TransactionID = "legacy-refund" }, true},
		{"combined identity", func(b *billing.DeclaredBilling) { b.Transactions[1].TransactionID = "100" }, true},
		{"source collision", func(b *billing.DeclaredBilling) { b.Transactions[1].SourceID = "sub" }, true},
		{"ambiguous account", func(b *billing.DeclaredBilling) { b.Subscriptions = append(b.Subscriptions, b.Subscriptions[0]) }, true},
		{"duplicate receipt", func(b *billing.DeclaredBilling) {
			b.Transactions = append(b.Transactions, b.Transactions[0])
			b.Transactions[2].SourceID = "copy"
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			book := billing.DeclaredBilling{Subscriptions: []billing.DeclaredSubscription{{SourceID: "sub", Rail: "nmi", RailSubscriptionID: "s"}}, Transactions: []billing.DeclaredTransaction{
				{SourceID: "charge", RailSubscriptionID: "s", TransactionID: "100", Success: true, Amount: 10_000_000},
				{SourceID: "refund", RailSubscriptionID: "s", TransactionID: "101", OriginalTransactionID: "100", Type: "refund", Success: true, Amount: 1_000_000},
			}}
			tc.mutate(&book)
			err := validateReceiptSources(book, reconcile.NewDeclaredHistory())
			if tc.invalid {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestDeclaredReceiptFieldsRejectPAN(t *testing.T) {
	for _, receipt := range []billing.DeclaredTransaction{{SourceID: "4111111111111111"}, {OriginalTransactionID: "4111111111111111"}} {
		require.Error(t, rejectDeclaredPANs(billing.DeclaredBilling{Transactions: []billing.DeclaredTransaction{receipt}}))
	}
}
