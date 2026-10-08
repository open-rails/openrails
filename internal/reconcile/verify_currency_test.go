package reconcile

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/stretchr/testify/require"
)

func TestNMIVerifierUsesTheReportedCurrencyScale(t *testing.T) {
	sale := nmi.SaleAction{TransactionID: "jpy-sale", Amount: "500.00", Currency: "JPY", Success: true, At: time.Now()}
	transaction, err := remoteSale(sale, "schedule")
	require.NoError(t, err)
	require.Equal(t, int64(500), transaction.AmountCents)
	for _, probe := range []nmi.SaleProbeResult{{Sales: []nmi.SaleAction{sale}}, {SuccessFound: true, SuccessTransactionID: sale.TransactionID, SuccessAmount: sale.Amount, SuccessCurrency: sale.Currency, SuccessAt: sale.At}} {
		transactions, err := probeSaleTransactions(probe, "schedule")
		require.NoError(t, err)
		require.Len(t, transactions, 1)
		require.Equal(t, int64(500), transactions[0].AmountCents)
	}
	sale.Amount = "500.01"
	_, err = remoteSale(sale, "schedule")
	require.Error(t, err, "fractional yen cannot become financial proof")
	_, err = probeSaleTransactions(nmi.SaleProbeResult{Sales: []nmi.SaleAction{sale}}, "schedule")
	require.Error(t, err)
}

func TestNMIUndatedReceiptCannotBecomeCurrentPeriodEvidence(t *testing.T) {
	sale := nmi.SaleAction{TransactionID: "undated", Amount: "9.99", Currency: "USD", Success: true}
	_, err := remoteSale(sale, "schedule")
	require.Error(t, err)
	for _, probe := range []nmi.SaleProbeResult{{Sales: []nmi.SaleAction{sale}}, {SuccessFound: true, SuccessTransactionID: sale.TransactionID, SuccessAmount: sale.Amount, SuccessCurrency: sale.Currency}, {DeclineFound: true, DeclineTransactionID: sale.TransactionID, DeclineAmount: sale.Amount, DeclineCurrency: sale.Currency}} {
		transactions, err := probeSaleTransactions(probe, "schedule")
		require.Error(t, err)
		require.Empty(t, transactions)
	}
	sale.At = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	transaction, err := remoteSale(sale, "schedule")
	require.NoError(t, err)
	require.Equal(t, sale.At, transaction.OccurredAt, "a valid historical action time is never shifted forward")
}
