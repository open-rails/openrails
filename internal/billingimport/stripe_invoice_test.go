package billingimport

import (
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestDeclaredStripeInvoiceRequiresCompleteConsistentEvidence(t *testing.T) {
	at := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	base := billing.DeclaredStripeInvoice{ID: "in_1", CustomerReference: "cus_1", Status: "paid", Reason: "subscription_cycle", Currency: "USD", Total: 10_000_000, AmountDue: 10_000_000, AmountPaid: 10_000_000, Lines: []billing.DeclaredStripeInvoiceLine{{ID: "il_1", Description: "Verified", Amount: 10_000_000, PeriodStart: at, PeriodEnd: at.AddDate(0, 1, 0)}}, Payments: []billing.DeclaredStripeInvoicePayment{{ID: "inpay_1", ChargeID: "ch_1", Amount: 10_000_000, PaidAt: at}}}
	book := DeclaredBilling{Subscriptions: []billing.DeclaredSubscription{{Rail: "stripe", RailSubscriptionID: "sub_1", PaymentMethod: &billing.PaymentMethodRef{RailCustomerRef: "cus_1"}}}}
	for _, name := range []string{"valid", "fractional minor", "wrong cash", "wrong customer", "missing allocation", "conflicting snapshot", "oversize"} {
		t.Run(name, func(t *testing.T) {
			invoice := base
			txn := DeclaredTransaction{RailSubscriptionID: "sub_1", TransactionID: "ch_1", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: at, StripeInvoice: &invoice}
			snapshots := map[string][]byte{}
			switch name {
			case "fractional minor":
				invoice.Total++
			case "wrong cash":
				txn.Amount++
			case "wrong customer":
				invoice.CustomerReference = "cus_foreign"
			case "missing allocation":
				txn.TransactionID = "ch_other"
			case "conflicting snapshot":
				_, err := declaredStripeBill(book, txn, snapshots)
				require.NoError(t, err)
				invoice.Total += 10_000
			case "oversize":
				invoice.Lines = make([]billing.DeclaredStripeInvoiceLine, 1001)
			}
			bill, err := declaredStripeBill(book, txn, snapshots)
			if name != "valid" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1000, bill.Total)
			require.Equal(t, "ch_1", bill.Payments[0].ChargeID)
		})
	}
}
