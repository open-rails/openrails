//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/stretchr/testify/require"
)

func TestRefunds(t *testing.T) {
	t.Run("OR-J20-S01/partial-then-final-refund-conserves-settled-value", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		input := saleInput(t, a, p)
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "refund-purchase"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var order billing.Order
		require.NoError(t, json.Unmarshal(raw, &order))
		require.NotNil(t, order.Payment.PaymentID)
		paymentID := *order.Payment.PaymentID
		first, err := a.alpha.RefundPayment(t.Context(), paymentID, billing.RefundPaymentParams{Amount: 10_000_000, Reason: "Partial customer refund", IdempotencyKey: "partial-refund"})
		require.NoError(t, err)
		require.Equal(t, billing.PaymentSucceeded, first.Status)
		require.Equal(t, billing.PaymentRefund, first.Kind)
		require.EqualValues(t, -10_000_000, first.Amount)
		require.Equal(t, paymentID, *first.RefundedPaymentID)
		require.NotNil(t, first.Card, "a refund names the card it returns money to")
		require.Equal(t, "visa", *first.Card.Brand)
		require.Equal(t, "4242", *first.Card.Last4)
		replay, err := a.alpha.RefundPayment(t.Context(), paymentID, billing.RefundPaymentParams{Amount: 10_000_000, Reason: "Partial customer refund", IdempotencyKey: "partial-refund"})
		require.NoError(t, err)
		require.Equal(t, first.ID, replay.ID)
		// A charge whose card Stripe has not named yet: its refund learns the
		// card with the charge.
		paid, err := a.alpha.GetPayment(t.Context(), paymentID)
		require.NoError(t, err)
		_, err = a.pool.Exec(t.Context(), "UPDATE "+pgx.Identifier{a.schema, "payments"}.Sanitize()+" SET card_brand = NULL, card_last4 = NULL WHERE merchant_id = $1 AND id = $2", a.alpha.MerchantID().UUID(), paymentID.UUID())
		require.NoError(t, err)
		final, err := a.alpha.RefundPayment(t.Context(), paymentID, billing.RefundPaymentParams{Full: true, Reason: "Refund remaining balance", IdempotencyKey: "final-refund"})
		require.NoError(t, err)
		require.Equal(t, billing.PaymentSucceeded, final.Status)
		require.EqualValues(t, -20_000_000, final.Amount)
		require.Nil(t, final.Card)
		database, err := db.NewWithPGXPool(a.pool, a.schema)
		require.NoError(t, err)
		ctx := db.WithPSPID(merchant.WithID(t.Context(), a.alpha.MerchantID()), final.PSPID.UUID())
		require.NoError(t, payments.SnapshotPaymentCard(ctx, database, []string{paid.TransactionID}, &models.Card{Brand: "visa", Last4: "4242"}))
		final, err = a.alpha.GetPayment(t.Context(), final.ID)
		require.NoError(t, err)
		require.NotNil(t, final.Card, "the late card snapshot reaches the charge's refunds")
		require.Equal(t, "4242", *final.Card.Last4)
		charge, err := a.alpha.GetPayment(t.Context(), paymentID)
		require.NoError(t, err)
		require.Equal(t, billing.PaymentRefunded, charge.Status)
		require.EqualValues(t, 30_000_000, charge.Amount)
		require.EqualValues(t, 30_000_000, charge.AmountRefunded)
		require.Len(t, charge.Refunds, 2)
		effects := p.effects(t)
		require.Len(t, effects, 3)
		var debits, refunds []int64
		for _, effect := range effects {
			require.Equal(t, "usd", effect.Currency)
			switch effect.Kind {
			case "charge":
				debits = append(debits, effect.Amount)
			case "refund":
				refunds = append(refunds, effect.Amount)
			default:
				t.Fatalf("unexpected economic effect %q", effect.Kind)
			}
		}
		require.Equal(t, []int64{3000}, debits)
		require.ElementsMatch(t, []int64{1000, 2000}, refunds)
		_, err = a.alpha.RefundPayment(t.Context(), paymentID, billing.RefundPaymentParams{Amount: 1_000_000, Reason: "Cannot exceed settled value", IdempotencyKey: "excess-refund"})
		require.Error(t, err)
		require.Len(t, p.effects(t), 3, "over-refund creates no provider economic effect")
	})
}
