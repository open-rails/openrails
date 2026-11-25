//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

func TestPublicCart(t *testing.T) {
	t.Run("OR-J08-S01/cart-preview-and-consumption-have-exact-totals", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		_, price := publicSale(t, a, "cart-total", 9_990_000, nil)
		status, raw := a.request(t, "PUT", "/v1/me/cart/lines/"+price.ID.String(), a.aliceToken, billing.SetCartLineParams{Quantity: ptr(3)})
		require.Equal(t, http.StatusOK, status, string(raw))
		var cart billing.Cart
		require.NoError(t, json.Unmarshal(raw, &cart))
		require.EqualValues(t, 29_970_000, cart.Total)
		require.Len(t, cart.Lines, 1)
		require.Equal(t, price.ID, cart.Lines[0].PriceID)
		require.Equal(t, 3, *cart.Lines[0].Quantity)
		require.NotNil(t, cart.ExpiresAt)
		require.Equal(t, 30*24*time.Hour, cart.ExpiresAt.Sub(*cart.LastChangedAt))
		status, raw = a.request(t, "POST", "/v1/me/invoices/preview", a.aliceToken, billing.PreviewMyInvoiceParams{Cart: true})
		require.Equal(t, http.StatusOK, status, string(raw))
		var preview billing.InvoicePreview
		require.NoError(t, json.Unmarshal(raw, &preview))
		require.EqualValues(t, 29_970_000, preview.Total)
		status, raw = a.request(t, "POST", "/v1/me/invoices", a.aliceToken, billing.CreateMyInvoiceParams{Cart: true})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var invoice billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &invoice))
		require.Equal(t, billing.InvoiceOpen, invoice.Status)
		require.EqualValues(t, 29_970_000, invoice.Total)
		status, raw = a.request(t, "GET", "/v1/me/cart", a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &cart))
		require.Empty(t, cart.Lines)
		require.Zero(t, cart.Total)
		require.Empty(t, p.effects(t))
	})
	t.Run("OR-J08-S02/application-invoice-never-charges-the-saved-card", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		_, price := publicSale(t, a, "saved-card", 9_990_000, nil)
		paid := publicPurchase(t, a, p, price.ID, 1, 9_990_000, "save-card")
		methods, err := a.alphaRemote.ListPaymentMethods(t.Context(), paid.CustomerID, billing.PaymentMethodListParams{})
		require.NoError(t, err)
		require.Len(t, methods.Items, 1)
		input := billing.CreateInvoiceParams{CustomerID: paid.CustomerID, Lines: []billing.InvoiceLineParams{{PriceID: price.ID, Quantity: ptr(2)}}}
		unpaid, err := a.alphaRemote.CreateInvoice(t.Context(), "unpaid-application", input)
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceOpen, unpaid.Status)
		require.EqualValues(t, 19_980_000, unpaid.Total)
		require.Nil(t, unpaid.Payment.PaymentID)
		replay, err := a.alphaRemote.CreateInvoice(t.Context(), "unpaid-application", input)
		require.NoError(t, err)
		require.Equal(t, unpaid.ID, replay.ID)
		listed, err := a.alphaRemote.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: paid.CustomerID, Status: billing.InvoiceOpen})
		require.NoError(t, err)
		require.Len(t, listed.Items, 1)
		require.Equal(t, unpaid.ID, listed.Items[0].ID)
		_, err = a.alphaRemote.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{InvoiceID: unpaid.ID, Amount: unpaid.Total - 1, TransactionID: "underpayment"})
		var underpayment *billing.StatusError
		require.ErrorAs(t, err, &underpayment)
		require.Equal(t, http.StatusConflict, underpayment.Status)
		require.Equal(t, billing.CodePaymentExceedsDue, underpayment.Code)
		require.Equal(t, "A recorded payment must equal the invoice's total.", underpayment.Message)
		canceled, err := a.alphaRemote.CancelInvoice(t.Context(), unpaid.ID)
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceCanceled, canceled.Status)
		require.Len(t, p.effects(t), 1, "only the explicit original purchase charged")
	})
	t.Run("OR-J08-S03/unavailable-quantity-and-stale-total-cannot-charge", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		_, price := publicSale(t, a, "refused-cart", 9_990_000, nil)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		preview, err := a.alphaRemote.PreviewInvoice(t.Context(), billing.PreviewInvoiceParams{CustomerID: customer, Lines: []billing.InvoiceLineParams{{PriceID: price.ID, Quantity: ptr(-1)}}})
		require.ErrorIs(t, err, billing.ErrInvalid)
		var badQuantity *billing.StatusError
		require.ErrorAs(t, err, &badQuantity)
		require.Equal(t, billing.CodeInvalidParam, badQuantity.Code)
		require.Nil(t, preview)
		preview, err = a.alphaRemote.PreviewInvoice(t.Context(), billing.PreviewInvoiceParams{CustomerID: customer, Lines: []billing.InvoiceLineParams{{PriceID: price.ID, Quantity: ptr(10_001)}}})
		require.NoError(t, err)
		require.Equal(t, "quantity_invalid", preview.Lines[0].Refusal.Code)
		require.Equal(t, "The maximum quantity is 10000.", preview.Lines[0].Refusal.Message)
		input := billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(1_000_000)), Payment: &billing.InvoicePaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
		status, raw := a.request(t, "POST", "/v1/me/invoices", a.aliceToken, input, map[string]string{"Idempotency-Key": "stale-total"})
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), `"code":"invoice_total_changed"`)
		_, err = a.alphaRemote.UpdatePrice(t.Context(), price.ID, billing.UpdatePriceParams{Archived: catalog.Value(true)})
		require.NoError(t, err)
		preview, err = a.alphaRemote.PreviewInvoice(t.Context(), billing.PreviewInvoiceParams{CustomerID: customer, Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}})
		require.NoError(t, err)
		require.Len(t, preview.Lines, 1)
		require.NotNil(t, preview.Lines[0].Refusal)
		require.Equal(t, "unavailable", preview.Lines[0].Refusal.Code)
		invoices, err := a.alphaRemote.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Empty(t, invoices.Items)
		require.Empty(t, p.effects(t))
		_, payable := publicSale(t, a, "pay-expected-total", 9_990_000, nil)
		invoice, err := a.alphaRemote.CreateInvoice(t.Context(), "pay-expected-total", billing.CreateInvoiceParams{CustomerID: customer, Lines: []billing.InvoiceLineParams{{PriceID: payable.ID}}})
		require.NoError(t, err)
		pay := map[string]any{"payment": input.Payment}
		status, raw = a.request(t, "POST", "/v1/me/invoices/"+invoice.ID.String()+"/pay", a.aliceToken, pay, map[string]string{"Idempotency-Key": "pay-missing-total"})
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		require.Contains(t, string(raw), `"code":"invalid_param"`)
		require.Contains(t, string(raw), `"param":"expected_total"`)
		require.Contains(t, string(raw), "expected_total is required with a payment")
		pay["expected_total"] = "0"
		status, raw = a.request(t, "POST", "/v1/me/invoices/"+invoice.ID.String()+"/pay", a.aliceToken, pay, map[string]string{"Idempotency-Key": "pay-zero-total"})
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), `"code":"invoice_total_changed"`)
		require.Empty(t, p.effects(t))
	})
	t.Run("OR-J08-S04/cancel-and-expiry-preserve-settled-invoice", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		fleet := startFleet(t, a.world)
		_, price := publicSale(t, a, "invoice-expiry", 9_990_000, nil)
		paid := publicPurchase(t, a, p, price.ID, 1, 9_990_000, "settled-before-expiry")
		_, err := a.alphaRemote.CancelInvoice(t.Context(), paid.ID)
		require.Error(t, err)
		expires := a.clock.Now().Add(time.Hour)
		pending, err := a.alphaRemote.CreateInvoice(t.Context(), "will-expire", billing.CreateInvoiceParams{CustomerID: paid.CustomerID, Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}, ExpiresAt: &expires})
		require.NoError(t, err)
		a.clock.Advance(time.Hour)
		status, raw := a.request(t, "POST", "/v1/me/invoices/"+pending.ID.String()+"/pay", a.aliceToken, billing.PayInvoiceParams{ExpectedTotal: 9_990_000, Payment: billing.InvoicePaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}, map[string]string{"Idempotency-Key": "expired-payment"})
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), `"code":"invoice_not_payable"`)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		expired, err := a.alphaRemote.GetInvoice(t.Context(), pending.ID)
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceExpired, expired.Status)
		_, err = a.alphaRemote.CancelInvoice(t.Context(), pending.ID)
		require.Error(t, err)
		a.clock.Advance(31 * 24 * time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		kept, err := a.alphaRemote.GetInvoice(t.Context(), paid.ID)
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceComplete, kept.Status)
		require.Equal(t, paid.Payment.PaymentID, kept.Payment.PaymentID)
		require.Len(t, p.effects(t), 1)
	})
}
