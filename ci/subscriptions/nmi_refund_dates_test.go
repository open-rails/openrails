//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

// Signed webhook delivery and history imports must retain the same provider facts.
func TestNMIRefundWebhookSharesImportedDates(t *testing.T) {
	for _, order := range []string{"import_first", "webhook_first", "before_original", "concurrent", "query_recovery"} {
		t.Run(order, func(t *testing.T) {
			w := newWorld(t)
			client, buyer := w.client[embedded], w.newCustomer()
			paid := w.clock.Now().Add(-90 * day).UTC().Truncate(time.Second)
			refunded := w.clock.Now().UTC().Truncate(time.Second)
			sale := w.nmi.AddSale(nmimock.Sale{Vault: w.nmi.AddVault(visa), Amount: "4.23", At: paid})
			refundID := w.nmi.Refund(sale.TransactionID, 101)
			w.clock.Advance(2 * time.Hour)
			p := billing.DeclaredHistoricalPayment{SourceID: "historical-sale", Customer: buyer.cid(), Rail: "nmi", TransactionID: sale.TransactionID, Amount: 4_230_000, Currency: "USD", PaidAt: paid}
			book := billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "nmi"}, HistoricalPayments: []billing.DeclaredHistoricalPayment{p}}
			notice := nmiEvent("transaction.refund.success", obj{
				"transaction_id": refundID, "currency": "USD",
				"action":                  obj{"action_type": "refund", "amount": "-1.01", "date": "ignored-unqualified-webhook-date", "success": "1"},
				"transaction":             obj{"transaction_id": sale.TransactionID},
				"merchant_defined_fields": obj{"20": w.bookID()},
			})
			if order == "before_original" {
				require.NotEqual(t, 200, w.deliverNow("nmi", notice), "our refund waits for its original payment")
				require.Zero(t, w.countRows(`SELECT count(*) FROM billing.payments`))
			}
			initial, err := client.ImportBilling(t.Context(), book)
			require.NoError(t, err)
			require.Empty(t, initial.Blocked)
			book.HistoricalPayments[0].Refunds = []billing.DeclaredHistoricalRefund{{TransactionID: refundID, Amount: 1_010_000, RefundedAt: refunded}}
			var result *billing.BillingImportResult
			switch order {
			case "import_first":
				result, err = client.ImportBilling(t.Context(), book)
				require.NoError(t, err)
				require.Equal(t, 200, w.deliverNow("nmi", notice))
			case "query_recovery":
				w.nmi.QueryUnavailable(true)
				require.NotEqual(t, 200, w.deliverNow("nmi", notice))
				require.Equal(t, 1, w.countRows(`SELECT count(*) FROM billing.payments`))
				w.nmi.QueryUnavailable(false)
				require.Equal(t, 200, w.deliverNow("nmi", notice))
				result, err = client.ImportBilling(t.Context(), book)
				require.NoError(t, err)
			case "webhook_first", "before_original":
				require.Equal(t, 200, w.deliverNow("nmi", notice))
				result, err = client.ImportBilling(t.Context(), book)
				require.NoError(t, err)
			case "concurrent":
				held := w.nmi.Hold(func(r *http.Request) bool {
					return r.URL.Path == "/api/query.php" && r.ParseForm() == nil && r.Form.Get("transaction_id") == refundID
				}, nmimock.HoldResponse)
				defer held.Release()
				done := make(chan int, 1)
				go func() { done <- w.deliverNow("nmi", notice) }()
				select {
				case <-held.Arrived():
				case <-time.After(20 * time.Second):
					t.Fatal("webhook did not read the refund report")
				}
				result, err = client.ImportBilling(t.Context(), book)
				require.NoError(t, err)
				held.Release()
				require.Equal(t, 200, <-done)
				w.nmi.ClearIntercepts()
			}
			require.Equal(t, 200, w.deliverNow("nmi", notice), "same delivery retries safely")
			notice["event_id"] = uuid.NewString()
			require.Equal(t, 200, w.deliverNow("nmi", notice), "new event ID for the same refund retries safely")
			require.Equal(t, 2, w.countRows(`SELECT count(*) FROM billing.payments`), "one original and one refund")
			page, err := client.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: buyer.cid(), Kind: billing.PaymentRefund})
			require.NoError(t, err)
			require.Len(t, page.Items, 1)
			require.EqualValues(t, -1_010_000, page.Items[0].Amount)
			for _, table := range []string{"subscriptions", "grants", "provider_intents"} {
				require.Zero(t, w.countRows("SELECT count(*) FROM billing."+table), table)
			}
			require.Empty(t, result.Blocked, "same refund must import regardless of arrival order")
			require.True(t, refunded.Equal(page.Items[0].CreatedAt), "refund history retains provider occurrence time")
		})
	}
}

func TestNMIRefundWebhookChecksPartialReceipts(t *testing.T) {
	w := newWorld(t)
	client, buyer := w.client[embedded], w.newCustomer()
	paid := w.clock.Now().Add(-90 * day).UTC().Truncate(time.Second)
	refunded := w.clock.Now().UTC().Truncate(time.Second)
	sale := w.nmi.AddSale(nmimock.Sale{Vault: w.nmi.AddVault(visa), Amount: "4.23", At: paid})
	refunds := []billing.DeclaredHistoricalRefund{
		{TransactionID: w.nmi.Refund(sale.TransactionID, 101), Amount: 1_010_000, RefundedAt: refunded},
		{TransactionID: w.nmi.Refund(sale.TransactionID, 101), Amount: 1_010_000, RefundedAt: refunded},
	}
	w.clock.Advance(2 * time.Hour)
	book := billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "nmi"}, HistoricalPayments: []billing.DeclaredHistoricalPayment{{
		SourceID: "partial-sale", Customer: buyer.cid(), Rail: "nmi", TransactionID: sale.TransactionID, Amount: 4_230_000, Currency: "USD", PaidAt: paid,
	}}}
	result, err := client.ImportBilling(t.Context(), book)
	require.NoError(t, err)
	require.Empty(t, result.Blocked)
	notice := func(id, amount string) obj {
		return nmiEvent("transaction.refund.success", obj{"transaction_id": id, "currency": "USD",
			"action": obj{"amount": amount}, "transaction": obj{"transaction_id": sale.TransactionID}})
	}
	// A cumulative total cannot be mistaken for the amount of one partial refund.
	require.NotEqual(t, http.StatusOK, w.deliverNow("nmi", notice(refunds[0].TransactionID, "-2.02")))
	require.Equal(t, 1, w.countRows(`SELECT count(*) FROM billing.payments`))
	for _, refund := range refunds {
		require.Equal(t, http.StatusOK, w.deliverNow("nmi", notice(refund.TransactionID, "-1.01")))
	}
	book.HistoricalPayments[0].Refunds = refunds
	result, err = client.ImportBilling(t.Context(), book)
	require.NoError(t, err)
	require.Empty(t, result.Blocked)
	// An already recorded refund does not depend on another successful provider read.
	w.nmi.QueryUnavailable(true)
	for _, refund := range refunds {
		require.Equal(t, http.StatusOK, w.deliverNow("nmi", notice(refund.TransactionID, "-1.01")))
	}
	w.nmi.QueryUnavailable(false)
	require.Equal(t, 3, w.countRows(`SELECT count(*) FROM billing.payments`))
	page, err := client.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: buyer.cid(), TransactionID: sale.TransactionID})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.EqualValues(t, 2_020_000, page.Items[0].AmountRefunded)
	require.True(t, paid.Equal(page.Items[0].CreatedAt))
	page, err = client.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: buyer.cid(), Kind: billing.PaymentRefund})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	for _, refund := range page.Items {
		require.True(t, refunded.Equal(refund.CreatedAt))
	}
}
