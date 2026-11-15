//go:build integration

package critical_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestCCBillRefunds(t *testing.T) {
	t.Run("OR-J20-S05/each-partial-refund-of-a-sale-is-booked-once", func(t *testing.T) {
		a := ccbillWebhookWorld(t)
		price := ccbillMemberPrice(t, a.alpha)
		alice := customerOf(a.alice)
		now := a.clock.Now().UTC().Truncate(time.Second)
		const ref, sale = "556677889900112233", "0812233445566778811"
		importLegacyCCBill(t, a.alpha, now)(legacyCCBillMember(alice, price, ref, sale, now.Add(5*24*time.Hour)))
		charge := func() *billing.Payment {
			t.Helper()
			page, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: alice})
			require.NoError(t, err)
			for _, payment := range page.Items {
				if payment.TransactionID == sale {
					full, err := a.alpha.GetPayment(t.Context(), payment.ID)
					require.NoError(t, err)
					return full
				}
			}
			t.Fatalf("no payment of CCBill sale %s", sale)
			return nil
		}
		require.EqualValues(t, 10_000_000, charge().Amount)

		// CCBill may name the refunded sale on every partial refund of it.
		refund := func(amount string, at time.Time) {
			t.Helper()
			status, body := ccbillNotice(t, a, "Refund", map[string]any{"subscriptionId": ref, "transactionId": sale,
				"amount": amount, "currencyCode": "840", "reason": "customer request", "timestamp": at.Format(time.DateTime)})
			require.Equal(t, http.StatusOK, status, body)
		}
		first, second := now.Add(-2*time.Hour), now.Add(-time.Hour)
		refund("3.00", first)
		refund("4.00", second)
		refund("3.00", first)
		refund("4.00", second)
		booked := func() {
			t.Helper()
			paid := charge()
			require.EqualValues(t, 7_000_000, paid.AmountRefunded, "each partial refund is booked once")
			require.Len(t, paid.Refunds, 2)
			require.ElementsMatch(t, []int64{-3_000_000, -4_000_000}, []int64{paid.Refunds[0].Amount, paid.Refunds[1].Amount})
			require.NotEqual(t, paid.Refunds[0].TransactionID, paid.Refunds[1].TransactionID)
		}
		booked()
		unbooked, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "consistency.ledger.unbooked"})
		require.NoError(t, err)
		require.Empty(t, unbooked.Items)

		// More than remains of the sale: nothing is booked and a person is told.
		refund("5.00", now)
		booked()
		unbooked, err = a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "consistency.ledger.unbooked"})
		require.NoError(t, err)
		require.Len(t, unbooked.Items, 1)
		require.Equal(t, charge().ID.String(), unbooked.Items[0].Evidence["original_payment_id"])
	})
}
