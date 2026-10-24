//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func unpaidSale(t *testing.T, a *authorityWorld, p *providerLedger, customer billing.CustomerID, key string) *billing.Order {
	t.Helper()
	input := saleInput(t, a, p)
	order, err := a.alpha.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: customer, Lines: input.Lines, IdempotencyKey: key})
	require.NoError(t, err)
	require.Equal(t, billing.OrderOpen, order.Status)
	require.Empty(t, p.effects(t))
	return order
}

func TestExternalPayments(t *testing.T) {
	t.Run("OR-J21-S01/record-confirmed-external-money-without-provider-charge", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		order := unpaidSale(t, a, p, customer, "external-order")
		payment, err := a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{OrderID: order.ID, Amount: 30_000_000, TransactionID: "bank-receipt-0001"})
		require.NoError(t, err)
		require.Equal(t, billing.ChannelManual, payment.Channel)
		require.Equal(t, billing.PaymentSucceeded, payment.Status)
		require.Nil(t, payment.Rail)
		require.EqualValues(t, 30_000_000, payment.Amount)
		require.Equal(t, customer, payment.CustomerID)
		after, err := a.alpha.GetOrder(t.Context(), order.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderComplete, after.Status)
		require.Equal(t, payment.ID, *after.Payment.PaymentID)
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"sale:owned"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["sale:owned"])
		_, err = a.alpha.RefundPayment(t.Context(), payment.ID, billing.RefundPaymentParams{Full: true, Reason: "Must refund outside the rail", IdempotencyKey: "manual-refund"})
		require.ErrorIs(t, err, billing.ErrRefundUnsupported)
		require.Empty(t, p.effects(t), "recording an external receipt never calls a provider to move money")
	})
	t.Run("OR-J21-S02/external-receipt-replay-is-one-payment", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		order := unpaidSale(t, a, p, customer, "external-replay-order")
		request := billing.RecordExternalPaymentParams{OrderID: order.ID, Amount: 30_000_000, TransactionID: "stable-bank-receipt"}
		first, err := a.alpha.RecordExternalPayment(t.Context(), request)
		require.NoError(t, err)
		replay, err := a.alpha.RecordExternalPayment(t.Context(), request)
		require.NoError(t, err)
		require.Equal(t, first.ID, replay.ID)
		rows, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, rows.Items, 1)
		require.Equal(t, first.ID, rows.Items[0].ID)
		require.Empty(t, p.effects(t))
	})
	t.Run("OR-J21-S03/receipt-cannot-be-relinked-to-another-customer-or-merchant", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		alice := billing.CustomerID(uuid.MustParse(a.alice.ID))
		bob := billing.CustomerID(uuid.MustParse(a.bob.ID))
		firstOrder := unpaidSale(t, a, p, alice, "external-alice")
		first, err := a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{OrderID: firstOrder.ID, Amount: 30_000_000, TransactionID: "owned-bank-receipt"})
		require.NoError(t, err)
		second, err := a.alpha.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: bob, Lines: []billing.OrderLineParams{{PriceID: *firstOrder.Lines[0].PriceID}}, IdempotencyKey: "external-bob"})
		require.NoError(t, err)
		_, err = a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{OrderID: second.ID, Amount: 30_000_000, TransactionID: "owned-bank-receipt"})
		require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
		_, err = a.beta.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{OrderID: firstOrder.ID, Amount: 30_000_000, TransactionID: "owned-bank-receipt"})
		require.ErrorIs(t, err, billing.ErrNotFound)
		retained, err := a.alpha.GetPayment(t.Context(), first.ID)
		require.NoError(t, err)
		require.Equal(t, alice, retained.CustomerID)
		require.Equal(t, firstOrder.ID, *retained.OrderID)
		unpaid, err := a.alpha.GetOrder(t.Context(), second.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderOpen, unpaid.Status)
		other, err := a.beta.ListPayments(t.Context(), billing.PaymentListParams{})
		require.NoError(t, err)
		require.Empty(t, other.Items)
		require.Empty(t, p.effects(t))
	})
}

func TestImportedReceipt(t *testing.T) {
	t.Run("OR-J21-S04/import-preserves-receipt-refund-allowance-and-event-identity", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		input := saleInput(t, a, p)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		providerCustomer := p.core.AddCustomer(a.alice.Email, "Imported payer", nil)
		form := url.Values{"amount": {"3000"}, "currency": {"usd"}, "customer": {providerCustomer}, "payment_method": {input.Payment.Token}, "confirm": {"true"}}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, p.server.URL+"/v1/payment_intents", strings.NewReader(form.Encode()))
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer sk_test_critical")
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Idempotency-Key", "outside-openrails-charge")
		response, err := p.server.Client().Do(request)
		require.NoError(t, err)
		defer response.Body.Close()
		require.Equal(t, http.StatusOK, response.StatusCode)
		var receipt struct {
			ID      string `json:"id"`
			Status  string `json:"status"`
			Charge  string `json:"latest_charge"`
			Created int64  `json:"created"`
		}
		require.NoError(t, json.NewDecoder(response.Body).Decode(&receipt))
		require.Equal(t, "succeeded", receipt.Status)
		require.NotEmpty(t, receipt.Charge)
		effects := p.effects(t)
		require.Len(t, effects, 1)
		require.EqualValues(t, 3000, effects[0].Amount)
		book := billing.DeclaredBilling{AsOf: a.clock.Now(), DefaultPSP: billing.PSPRef{Key: "stripe"}, Customers: []billing.DeclaredCustomer{{Customer: customer}}, Purchases: []billing.DeclaredPurchase{{SourceID: "known-settled-receipt", Customer: customer, Rail: "stripe", TransactionID: receipt.Charge, Currency: "USD", Amount: 30_000_000, PaidAt: time.Unix(receipt.Created, 0).UTC(), Lines: []billing.DeclaredOrderLine{{Price: input.Lines[0].PriceID, Quantity: ptr(1), UnitAmount: 30_000_000}}}}}
		imported, err := a.alpha.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Empty(t, imported.Blocked)
		require.Contains(t, imported.Imported, "known-settled-receipt")
		payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, payments.Items, 1)
		payment := payments.Items[0]
		require.Equal(t, receipt.Charge, payment.TransactionID)
		require.EqualValues(t, 30_000_000, payment.Amount)
		require.EqualValues(t, 0, payment.AmountRefunded)
		before, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment.settled"}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, before.Events, 1)
		replay, err := a.alpha.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Empty(t, replay.Blocked)
		require.Contains(t, replay.Skipped, "known-settled-receipt")
		after, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment.settled"}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, after.Events, 1)
		require.Equal(t, before.Events[0].ID, after.Events[0].ID)
		refund, err := a.alpha.RefundPayment(t.Context(), payment.ID, billing.RefundPaymentParams{Amount: 10_000_000, Reason: "Partial imported receipt refund", IdempotencyKey: "imported-refund"})
		require.NoError(t, err)
		require.Equal(t, billing.PaymentSucceeded, refund.Status)
		require.EqualValues(t, -10_000_000, refund.Amount)
		preserved, err := a.alpha.GetPayment(t.Context(), payment.ID)
		require.NoError(t, err)
		require.EqualValues(t, 30_000_000, preserved.Amount)
		require.EqualValues(t, 10_000_000, preserved.AmountRefunded)
		require.Len(t, p.effects(t), 2, "import/replay made no charge; refund added exactly one economic effect")
	})
}
