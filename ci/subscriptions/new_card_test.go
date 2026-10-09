//go:build e2e && integration

package subscriptions_test

import (
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
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

func (h hostedPay) pay(key string, payment billing.CheckoutPaymentOptions) (*billing.CheckoutAttempt, error) {
	h.w.t.Helper()
	payment.PSP = "nmi"
	if payment.PaymentToken != "" {
		payment.BillingDetails = &billing.BillingDetails{Name: new("Hosted Payer"), Address: &billing.BillingAddress{PostalCode: new("10001"), Country: new("US")}}
	}
	return createCheckoutAttempt(h.w.t.Context(), h.w.client[h.tp], billing.CreateCheckoutAttemptParams{
		Customer: billing.CheckoutCustomerIdentity{ID: cid(h.c.id)}, PriceID: pid(h.price), IdempotencyKey: key,
		PaymentOptions: payment,
	})
}

func (h hostedPay) methods() []billing.PaymentMethod {
	h.w.t.Helper()
	page, err := h.w.client[embedded].ListPaymentMethods(h.w.t.Context(), h.c.cid(), billing.PageRequest{Limit: 100})
	require.NoError(h.w.t, err)
	return page.Items
}

func (h hostedPay) subscriptions() []billing.Subscription {
	h.w.t.Helper()
	subs, err := h.w.client[embedded].ListSubscriptions(h.w.t.Context(), billing.SubscriptionListParams{CustomerID: h.c.customerID()})
	require.NoError(h.w.t, err)
	return subs.Items
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
			h := hostedPay{w: w, c: w.newCustomer(), tp: tp, price: w.membership("content:members", 9_990_000).ID.String()}
			token := w.nmi.Tokenize(visa)

			session, err := h.pay("pay-1", billing.CheckoutPaymentOptions{PaymentToken: token})
			require.NoError(t, err)
			require.Equal(t, "succeeded", string(session.Status))
			require.NotNil(t, session.SubscriptionID)
			w.settle()
			require.True(t, h.c.entitled("content:members"))
			subs := h.subscriptions()
			require.Len(t, subs, 1)
			require.Equal(t, "engine", subs[0].CollectionPolicy)
			require.Equal(t, billing.SubscriptionActive, subs[0].Status)
			require.Equal(t, *session.SubscriptionID, subs[0].ID)
			methods := h.methods()
			require.Len(t, methods, 1, "the new card is saved for renewals")
			require.NotNil(t, subs[0].PaymentMethodID)
			require.Equal(t, methods[0].ID.String(), subs[0].PaymentMethodID.String())
			require.Len(t, w.nmi.ledger(""), 1, "one initial charge")

			// A retried or double-submitted pay replays the accepted enrollment.
			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					again, err := h.pay("pay-1", billing.CheckoutPaymentOptions{PaymentToken: token})
					if err == nil {
						require.Equal(t, session.ID, again.ID)
						require.Equal(t, "succeeded", string(again.Status))
					} else {
						require.ErrorIs(t, err, billing.ErrConflict, "a concurrent duplicate is refused, never charged")
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
	h := hostedPay{w: w, c: w.newCustomer(), tp: embedded, price: w.membership("content:members", 9_990_000).ID.String()}
	vaults := w.vaultCount()

	_, err := h.pay("pay-declined", billing.CheckoutPaymentOptions{PaymentToken: w.nmi.Tokenize(card{Brand: "visa", Last4: "0002", Decline: "202"})})
	require.ErrorIs(t, err, billing.ErrPaymentRefused)
	var status *billing.StatusError
	require.True(t, errors.As(err, &status))
	require.Equal(t, billing.CodeCardDeclined, status.Code)
	w.settle()
	require.Empty(t, h.subscriptions())
	require.Empty(t, h.methods(), "the declined card is not kept")
	require.Equal(t, vaults, w.vaultCount(), "its vault is removed at NMI")
	require.False(t, h.c.entitled("content:members"))

	// The next attempt with another card succeeds.
	session, err := h.pay("pay-retry", billing.CheckoutPaymentOptions{PaymentToken: w.nmi.Tokenize(visa)})
	require.NoError(t, err)
	require.Equal(t, "succeeded", string(session.Status))
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
			session, err := createCheckoutAttempt(t.Context(), w.client[embedded], billing.CreateCheckoutAttemptParams{
				Customer: billing.CheckoutCustomerIdentity{ID: cid(c.id)}, PriceID: price.ID, IdempotencyKey: "saved-" + uuid.NewString(),
				PaymentOptions: billing.CheckoutPaymentOptions{PSP: rail, PaymentMethodID: pmid(method)},
				SuccessURL:     "https://e2e.test/return", CancelURL: "https://e2e.test/return?canceled=1",
			})
			require.NoError(t, err)
			require.Equal(t, "succeeded", string(session.Status))
			w.settle()
			require.True(t, c.entitled("content:members"))
		})
	}
}

// A purchase the merchant prepares for its customer is handed over as a
// checkout session (D1): the customer pays it on the page with a new card.
func TestMerchantHandsOverACheckoutSession(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	price := w.membership("content:members", 9_990_000)
	session := w.handOver(c, price.ID)
	require.Empty(t, w.nmi.ledger(""), "a handed-over session charges nothing")
	status, done := session.payCard(visa)
	require.Equal(t, http.StatusOK, status, "%v", done)
	require.Equal(t, "succeeded", done["status"], "%v", done)
	w.settle()
	require.True(t, c.entitled("content:members"))
	require.Len(t, w.nmi.ledger(""), 1)
}

// One-time new-card sales are unchanged.
func TestHostedNewCardOneTimeSale(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	client := w.client[embedded]
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "post-" + uuid.NewString()[:8], DisplayName: "Paid post", Entitlements: []string{"content:post"}})
	require.NoError(t, err)
	price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 4_990_000, Currency: "USD"})
	require.NoError(t, err)
	h := hostedPay{w: w, c: w.newCustomer(), tp: embedded, price: price.ID.String()}
	session, err := h.pay("sale-1", billing.CheckoutPaymentOptions{PaymentToken: w.nmi.Tokenize(visa)})
	require.NoError(t, err)
	require.Equal(t, "succeeded", string(session.Status))
	w.settle()
	require.True(t, h.c.entitled("content:post"))
	require.Empty(t, h.subscriptions())
	require.Len(t, w.nmi.ledger(""), 1)
}

// A Stripe card setup is a checkout attempt the merchant can read.
func TestStripeCardSetupReadsAsAnAttempt(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("stripe", visa)
	var id uuid.UUID
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT id FROM billing.checkout_attempts WHERE customer_id = $1 AND mode = 'payment_method'`), c.id).Scan(&id))
	got := w.attempt(billing.CheckoutAttemptID(id))
	require.Equal(t, "payment_method", got["mode"])
	require.Equal(t, "succeeded", got["status"])
	require.Equal(t, method, got["payment_method_id"])
}
