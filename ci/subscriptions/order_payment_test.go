//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/stripemock"
)

// A card just entered pays an order in one call: payment {token}, the
// single-use output of the PSP's own fields. One provider request charges
// and saves it, as a Stripe PaymentIntent with setup_future_usage and
// confirm does; nothing is vaulted or verified first.

// newCardToken is the browser tokenizing c in rail's own fields.
func (w *world) newCardToken(rail string, c card) string {
	if rail == "stripe" {
		return w.stripe.NewPaymentMethod(stripemock.Card{Brand: c.Brand, Last4: c.Last4, Decline: c.Decline})
	}
	return w.nmi.Tokenize(c)
}

// providerWrites counts what the provider was asked to do: NMI's every
// mutation, Stripe's PaymentIntent and SetupIntent creates.
func (w *world) providerWrites(rail string) (charges, setups int) {
	if rail == "stripe" {
		return len(w.stripe.Submitted("/v1/payment_intents")), len(w.stripe.Submitted("/v1/setup_intents"))
	}
	return len(w.nmi.Calls()), len(w.nmi.CallsTo(http.MethodPost, "/customers", nil))
}

func (c *customer) paymentMethods() []map[string]any {
	c.w.t.Helper()
	var out []map[string]any
	for _, m := range c.must(http.MethodGet, "/payment-methods", "", nil)["data"].([]any) {
		out = append(out, m.(map[string]any))
	}
	return out
}

func TestOrderNewCard(t *testing.T) {
	t.Parallel()
	for _, rail := range []string{"nmi", "stripe"} {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := orderWorld(t)
			life := w.lifetime("orders:new-"+rail, 11_000_000)
			pack := w.lifetime("orders:again-"+rail, 3_000_000)
			c := w.newCustomer()
			token := w.newCardToken(rail, visa)
			charges, setups := w.providerWrites(rail)
			body := map[string]any{"lines": []any{line(life, 0)}, "expected_total": micros(11_000_000),
				"payment": map[string]any{"token": token, "psp_id": w.psp[rail], "billing_details": map[string]any{"name": "New Card", "address": map[string]any{"country": "US"}}}}
			bought := c.order(http.MethodPost, "/orders", "new-"+uuid.NewString(), body)
			require.Equal(t, http.StatusCreated, bought.status, "%v", bought.body)
			require.Equal(t, "complete", bought.body["status"], "%v", bought.body)
			require.Equal(t, "succeeded", paymentOf(bought)["status"])
			require.True(t, c.entitled("orders:new-"+rail))
			after, afterSetups := w.providerWrites(rail)
			require.Equal(t, charges+1, after, "one provider request charged and saved the card")
			require.Equal(t, setups, afterSetups, "nothing was vaulted or set up first")
			if rail == "nmi" {
				sale := w.nmi.Sales()[len(w.nmi.Sales())-1]
				require.NotEmpty(t, sale.Vault, "the sale saved the card")
				require.Equal(t, "customer", sale.InitiatedBy)
				require.Equal(t, "stored", sale.Indicator, "the charge is the storing transaction")
				require.Empty(t, w.nmi.Validations(sale.Vault), "no verification")
			} else {
				pi := w.stripe.Submitted("/v1/payment_intents")
				form := pi[len(pi)-1].Form
				require.Equal(t, token, form.Get("payment_method"))
				require.Equal(t, "off_session", form.Get("setup_future_usage"))
				require.Equal(t, "true", form.Get("confirm"))
				require.NotEmpty(t, w.stripe.CustomerOf(token), "the PaymentIntent attached the card")
			}

			methods := c.paymentMethods()
			require.Len(t, methods, 1, "the card was saved")
			saved := methods[0]
			require.Equal(t, saved["id"], paymentOf(bought)["payment_method_id"])
			require.Equal(t, "4242", saved["card"].(map[string]any)["last4"])
			require.Equal(t, true, saved["reusable"], "outside the EEA and the UK a new card is kept")

			// A one-click buy with the saved card cites what the first charge stored.
			again := c.order(http.MethodPost, "/orders", "again-"+uuid.NewString(), map[string]any{"lines": []any{line(pack, 0)}, "expected_total": micros(3_000_000),
				"payment": map[string]any{"payment_method_id": saved["id"]}})
			require.Equal(t, "complete", again.body["status"], "%v", again.body)
			if rail == "nmi" {
				sales := w.nmi.Sales()
				require.Equal(t, "used", sales[len(sales)-1].Indicator)
				require.Equal(t, sales[len(sales)-2].TransactionID, sales[len(sales)-1].Initial)
			}
		})
	}
}

// A new card the issuer declines is no stored credential: 402 with the
// order, nothing saved, and the key replays the same 402.
func TestOrderNewCardDeclined(t *testing.T) {
	t.Parallel()
	for _, rail := range []string{"nmi", "stripe"} {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := orderWorld(t)
			life := w.lifetime("orders:declined-new-"+rail, 6_000_000)
			c := w.newCustomer()
			refused := card{Brand: "visa", Last4: "0341", Decline: "202"}
			if rail == "stripe" {
				refused.Decline = "insufficient_funds"
			}
			key := "declined-" + uuid.NewString()
			body := map[string]any{"lines": []any{line(life, 0)}, "expected_total": micros(6_000_000), "payment": map[string]any{"token": w.newCardToken(rail, refused), "psp_id": w.psp[rail]}}
			declined := c.order(http.MethodPost, "/orders", key, body)
			require.Equal(t, http.StatusPaymentRequired, declined.status, "%v", declined.body)
			require.Equal(t, "card_declined", orderError(declined))
			require.Equal(t, "insufficient_funds", paymentOf(declined)["last_payment_error"].(map[string]any)["reason"])
			require.Equal(t, "open", orderOf(declined)["status"])
			require.Nil(t, paymentOf(declined)["payment_method_id"], "nothing was saved")
			require.Empty(t, c.paymentMethods())
			if rail == "nmi" {
				require.Empty(t, w.nmi.Vaults(), "no vault outlives a declined card")
			}
			charges, _ := w.providerWrites(rail)
			replay := c.order(http.MethodPost, "/orders", key, body)
			require.Equal(t, http.StatusPaymentRequired, replay.status)
			require.True(t, replay.replayed)
			require.Equal(t, orderOf(declined)["id"], orderOf(replay)["id"])
			after, _ := w.providerWrites(rail)
			require.Equal(t, charges, after, "a replay never charges")
		})
	}
}

// In the EEA and the UK a new card is kept only with the customer's opt-in.
// Without it, a one-time order's card is charged for this purchase only:
// no stored-credential fields, and nothing saved.
func TestOrderNewCardReuseOptIn(t *testing.T) {
	t.Parallel()
	w := orderWorld(t)
	first := w.lifetime("orders:eea-once", 4_000_000)
	second := w.lifetime("orders:eea-kept", 4_000_000)
	c := w.newCustomer()
	german := map[string]any{"name": "Eu Buyer", "address": map[string]any{"country": "DE"}}

	once := c.order(http.MethodPost, "/orders", "eea-"+uuid.NewString(), map[string]any{"lines": []any{line(first, 0)}, "expected_total": micros(4_000_000),
		"payment": map[string]any{"token": w.nmi.Tokenize(visa), "psp_id": w.psp["nmi"], "billing_details": german}})
	require.Equal(t, "complete", once.body["status"], "%v", once.body)
	sale := w.nmi.Sales()[len(w.nmi.Sales())-1]
	require.Empty(t, sale.Vault, "nothing saved")
	require.Empty(t, sale.Indicator, "no stored-credential fields")
	require.Empty(t, sale.InitiatedBy)
	require.Nil(t, paymentOf(once)["payment_method_id"])
	require.Empty(t, c.paymentMethods())

	kept := c.order(http.MethodPost, "/orders", "eea-kept-"+uuid.NewString(), map[string]any{"lines": []any{line(second, 0)}, "expected_total": micros(4_000_000),
		"payment": map[string]any{"token": w.nmi.Tokenize(mastercard), "psp_id": w.psp["nmi"], "billing_details": german}, "reusable": true})
	require.Equal(t, "complete", kept.body["status"], "%v", kept.body)
	methods := c.paymentMethods()
	require.Len(t, methods, 1, "the opt-in keeps the card")
	require.Equal(t, true, methods[0]["reusable"])
	require.Equal(t, "stored", w.nmi.Sales()[len(w.nmi.Sales())-1].Indicator)
}

// A new Stripe card the issuer challenges: the order waits for the
// customer's action, confirm reads Stripe after it, and the card is saved
// once the charge succeeds.
func TestOrderNewCardAuthentication(t *testing.T) {
	t.Parallel()
	w := orderWorld(t)
	member := w.membership("orders:new-3ds", 8_000_000)
	c := w.newCustomer()
	token := w.newCardToken("stripe", card{Brand: "visa", Last4: "3155", Decline: "auth"})
	bought := c.order(http.MethodPost, "/orders", "3ds-"+uuid.NewString(), map[string]any{"lines": []any{line(member, 0)}, "expected_total": micros(8_000_000),
		"payment": map[string]any{"token": token, "psp_id": w.psp["stripe"]}})
	require.Equal(t, http.StatusCreated, bought.status, "%v", bought.body)
	require.Equal(t, "open", bought.body["status"])
	require.Equal(t, "requires_action", paymentOf(bought)["status"])
	next := paymentOf(bought)["next_action"].(map[string]any)
	require.Equal(t, "authenticate", next["type"])
	require.Empty(t, c.paymentMethods(), "saved only once the charge succeeds")
	id := bought.body["id"].(string)

	require.True(t, w.stripe.Authenticate(next["payload"].(map[string]any)["payment_intent_id"].(string)))
	confirmed := c.order(http.MethodPost, "/orders/"+id+"/confirm", "", nil)
	require.Equal(t, http.StatusOK, confirmed.status, "%v", confirmed.body)
	require.Equal(t, "complete", confirmed.body["status"], "%v", confirmed.body)
	require.True(t, c.entitled("orders:new-3ds"))
	methods := c.paymentMethods()
	require.Len(t, methods, 1)
	require.Equal(t, methods[0]["id"], paymentOf(confirmed)["payment_method_id"])
	require.Len(t, methods[0]["subscriptions"], 1, "the membership renews on the new card")
}

// A charge whose answer is lost is processing until a provider read settles
// it: confirm reads NMI, and the order completes without a second charge.
func TestOrderProcessing(t *testing.T) {
	t.Parallel()
	w := orderWorld(t)
	life := w.lifetime("orders:processing", 5_000_000)
	c := w.newCustomer()
	saved := c.saveCard("nmi", visa)
	w.nmi.DropSaleResponses(1)
	w.nmi.QueryUnavailable(true)
	bought := c.order(http.MethodPost, "/orders", "lost-"+uuid.NewString(), map[string]any{"lines": []any{line(life, 0)}, "expected_total": micros(5_000_000),
		"payment": map[string]any{"payment_method_id": saved}})
	require.Equal(t, http.StatusCreated, bought.status, "%v", bought.body)
	require.Equal(t, "processing", bought.body["status"], "%v", bought.body)
	require.Equal(t, "processing", paymentOf(bought)["status"])
	id := bought.body["id"].(string)
	require.Equal(t, "order_not_cancelable", orderError(c.order(http.MethodPost, "/orders/"+id+"/cancel", "", nil)), "the provider has the payment")
	require.Equal(t, "order_payment_in_progress", orderError(c.order(http.MethodPost, "/orders/"+id+"/pay", "pay-"+uuid.NewString(),
		map[string]any{"payment": map[string]any{"payment_method_id": saved}, "expected_total": micros(5_000_000)})))

	still := c.order(http.MethodPost, "/orders/"+id+"/confirm", "", nil)
	require.Equal(t, "processing", still.body["status"], "an unreadable provider settles nothing: %v", still.body)
	w.nmi.QueryUnavailable(false)
	confirmed := c.order(http.MethodPost, "/orders/"+id+"/confirm", "", nil)
	require.Equal(t, "complete", confirmed.body["status"], "%v", confirmed.body)
	require.True(t, c.entitled("orders:processing"))
	require.Len(t, w.nmi.saleOrders(), 1, "the lost answer is read, never resent")
}
