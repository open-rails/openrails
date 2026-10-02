//go:build greenfield && integration

package subscriptions_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// #1104: a host whose lookup after a decline failed retries the same key with
// another card. The lookup answers the finished session's status, only its
// status, so the host moves on instead of treating the key as busy.
func TestLookupAnswersAFinishedSessionForOtherDetails(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	client := w.client[embedded]
	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "post-" + uuid.NewString()[:8], DisplayName: "Paid post", EntitlementsSpec: map[string]*int{"content:post": nil}})
	require.NoError(t, err)
	price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 4_990_000, Currency: "USD"})
	require.NoError(t, err)
	c := w.newCustomer()
	request := func(key string, c2 card) openrails.CreateCheckoutSessionRequest {
		return openrails.CreateCheckoutSessionRequest{
			Customer: openrails.CheckoutCustomerIdentity{ID: c.id}, PriceID: price.ID, IdempotencyKey: key, Confirm: true,
			PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: w.psp["nmi"], Rail: "nmi", PaymentToken: w.nmi.Tokenize(c2), NameOnCard: "Lookup Payer", Zip: "10001", Country: "US"},
		}
	}
	declinedCard := card{Brand: "visa", Last4: "0002", Decline: "202"}

	declined := request("checkout:"+uuid.NewString()+":1", declinedCard)
	_, err = client.CreateCheckoutSession(t.Context(), declined)
	requireStatus(t, err, http.StatusPaymentRequired)

	// The retry carries another card under the same key.
	retry := declined
	retry.PaymentOptions.PaymentToken = w.nmi.Tokenize(visa)
	_, err = client.CreateCheckoutSession(t.Context(), retry)
	requireStatus(t, err, http.StatusConflict) // the key belongs to the declined attempt
	found, err := client.LookupCheckoutSession(t.Context(), retry)
	require.NoError(t, err)
	require.Equal(t, "failed", found.Status)
	require.NotEmpty(t, found.ID)
	require.Nil(t, found.Amount, "nothing but the status")
	require.Nil(t, found.Operation)
	require.Nil(t, found.Failure)

	// The same for a succeeded attempt; the matching lookup keeps its details.
	paid := request("checkout:"+uuid.NewString()+":2", visa)
	session, err := client.CreateCheckoutSession(t.Context(), paid)
	require.NoError(t, err)
	require.Equal(t, "succeeded", session.Status)
	full, err := client.LookupCheckoutSession(t.Context(), paid)
	require.NoError(t, err)
	require.NotNil(t, full.Amount)
	other := paid
	other.PaymentOptions.PaymentToken = w.nmi.Tokenize(mastercard)
	found, err = client.LookupCheckoutSession(t.Context(), other)
	require.NoError(t, err)
	require.Equal(t, "succeeded", found.Status)
	require.Equal(t, session.ID, found.ID)
	require.Nil(t, found.Amount)
	require.Len(t, w.nmi.Ledger(""), 1, "one charge in all")
}
