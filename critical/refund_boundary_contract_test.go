//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestRefundBoundaryContract(t *testing.T) {
	t.Run("OR-J20-S04/foreign-account-and-off-rail-refunds-have-no-effect", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		input := saleInput(t, a, p)
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "owned-refund-boundary"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var order billing.Order
		require.NoError(t, json.Unmarshal(raw, &order))
		require.NotNil(t, order.Payment.PaymentID)
		paymentID := *order.Payment.PaymentID
		before, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Limit: 100})
		require.NoError(t, err)
		_, err = a.beta.RefundPayment(t.Context(), paymentID, billing.RefundPaymentParams{Full: true, IdempotencyKey: "foreign-refund"})
		require.ErrorIs(t, err, billing.ErrNotFound)
		charge, err := a.alpha.GetPayment(t.Context(), paymentID)
		require.NoError(t, err)
		require.Equal(t, "USD", charge.Currency)
		require.EqualValues(t, 30_000_000, charge.Amount)
		require.Zero(t, charge.AmountRefunded)
		require.Empty(t, charge.Refunds)
		require.Equal(t, billing.PaymentSucceeded, charge.Status)
		after, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{After: before.NextCursor, Limit: 100})
		require.NoError(t, err)
		require.Empty(t, after.Events)
		require.Len(t, p.effects(t), 1)
		require.EqualValues(t, 3000, p.effects(t)[0].Amount)

		manualOrder, err := a.alpha.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Lines: input.Lines, IdempotencyKey: "off-rail-refund-boundary"})
		require.NoError(t, err)
		manual, err := a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{OrderID: manualOrder.ID, Amount: 30_000_000, TransactionID: "wire-not-card-refund"})
		require.NoError(t, err)
		before, err = a.alpha.ListEvents(t.Context(), billing.EventQuery{Limit: 100})
		require.NoError(t, err)
		_, err = a.alpha.RefundPayment(t.Context(), manual.ID, billing.RefundPaymentParams{Full: true, IdempotencyKey: "unsupported-refund"})
		require.ErrorIs(t, err, billing.ErrRefundUnsupported)
		retained, err := a.alpha.GetPayment(t.Context(), manual.ID)
		require.NoError(t, err)
		require.Equal(t, billing.PaymentSucceeded, retained.Status)
		require.Zero(t, retained.AmountRefunded)
		require.Empty(t, retained.Refunds)
		after, err = a.alpha.ListEvents(t.Context(), billing.EventQuery{After: before.NextCursor, Limit: 100})
		require.NoError(t, err)
		require.Empty(t, after.Events)
		require.Len(t, p.effects(t), 1, "neither refusal can issue an external refund")
	})
}
