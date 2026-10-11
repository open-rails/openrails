//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// attempt is one payment_attempts row.
type attempt struct {
	Kind, Owner, CardEntry, Category          string
	Reason, CVV, TransactionID, Target, Last4 *string
	Checkout, Payment                         *uuid.UUID
}

func (w *world) attempts(customerID string) []attempt {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), `SELECT kind, owner, card_entry, category, reason, cvv_result, transaction_id, checkout_target, card_last4, checkout_id, payment_id
		FROM `+pgx.Identifier{w.schema}.Sanitize()+`.payment_attempts WHERE customer_id = $1 ORDER BY attempted_at, id`, customerID)
	require.NoError(w.t, err)
	defer rows.Close()
	var out []attempt
	for rows.Next() {
		var a attempt
		require.NoError(w.t, rows.Scan(&a.Kind, &a.Owner, &a.CardEntry, &a.Category, &a.Reason, &a.CVV, &a.TransactionID, &a.Target, &a.Last4, &a.Checkout, &a.Payment))
		out = append(out, a)
	}
	require.NoError(w.t, rows.Err())
	return out
}

// nmiRequests is every authorization NMI answered: verifications and sales.
func (w *world) nmiRequests() int { return len(w.nmi.Validations("")) + len(w.nmi.Sales()) }

func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// A buyer mistypes the security code, then fixes it: every answer is one
// attempt, all on the one order that ends paid.
func TestNewCardAttemptsFatFinger(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	price := w.membership("content:members", 9_990_000)
	h := hostedPay{w: w, c: w.newCustomer(), tp: embedded, price: price.ID.String()}

	declined, err := h.pay("pay-cvc", w.nmi.Tokenize(card{Brand: "visa", Last4: "0005", Decline: "200", CVV: "N"}))
	declinedPay(t, declined, err)
	session, err := h.pay("pay-fixed", w.nmi.Tokenize(visa))
	require.NoError(t, err)
	require.Equal(t, "succeeded", session.Status)
	w.settle()

	rows := w.attempts(h.c.id)
	require.Len(t, rows, 2)
	require.Equal(t, w.nmiRequests(), len(rows), "one attempt per NMI answer")
	refused, charged := rows[0], rows[1]
	require.Equal(t, []string{"initial", "card_data", "incorrect_cvc", "N"}, []string{refused.Kind, refused.Category, str(refused.Reason), str(refused.CVV)})
	require.NotEmpty(t, str(refused.TransactionID), "NMI's id for the refused sale")
	require.Equal(t, []string{"initial", "approved"}, []string{charged.Kind, charged.Category})
	for _, a := range rows {
		require.Equal(t, "new", a.CardEntry)
		require.Equal(t, *rows[0].Checkout, *a.Checkout, "one checkout")
	}
	paid := completed(w.payments(embedded, h.c.id))
	require.Len(t, paid, 1)
	require.Equal(t, strings.TrimPrefix(paid[0].ID.String(), "pay_"), charged.Payment.String())
}

// An issuer decline on the sale is recorded with its reason; the next card in
// the same hour continues the checkout.
func TestNewCardAttemptsSaleDeclined(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	price := w.membership("content:members", 9_990_000)
	h := hostedPay{w: w, c: w.newCustomer(), tp: remote, price: price.ID.String()}

	paid, err := h.pay("pay-nsf", w.nmi.Tokenize(card{Brand: "visa", Last4: "0002", Decline: "202"}))
	declinedPay(t, paid, err)
	paid, err = h.pay("pay-ok", w.nmi.Tokenize(visa))
	require.NoError(t, err)
	require.Equal(t, "succeeded", paid.Status)
	w.settle()

	rows := w.attempts(h.c.id)
	require.Len(t, rows, 2)
	require.Equal(t, w.nmiRequests(), len(rows))
	declined := rows[0]
	require.Equal(t, []string{"initial", "issuer_soft", "insufficient_funds", "new"}, []string{declined.Kind, declined.Category, str(declined.Reason), declined.CardEntry})
	require.NotEmpty(t, str(declined.TransactionID))
	require.Nil(t, declined.Payment, "a decline moved no money")
	for _, a := range rows {
		require.Equal(t, *rows[0].Checkout, *a.Checkout)
	}
}

// A card saved outside a purchase, and a one-time purchase on a new card.
func TestNewCardAttemptsCardAddAndSale(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	status, _ := c.call(http.MethodPost, "/payment-methods", "", map[string]any{"psp_id": w.psp["nmi"],
		"token": w.nmi.Tokenize(card{Brand: "visa", Last4: "0005", Decline: "200", CVV: "N"}), "billing_details": map[string]any{"name": "E2E Payer"}})
	require.Equal(t, http.StatusPaymentRequired, status)
	c.saveCard("nmi", visa)
	rows := w.attempts(c.id)
	require.Len(t, rows, 2)
	for _, a := range rows {
		require.Equal(t, []string{"verify", "none", "new", "card_save"}, []string{a.Kind, a.Owner, a.CardEntry, str(a.Target)})
	}
	require.Equal(t, "incorrect_cvc", str(rows[0].Reason))
	require.Equal(t, "approved", rows[1].Category)
	require.Equal(t, *rows[0].Checkout, *rows[1].Checkout)

	product, err := w.client[embedded].CreateProduct(t.Context(), billing.CreateProductParams{Key: "post-" + uuid.NewString()[:8], DisplayName: "Paid post", Entitlements: []string{"content:post"}})
	require.NoError(t, err)
	post, err := w.client[embedded].CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 4_990_000, Currency: "USD"})
	require.NoError(t, err)
	buyer := w.newCustomer()
	h := hostedPay{w: w, c: buyer, tp: embedded, price: post.ID.String()}
	paid, err := h.pay("sale-1", w.nmi.Tokenize(visa))
	require.NoError(t, err)
	require.Equal(t, "succeeded", paid.Status)
	w.settle()
	rows = w.attempts(buyer.id)
	require.Len(t, rows, 1, "a new card's sale saves it: no verification first")
	require.Equal(t, []string{"initial", "approved", "none", "new"}, []string{rows[0].Kind, rows[0].Category, rows[0].Owner, rows[0].CardEntry})
	require.Equal(t, w.nmiRequests(), 3, "two verifications, one refused, and a sale")
}
