//go:build integration

package critical_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

func TestRefunds(t *testing.T) {
	t.Run("OR-J20-S01/partial-then-final-refund-conserves-settled-value", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		input := saleInput(t, a, p)
		status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, input, map[string]string{"Idempotency-Key": "refund-purchase"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var invoice billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &invoice))
		require.NotNil(t, invoice.Payment.PaymentID)
		paymentID := *invoice.Payment.PaymentID
		first, err := a.alpha.RefundPayment(t.Context(), "partial-refund", paymentID, billing.RefundPaymentParams{Amount: 10_000_000, Reason: "Partial customer refund"})
		require.NoError(t, err)
		require.Equal(t, billing.PaymentSucceeded, first.Status)
		require.Equal(t, billing.PaymentRefund, first.Kind)
		require.EqualValues(t, -10_000_000, first.Amount)
		require.Equal(t, paymentID, *first.RefundedPaymentID)
		require.NotNil(t, first.Card, "a refund names the card it returns money to")
		require.Equal(t, "visa", *first.Card.Brand)
		require.Equal(t, "4242", *first.Card.Last4)
		replay, err := a.alpha.RefundPayment(t.Context(), "partial-refund", paymentID, billing.RefundPaymentParams{Amount: 10_000_000, Reason: "Partial customer refund"})
		require.NoError(t, err)
		require.Equal(t, first.ID, replay.ID)
		// A charge whose card Stripe has not named yet: its refund learns the
		// card with the charge.
		paid, err := a.alpha.GetPayment(t.Context(), paymentID)
		require.NoError(t, err)
		_, err = a.pool.Exec(t.Context(), "UPDATE "+pgx.Identifier{a.schema, "payments"}.Sanitize()+" SET card_brand = NULL, card_last4 = NULL WHERE merchant_id = $1 AND id = $2", a.alpha.MerchantID().UUID(), paymentID.UUID())
		require.NoError(t, err)
		final, err := a.alpha.RefundPayment(t.Context(), "final-refund", paymentID, billing.RefundPaymentParams{Full: true, Reason: "Refund remaining balance"})
		require.NoError(t, err)
		require.Equal(t, billing.PaymentSucceeded, final.Status)
		require.EqualValues(t, -20_000_000, final.Amount)
		require.Nil(t, final.Card)
		database, err := db.NewWithPGXPool(slog.Default(), a.pool, a.schema)
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
		_, err = a.alpha.RefundPayment(t.Context(), "excess-refund", paymentID, billing.RefundPaymentParams{Amount: 1_000_000, Reason: "Cannot exceed settled value"})
		require.Error(t, err)
		require.Len(t, p.effects(t), 3, "over-refund creates no provider economic effect")
	})
	t.Run("OR-J20-S06/a-won-dispute-gives-back-what-its-chargeback-took", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, saleInput(t, a, p), map[string]string{"Idempotency-Key": "disputed-purchase"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var invoice billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &invoice))
		require.NotNil(t, invoice.Payment.PaymentID)
		paymentID := *invoice.Payment.PaymentID
		paid, err := a.alpha.GetPayment(t.Context(), paymentID)
		require.NoError(t, err)
		var charge, intent string
		for _, effect := range p.effects(t) {
			if effect.Kind == "charge" && (effect.ID == paid.TransactionID || effect.Operation == paid.TransactionID) {
				charge, intent = effect.ID, effect.Operation
			}
		}
		require.NotEmpty(t, charge)
		notice := func(id, kind string, object stripemock.Object) {
			t.Helper()
			body := stripeNotice(t, id, kind, object)
			status, raw := callback(t, a, "acct_e2e", body, stripeSignature(body, "whsec_critical_fixture"))
			require.Equal(t, http.StatusOK, status, raw)
		}
		// The charge as its detail and the merchant's list both read it.
		reads := func(status billing.PaymentStatus, refunded int64) {
			t.Helper()
			read, err := a.alpha.GetPayment(t.Context(), paymentID)
			require.NoError(t, err)
			require.Equal(t, status, read.Status)
			require.Equal(t, refunded, read.AmountRefunded)
			page, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: paid.CustomerID})
			require.NoError(t, err)
			found := false
			for _, listed := range page.Items {
				if listed.ID == paymentID {
					found = true
					require.Equal(t, status, listed.Status)
					require.Equal(t, refunded, listed.AmountRefunded)
				}
			}
			require.True(t, found, "the charge is listed")
			for _, filter := range []billing.PaymentStatus{billing.PaymentSucceeded, billing.PaymentPartiallyRefunded, billing.PaymentRefunded} {
				page, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: paid.CustomerID, Status: filter})
				require.NoError(t, err)
				listed := false
				for _, item := range page.Items {
					listed = listed || item.ID == paymentID
				}
				require.Equal(t, filter == status, listed, "?status=%s", filter)
			}
		}
		dispute := stripemock.Object{"id": "dp_won", "object": "dispute", "amount": 3000, "currency": "usd", "charge": charge, "payment_intent": intent, "status": "needs_response", "reason": "fraudulent"}
		notice("evt_dispute_opened", "charge.dispute.created", dispute)
		reads(billing.PaymentRefunded, 30_000_000)
		dispute["status"] = "won"
		notice("evt_dispute_won", "charge.dispute.closed", dispute)
		reads(billing.PaymentSucceeded, 0)
		// A later partial refund is all that is refunded.
		_, err = a.alpha.RefundPayment(t.Context(), "after-won-dispute", paymentID, billing.RefundPaymentParams{Amount: 10_000_000, Reason: "Partial refund after the dispute"})
		require.NoError(t, err)
		reads(billing.PaymentPartiallyRefunded, 10_000_000)
		read, err := a.alpha.GetPayment(t.Context(), paymentID)
		require.NoError(t, err)
		require.Len(t, read.Refunds, 3, "the chargeback, its recovery and the refund")
	})
}
