//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// NMI writes ¥500 as "500.00". Its decimals are read in the charge's own
// currency: a ¥500 refund or chargeback reverses 5_000_000 native units,
// never a hundred times that.
func TestNMIYenReversalsAreReadInYen(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"refund", "chargeback"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			client := w.client[embedded]
			product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "yen-" + uuid.NewString()[:8], DisplayName: "Yen pass", Entitlements: []string{"content:pass"}})
			require.NoError(t, err)
			hours := monthHours
			price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-jpy", UnitAmount: 5_000_000, Currency: "JPY", AccessDurationHours: &hours})
			require.NoError(t, err)
			c := w.newCustomer()
			method := c.saveCard("nmi", card{Brand: "visa", Last4: "5100"})
			c.buyWith("nmi", method, price, billing.OfferFinite, "content:pass")
			paid := completed(w.payments(embedded, c.id))
			require.Len(t, paid, 1)
			require.Equal(t, []any{"JPY", int64(5_000_000)}, []any{paid[0].Currency, paid[0].Amount})

			notice := nmiEvent("chargeback.batch.complete", obj{"count": 1, "chargebacks": []obj{{
				"id": "cb-" + paid[0].ID.String()[:8], "date": w.clock.Now().UTC().Format("2006-01-02"), "customer_name": "E2E Payer",
				"cc_number": "4xxxxxxxxxxx5100", "amount": "500.00", "reason_code": "10", "reason": "Fraud",
			}}})
			if kind == "refund" {
				notice = nmiEvent("transaction.refund.success", obj{"transaction_id": "refund-" + paid[0].ID.String()[:8], "transaction_type": "cc", "condition": "complete",
					"amount": "500.00", "currency": "JPY", "action": obj{"action_type": "refund", "amount": "500.00", "success": "1"},
					"transaction": obj{"transaction_id": paid[0].TransactionID}})
			}
			require.Equal(t, http.StatusOK, w.deliver("nmi", notice))
			for _, view := range []topology{embedded, remote} {
				got, err := w.client[view].GetPayment(t.Context(), paid[0].ID)
				require.NoError(t, err)
				require.EqualValues(t, 5_000_000, got.AmountRefunded, "¥500 reversed exactly once (%s)", view)
			}
		})
	}
}
