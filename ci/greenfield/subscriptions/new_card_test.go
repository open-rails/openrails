//go:build greenfield && integration

package subscriptions_test

import (
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// A hosted checkout (#1085) relays the customer's one "Subscribe" click with a
// fresh Collect.js token: OpenRails saves the card, quotes the price and
// enrolls it in one merchant call.
type hostedPay struct {
	w     *world
	c     *customer
	tp    topology
	price string
}

func (h hostedPay) pay(key string, payment openrails.CheckoutPaymentOptions) (*openrails.CheckoutSession, error) {
	h.w.t.Helper()
	payment.PSPID, payment.Rail = h.w.psp["nmi"], "nmi"
	if payment.PaymentToken != "" {
		payment.NameOnCard, payment.Zip, payment.Country = "Hosted Payer", "10001", "US"
	}
	return h.w.client[h.tp].CreateCheckoutSession(h.w.t.Context(), openrails.CreateCheckoutSessionRequest{
		Customer: openrails.CheckoutCustomerIdentity{ID: h.c.id}, PriceID: h.price, IdempotencyKey: key,
		PaymentOptions: payment, Confirm: true,
	})
}

func (h hostedPay) methods() []openrails.PaymentMethod {
	h.w.t.Helper()
	page, err := h.w.client[embedded].ListPaymentMethods(h.w.t.Context(), h.c.id, openrails.PageOptions{Limit: 100})
	require.NoError(h.w.t, err)
	return page.Data
}

func (h hostedPay) subscriptions() []openrails.Subscription {
	h.w.t.Helper()
	subs, err := h.w.client[embedded].ListSubscriptions(h.w.t.Context(), openrails.SubscriptionFilter{CustomerID: h.c.id})
	require.NoError(h.w.t, err)
	return subs.Data
}

func (w *world) vaultCount() int {
	return len(w.nmi.Vaults())
}

func TestHostedNewCardSubscription(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			h := hostedPay{w: w, c: w.newCustomer(), tp: tp, price: w.membership("content:members", 9_990_000).ID}
			token := w.nmi.Tokenize(visa)

			session, err := h.pay("pay-1", openrails.CheckoutPaymentOptions{PaymentToken: token})
			require.NoError(t, err)
			require.Equal(t, "succeeded", session.Status)
			require.NotNil(t, session.SubscriptionID)
			w.settle()
			require.True(t, h.c.entitled("content:members"))
			subs := h.subscriptions()
			require.Len(t, subs, 1)
			require.Equal(t, "engine", subs[0].CollectionPolicy)
			require.Equal(t, "active", subs[0].Status)
			require.Equal(t, *session.SubscriptionID, subs[0].ID.String())
			methods := h.methods()
			require.Len(t, methods, 1, "the new card is saved for renewals")
			require.NotNil(t, subs[0].PaymentMethodID)
			require.Equal(t, methods[0].ID, subs[0].PaymentMethodID.String())
			require.Len(t, w.nmi.ledger(""), 1, "one initial charge")

			// A retried or double-submitted pay replays the accepted enrollment.
			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					again, err := h.pay("pay-1", openrails.CheckoutPaymentOptions{PaymentToken: token})
					if err == nil {
						require.Equal(t, session.ID, again.ID)
						require.Equal(t, "succeeded", again.Status)
					} else {
						require.ErrorIs(t, err, openrails.ErrConflict, "a concurrent duplicate is refused, never charged")
					}
				}()
			}
			wg.Wait()
			w.settle()
			require.Len(t, w.nmi.ledger(""), 1, "no second charge")
			require.Len(t, h.methods(), 1, "no second saved card")
			require.Len(t, h.subscriptions(), 1)
			require.Empty(t, w.nmi.Unexpected())
		})
	}
}

// A declined new card leaves no subscription, no saved method and no vault.
func TestHostedNewCardSubscriptionDeclined(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	h := hostedPay{w: w, c: w.newCustomer(), tp: embedded, price: w.membership("content:members", 9_990_000).ID}
	vaults := w.vaultCount()

	_, err := h.pay("pay-declined", openrails.CheckoutPaymentOptions{PaymentToken: w.nmi.Tokenize(card{Brand: "visa", Last4: "0002", Decline: "202"})})
	require.ErrorIs(t, err, openrails.ErrPaymentRefused)
	var status *openrails.StatusError
	require.True(t, errors.As(err, &status))
	require.Equal(t, openrails.CodeCardDeclined, status.Code)
	w.settle()
	require.Empty(t, h.subscriptions())
	require.Empty(t, h.methods(), "the declined card is not kept")
	require.Equal(t, vaults, w.vaultCount(), "its vault is removed at NMI")
	require.False(t, h.c.entitled("content:members"))

	// The next attempt with another card succeeds.
	session, err := h.pay("pay-retry", openrails.CheckoutPaymentOptions{PaymentToken: w.nmi.Tokenize(visa)})
	require.NoError(t, err)
	require.Equal(t, "succeeded", session.Status)
	require.Len(t, h.subscriptions(), 1)
	require.Len(t, h.methods(), 1)
}

// The saved-card path accepts the quote in the same call on both card rails.
func TestHostedSavedCardSubscription(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			c := w.newCustomer()
			method := c.saveCard(rail, visa)
			price := w.membership("content:members", 9_990_000)
			session, err := w.client[embedded].CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
				Customer: openrails.CheckoutCustomerIdentity{ID: c.id}, PriceID: price.ID, IdempotencyKey: "saved-" + uuid.NewString(),
				PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: w.psp[rail], Rail: rail, PaymentMethodID: method}, Confirm: true,
				SuccessURL: "https://greenfield.test/return", CancelURL: "https://greenfield.test/return?canceled=1",
			})
			require.NoError(t, err)
			require.Equal(t, "succeeded", session.Status)
			w.settle()
			require.True(t, c.entitled("content:members"))
		})
	}
}

// Without Confirm a token still saves the card and quotes; the signed-in
// customer accepts at /v1/me/checkout/{id}/confirm.
func TestHostedNewCardQuoteThenConfirm(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	price := w.membership("content:members", 9_990_000)
	session, err := w.client[embedded].CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
		Customer: openrails.CheckoutCustomerIdentity{ID: c.id}, PriceID: price.ID, IdempotencyKey: "quote-" + uuid.NewString(),
		PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: w.psp["nmi"], Rail: "nmi", PaymentToken: w.nmi.Tokenize(visa), NameOnCard: "Quoted Payer", Zip: "10001", Country: "US"},
	})
	require.NoError(t, err)
	require.Equal(t, "requires_action", session.Status)
	require.Empty(t, w.nmi.ledger(""), "a quote charges nothing")
	done := unwrap(c.must(http.MethodPost, "/checkout/"+session.ID+"/confirm", "", map[string]any{"payment": map[string]string{"rail": "nmi"}}))
	require.Equal(t, "succeeded", done["status"], "%v", done)
	w.settle()
	require.True(t, c.entitled("content:members"))
}

// One-time new-card sales are unchanged.
func TestHostedNewCardOneTimeSale(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	client := w.client[embedded]
	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "post-" + uuid.NewString()[:8], DisplayName: "Paid post", EntitlementsSpec: map[string]*int{"content:post": nil}})
	require.NoError(t, err)
	price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 4_990_000, Currency: "USD"})
	require.NoError(t, err)
	h := hostedPay{w: w, c: w.newCustomer(), tp: embedded, price: price.ID}
	session, err := h.pay("sale-1", openrails.CheckoutPaymentOptions{PaymentToken: w.nmi.Tokenize(visa)})
	require.NoError(t, err)
	require.Equal(t, "succeeded", session.Status)
	w.settle()
	require.True(t, h.c.entitled("content:post"))
	require.Empty(t, h.subscriptions())
	require.Len(t, w.nmi.ledger(""), 1)
}
