//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

func TestStripeCustomerPerAccount(t *testing.T) {
	t.Run("OR-J12-S13/each-stripe-account-resolves-its-own-customer", func(t *testing.T) {
		var second *providerLedger
		a, first := newPaymentWorld(t, func(w *world) { second = armAlternateStripe(t, w) })
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 2)
		pspIDs := map[string]billing.PSPID{}
		for _, psp := range psps.Items {
			pspIDs[psp.Key] = psp.ID
		}
		firstID, secondID := pspIDs["stripe"], pspIDs["alternate"]
		require.NotEqual(t, uuid.Nil, firstID.UUID())
		require.NotEqual(t, uuid.Nil, secondID.UUID())
		mapped := func(psp billing.PSPID) string {
			t.Helper()
			var ref string
			require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT remote_customer_ref FROM "+pgx.Identifier{a.schema, "psp_customers"}.Sanitize()+
				" WHERE merchant_id = $1 AND customer_id = $2 AND psp_id = $3", a.alpha.MerchantID().UUID(), uuid.MustParse(a.alice.ID), psp.UUID()).Scan(&ref))
			return ref
		}
		saveCard := func(p *providerLedger, psp billing.PSPID, last4 string) string {
			t.Helper()
			status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, billing.CreatePaymentMethodParams{PSPID: psp, Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: last4})})
			require.Equal(t, http.StatusCreated, status, string(raw))
			setups := p.core.Submitted("/v1/setup_intents")
			require.NotEmpty(t, setups)
			return setups[len(setups)-1].Form.Get("customer")
		}
		sale := saleInput(t, a, first)
		buy := func(p *providerLedger, psp billing.PSPID, key, last4 string) string {
			t.Helper()
			input := sale
			input.Payment = &billing.OrderPaymentParams{PSPID: &psp, Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: last4})}
			status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": key})
			require.Equal(t, http.StatusCreated, status, string(raw))
			var order billing.Order
			require.NoError(t, json.Unmarshal(raw, &order))
			require.Equal(t, billing.OrderComplete, order.Status)
			charges := p.core.Submitted("/v1/payment_intents")
			require.NotEmpty(t, charges)
			return charges[len(charges)-1].Form.Get("customer")
		}

		// The first account mints the customer's first Stripe customer.
		firstRef := saveCard(first, firstID, "4242")
		require.NotEmpty(t, firstRef)
		require.Equal(t, firstRef, mapped(firstID))
		require.Len(t, first.core.Submitted("/v1/customers"), 1)

		// A new card on the second account gets that account's own customer,
		// not the first account's, which Stripe would refuse there.
		secondRef := buy(second, secondID, "second-account-first-sale", "5556")
		created := second.core.Submitted("/v1/customers")
		require.Len(t, created, 1, "the second account mints its own customer")
		require.Equal(t, a.alice.ID, created[0].Form.Get("metadata[app_user_id]"))
		require.NotEqual(t, firstRef, secondRef)
		require.Equal(t, secondRef, mapped(secondID))
		require.Equal(t, firstRef, mapped(firstID), "the first account's mapping is untouched")

		// Each account keeps reusing its own customer, whichever was used last.
		require.Equal(t, firstRef, saveCard(first, firstID, "4444"))
		require.Equal(t, secondRef, buy(second, secondID, "second-account-second-sale", "1881"))
		require.Equal(t, firstRef, mapped(firstID))
		require.Equal(t, secondRef, mapped(secondID))
		require.Len(t, first.core.Submitted("/v1/customers"), 1)
		require.Len(t, second.core.Submitted("/v1/customers"), 1)

		require.Empty(t, first.effects(t), "card saves charge nothing")
		require.Len(t, second.effects(t), 2)
		for _, effect := range second.effects(t) {
			require.EqualValues(t, 3000, effect.Amount)
		}
		require.Empty(t, first.core.Unexpected())
		require.Empty(t, second.core.Unexpected())
	})
}
