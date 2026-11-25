//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

func TestCheckout(t *testing.T) {
	t.Run("OR-J09-S01/real-charge-invoice-access-and-committed-event", func(t *testing.T) {
		a, provider := newPaymentWorld(t)
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "download", DisplayName: "Download", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"download:owned"}})
		require.NoError(t, err)
		price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "download", UnitAmount: 9_990_000, Currency: "USD"})
		require.NoError(t, err)
		before, err := a.alpha.ListEvents(t.Context(), billing.EventListParams{Limit: 100})
		require.NoError(t, err)
		token := provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})
		input := billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price.ID, Quantity: ptr(3)}}, ExpectedTotal: ptr(int64(29_970_000)), Reusable: ptr(true), Payment: &billing.InvoicePaymentParams{Token: token}}
		status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, input, map[string]string{"Idempotency-Key": "purchase-once"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var invoice billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &invoice))
		require.Equal(t, billing.InvoiceComplete, invoice.Status)
		require.Equal(t, billing.InvoicePaymentSucceeded, invoice.Payment.Status)
		require.EqualValues(t, 29_970_000, invoice.Total)
		require.Equal(t, catalog.Currency("USD"), invoice.Currency)
		require.NotNil(t, invoice.Payment.PaymentID)
		windows, err := a.alphaRemote.ListProductAccess(t.Context(), billing.ProductAccessListParams{CustomerIDs: []billing.CustomerID{invoice.CustomerID}, ProductIDs: []billing.ProductID{product.ID}})
		require.NoError(t, err)
		require.Len(t, windows.Items, 1)
		require.Equal(t, billing.ProductAccessSourcePurchase, windows.Items[0].SourceType)
		require.Equal(t, invoice.Lines[0].ID.String(), windows.Items[0].SourceID)
		require.Equal(t, invoice.Payment.PaymentID, windows.Items[0].PaymentID)
		money, err := a.alpha.GetPayment(t.Context(), *invoice.Payment.PaymentID)
		require.NoError(t, err)
		require.Equal(t, billing.PaymentSucceeded, money.Status)
		require.EqualValues(t, 29_970_000, money.Amount)
		require.Equal(t, billing.PaymentCharge, money.Kind)
		require.Equal(t, invoice.ID, *money.InvoiceID)
		effects := provider.effects(t)
		require.Len(t, effects, 1)
		require.Equal(t, "charge", effects[0].Kind)
		require.EqualValues(t, 2997, effects[0].Amount)
		require.Equal(t, "usd", effects[0].Currency)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"download:owned", "another:unowned"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["download:owned"])
		require.False(t, access.Entitlements["another:unowned"])
		events, err := a.alpha.ListEvents(t.Context(), billing.EventListParams{After: before.NextCursor, Limit: 100})
		require.NoError(t, err)
		settled, completed := 0, 0
		for _, event := range events.Events {
			require.Equal(t, a.alpha.MerchantID(), event.MerchantID)
			if event.Type == "payment.settled" {
				settled++
				require.Contains(t, string(event.Data), money.ID.String())
			}
			if event.Type == "invoice.completed" {
				completed++
				require.Contains(t, string(event.Data), invoice.ID.String())
			}
		}
		require.Equal(t, 1, settled)
		require.Equal(t, 1, completed)
		// A card kept for this purchase only still names the card it charged.
		once := provider.core.NewPaymentMethod(stripemock.Card{Brand: "mastercard", Last4: "4444"})
		input = billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(9_990_000)), Reusable: ptr(false), Payment: &billing.InvoicePaymentParams{Token: once}}
		status, raw = a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, input, map[string]string{"Idempotency-Key": "purchase-unkept-card"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var unkept billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &unkept))
		require.Equal(t, billing.InvoiceComplete, unkept.Status)
		money, err = a.alpha.GetPayment(t.Context(), *unkept.Payment.PaymentID)
		require.NoError(t, err)
		require.NotNil(t, money.Card, "the payment names the card it charged")
		require.Equal(t, "mastercard", *money.Card.Brand)
		require.Equal(t, "4444", *money.Card.Last4)
		require.Empty(t, provider.core.Unexpected(), "every provider request must be explicitly modeled")
		// So does an NMI card kept for one purchase: its sale's report names it.
		nmi, journal := newNMIWorld(t)
		nmiProduct, err := nmi.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "nmi-download", DisplayName: "NMI download", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"download:owned"}})
		require.NoError(t, err)
		nmiPrice, err := nmi.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: nmiProduct.ID, Key: "nmi-download", UnitAmount: 9_990_000, Currency: "USD"})
		require.NoError(t, err)
		input = billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: nmiPrice.ID}}, ExpectedTotal: ptr(int64(9_990_000)), Reusable: ptr(false),
			Payment: &billing.InvoicePaymentParams{Token: journal.core.Tokenize(nmimock.Card{Brand: "mastercard", Last4: "5454", Exp: "1235"}), BillingDetails: billedTo("US", "10001")}}
		status, raw = nmi.request(t, http.MethodPost, "/v1/me/invoices", nmi.aliceToken, input, map[string]string{"Idempotency-Key": "nmi-unkept-card"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var nmiInvoice billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &nmiInvoice))
		require.Equal(t, billing.InvoiceComplete, nmiInvoice.Status, string(raw))
		money, err = nmi.alpha.GetPayment(t.Context(), *nmiInvoice.Payment.PaymentID)
		require.NoError(t, err)
		require.NotNil(t, money.Card, "the NMI payment names the card it charged")
		require.Equal(t, "mastercard", *money.Card.Brand)
		require.Equal(t, "5454", *money.Card.Last4)
	})
}
