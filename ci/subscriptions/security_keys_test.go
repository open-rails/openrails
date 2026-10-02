//go:build greenfield && integration

package subscriptions_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// SEC-33: tier-change idempotency keys belong to one customer. Another
// customer who guesses or observes a key cannot pre-claim it and turn the
// victim's upgrade into a conflict; each customer's change runs once under the
// same key, on the embedded and the remote Client.
func TestSecurityTierChangeKeysAreCustomerScoped(t *testing.T) {
	t.Parallel()
	forEachRail(t, func(t *testing.T, rail string) {
		w := newWorld(t)
		group := "g" + uuid.NewString()[:8]
		from := w.tierPrice(group, 1, 1000, 720, false)
		to := w.tierPrice(group, 2, 2000, 720, false)
		const key = "upgrade-1"
		for _, tp := range []topology{embedded, remote} {
			_, sub := w.engineMember(rail, tp, from)
			done, err := w.client[tp].ChangeTier(t.Context(), sub, key, openrails.ChangeTierRequest{PriceID: to.ID})
			require.NoError(t, err, "%s: a key another customer used is still this customer's", tp)
			require.Equal(t, "succeeded", done.Status, "%+v", done)
			require.NotEqual(t, sub, *done.SubscriptionID)
		}
	})
}

// SEC-33: a customer probing another customer's payment-method or
// subscription ids gets the same answer as for an id that does not exist.
func TestSecurityForeignIDsLookMissing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	price := w.membership("content:members", 9_990_000)
	owner := w.newCustomer()
	method := owner.saveCard("nmi", visa)
	sub := owner.subscribe(embedded, "nmi", price.ID, "content:members", method)
	prober := w.newCustomer()
	mine := prober.saveCard("nmi", visa)
	missingMethod := openrails.PaymentMethodID(uuid.New()).String()
	missingSub := openrails.SubscriptionID(uuid.New()).String()

	same := func(what, method, foreign, missing string, body any) {
		t.Helper()
		fs, fb := prober.call(method, foreign, "", body)
		ms, mb := prober.call(method, missing, "", body)
		require.Equal(t, http.StatusNotFound, fs, "%s: %v", what, fb)
		require.Equal(t, ms, fs, what)
		require.Equal(t, errorWithoutRequestID(mb), errorWithoutRequestID(fb), what)
	}
	same("update card", http.MethodPut, "/payment-methods/"+method, "/payment-methods/"+missingMethod, map[string]any{"payment_token": w.nmi.Tokenize(visa)})
	same("delete card", http.MethodDelete, "/payment-methods/"+method, "/payment-methods/"+missingMethod, nil)
	same("select card on a subscription", http.MethodPut, "/subscriptions/"+sub.String()+"/payment-method", "/subscriptions/"+missingSub+"/payment-method", map[string]any{"payment_method_id": mine})
	require.True(t, owner.entitled("content:members"))
}

func errorWithoutRequestID(body map[string]any) any {
	e, ok := body["error"].(map[string]any)
	if !ok {
		return body["error"]
	}
	out := map[string]any{}
	for k, v := range e {
		if k != "request_id" {
			out[k] = v
		}
	}
	return out
}
