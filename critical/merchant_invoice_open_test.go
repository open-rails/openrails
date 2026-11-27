//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

// openPurchase is alice's unpaid purchase of quantity of price: a checkout,
// the one kind of invoice that expires, a day after it is made.
func openPurchase(t *testing.T, a *authorityWorld, price billing.PriceID, quantity int) billing.Invoice {
	t.Helper()
	status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price, Quantity: ptr(quantity)}}})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var invoice billing.Invoice
	require.NoError(t, json.Unmarshal(raw, &invoice))
	require.Equal(t, billing.InvoiceOriginCustomer, invoice.Origin)
	require.NotNil(t, invoice.ExpiresAt)
	return invoice
}

// billFor is the merchant's bill to alice for price.
func billFor(t *testing.T, a *authorityWorld, key string, price billing.PriceID) *billing.Invoice {
	t.Helper()
	bill, err := a.alphaRemote.CreateInvoice(t.Context(), key, billing.CreateInvoiceParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Lines: []billing.InvoiceLineParams{{PriceID: price}}})
	require.NoError(t, err)
	require.Equal(t, billing.InvoiceOriginMerchant, bill.Origin)
	return bill
}

// myInvoiceList is alice's own list of invoices, filtered by query.
func myInvoiceList(t *testing.T, a *authorityWorld, query string) []billing.InvoiceID {
	t.Helper()
	status, raw := a.request(t, http.MethodGet, "/v1/me/invoices"+query, a.aliceToken, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var page billing.ListPage[billing.Invoice]
	require.NoError(t, json.Unmarshal(raw, &page))
	ids := make([]billing.InvoiceID, len(page.Items))
	for i, invoice := range page.Items {
		ids[i] = invoice.ID
	}
	return ids
}

// pastBothLimits is well past a merchant invoice's old 30-day default and its
// 120-day ceiling.
const pastBothLimits = 121 * 24 * time.Hour

func TestMerchantInvoicesStayOpen(t *testing.T) {
	t.Run("OR-J08-S72/a-merchant-invoice-is-open-and-payable-well-past-30-days", func(t *testing.T) {
		// A payment is never recorded in the database's future.
		a, p := newPaymentWorld(t, func(w *world) { w.clock.Advance(-pastBothLimits - 24*time.Hour) })
		fleet := quietFleet(t, a.world)
		_, price := publicSale(t, a, "open-bill", 9_990_000, nil)
		bill := billFor(t, a, "open-bill", price.ID)
		require.Nil(t, bill.ExpiresAt, "a bill has no expiry")
		advanceTo(a.world, a.clock.Now().Add(pastBothLimits))
		require.False(t, runPass(t, fleet))
		open, err := a.alphaRemote.GetInvoice(t.Context(), bill.ID)
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceOpen, open.Status)
		require.Nil(t, open.ExpiresAt)
		status, raw := a.request(t, http.MethodPost, "/v1/me/invoices/"+bill.ID.String()+"/pay", a.aliceToken,
			billing.PayInvoiceParams{ExpectedTotal: 9_990_000, Payment: billing.InvoicePaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}},
			map[string]string{"Idempotency-Key": "late-bill"})
		require.Equal(t, http.StatusOK, status, string(raw))
		paid, err := a.alphaRemote.GetInvoice(t.Context(), bill.ID)
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceComplete, paid.Status)
		require.NotNil(t, paid.Payment.PaymentID)
	})
	t.Run("OR-J08-S73/a-merchant-cancels-its-invoice-well-past-30-days", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		fleet := quietFleet(t, a.world)
		_, price := publicSale(t, a, "canceled-bill", 9_990_000, nil)
		bill := billFor(t, a, "canceled-bill", price.ID)
		advanceTo(a.world, a.clock.Now().Add(pastBothLimits))
		require.False(t, runPass(t, fleet))
		canceled, err := a.alphaRemote.CancelInvoice(t.Context(), bill.ID)
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceCanceled, canceled.Status)
		require.NotNil(t, canceled.CanceledAt)
		require.Nil(t, canceled.ExpiredAt)
		require.Empty(t, p.effects(t))
	})
	t.Run("OR-J08-S74/the-customer-list-omits-an-expired-purchase-the-merchant-list-keeps-it", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		fleet := quietFleet(t, a.world)
		_, price := publicSale(t, a, "lapsed-checkout", 9_990_000, nil)
		purchase := openPurchase(t, a, price.ID, 1)
		bill := billFor(t, a, "kept-bill", price.ID)
		require.ElementsMatch(t, []billing.InvoiceID{purchase.ID, bill.ID}, myInvoiceList(t, a, ""))
		// Read expired before any pass writes it, then after.
		advanceTo(a.world, purchase.ExpiresAt.Add(time.Minute))
		require.Equal(t, []billing.InvoiceID{bill.ID}, myInvoiceList(t, a, ""), "a lapsed checkout leaves the list at once")
		require.False(t, runPass(t, fleet))
		require.Equal(t, []billing.InvoiceID{bill.ID}, myInvoiceList(t, a, ""), "the bill stays; the expired purchase is not one")
		require.Empty(t, myInvoiceList(t, a, "?status=expired"))
		status, raw := a.request(t, http.MethodGet, "/v1/me/invoices/"+purchase.ID.String(), a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var own billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &own))
		require.Equal(t, billing.InvoiceExpired, own.Status, "read by its id it is still the customer's")
		staff, err := a.alphaRemote.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: purchase.CustomerID})
		require.NoError(t, err)
		statuses := map[billing.InvoiceID]billing.InvoiceStatus{}
		for _, invoice := range staff.Items {
			statuses[invoice.ID] = invoice.Status
		}
		require.Equal(t, map[billing.InvoiceID]billing.InvoiceStatus{purchase.ID: billing.InvoiceExpired, bill.ID: billing.InvoiceOpen}, statuses, "the merchant's list keeps it")
	})
}
