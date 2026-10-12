//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/payment"
)

// An accepted purchase can be bound to a provider bill after a reader loaded
// it. Neither the API nor a stale internal read may start another collector.
func TestProviderOrderRefusesGenericCollection(t *testing.T) {
	for _, owner := range []string{observed.OwnerProvider, observed.OwnerNMISchedule} {
		t.Run(owner, func(t *testing.T) {
			w := orderWorld(t)
			c := w.newCustomer()
			method := c.saveCard("nmi", visa)
			methodID, err := billing.ParsePaymentMethodID(method)
			require.NoError(t, err)
			price := w.lifetime("provider-order:"+owner, 7_000_000)
			created := c.order(http.MethodPost, "/orders", "create-"+uuid.NewString(), map[string]any{"lines": []any{line(price, 1)}})
			require.Equal(t, http.StatusCreated, created.status, "%v", created.body)
			id, err := billing.ParseOrderID(created.body["id"].(string))
			require.NoError(t, err)
			rt := engine.Graph(w.rt).Runtime
			ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
			stale, err := rt.Orders.Get(ctx, id.UUID())
			require.NoError(t, err)
			quote, err := rt.Orders.Preview(ctx, c.cid().UUID(), []billing.OrderLineParams{{PriceID: price.ID}})
			require.NoError(t, err)
			frozen := stale.Lines[0]
			quantity := int(*frozen.Quantity)
			bill := observed.Bill{
				PSPID: w.psp["nmi"].UUID(), CustomerID: c.cid().UUID(), ExistingOrderID: &stale.ID,
				Owner: owner, Kind: "purchase", SourceKind: "test_verified_invoice", SourceKey: "invoice-" + uuid.NewString(),
				Currency: stale.Currency, Total: stale.Total, ProviderReference: "provider-invoice-1",
				Lines: []observed.Line{{PriceID: frozen.PriceID, ProductID: frozen.ProductID, Description: frozen.Description,
					Quantity: &quantity, UnitAmount: frozen.UnitAmount, Amount: frozen.Amount}},
			}
			err = rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := observed.Record(ctx, rt.DB.NewWithPgxTx(tx), bill, w.clock.Now())
				return err
			})
			require.NoError(t, err)
			charges, setups := w.providerWrites("nmi")
			read := c.order(http.MethodGet, "/orders/"+id.String(), "", nil)
			require.Equal(t, http.StatusOK, read.status)
			require.Equal(t, owner, read.body["collection_owner"])
			require.Empty(t, read.body["payment_options"])
			require.Nil(t, paymentOf(read)["next_action"])
			references := read.body["provider_bills"].([]any)
			require.Len(t, references, 1)
			require.Equal(t, "provider-invoice-1", references[0].(map[string]any)["reference"])
			for _, action := range []string{"pay", "confirm"} {
				body := map[string]any{}
				if action == "pay" {
					body = map[string]any{"payment": map[string]any{"payment_method_id": method}, "expected_total": micros(stale.Total)}
				}
				refused := c.order(http.MethodPost, "/orders/"+id.String()+"/"+action, "refuse-"+uuid.NewString(), body)
				require.Equal(t, http.StatusConflict, refused.status, "%s: %v", action, refused.body)
				require.Equal(t, "order_not_payable", orderError(refused))
			}
			_, _, err = rt.Orders.RecordRemittance(ctx, orders.Remittance{OrderID: id.UUID(), Amount: stale.Total, TransactionID: "not-a-provider-settlement"})
			require.ErrorIs(t, err, orders.ErrNotPayable, "the locked writer refuses generic remittance")
			_, err = rt.OrderAttemptService.PayOrder(ctx, payment.OrderPayInput{Order: stale, PaymentMethodID: methodID.UUID(),
				Key: "stale-engine-read", User: &payment.UserIdentity{ID: c.id}, Prices: quote.Prices(), Products: quote.Products()})
			require.ErrorIs(t, err, orders.ErrNotPayable, "the row lock must recheck a stale engine-owned read")
			canceled := c.order(http.MethodPost, "/orders/"+id.String()+"/cancel", "", nil)
			require.Equal(t, http.StatusConflict, canceled.status)
			require.Equal(t, "order_not_cancelable", orderError(canceled))
			afterCharges, afterSetups := w.providerWrites("nmi")
			require.Equal(t, charges, afterCharges)
			require.Equal(t, setups, afterSetups)
			require.Empty(t, w.payments(embedded, c.id), "an unpaid observation never invents a payment")
		})
	}
}
