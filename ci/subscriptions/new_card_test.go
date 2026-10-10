//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// A hosted checkout: the customer's one "Subscribe" click on the payment page
// sends a fresh Collect.js token, and OpenRails saves the card, quotes the
// price and enrolls it in one pay.
type hostedPay struct {
	w     *world
	c     *customer
	tp    topology
	price string
}

// hostedPaySessions are the sessions hostedPay minted, by customer and key.
var hostedPaySessions sync.Map

// session is the session key names: the first pay with a key mints it through
// tp's Client, later pays with it pay the same session again.
func (h hostedPay) session(key string) hostedSession {
	h.w.t.Helper()
	if s, ok := hostedPaySessions.Load(h.c.id + "/" + key); ok {
		return s.(hostedSession)
	}
	s, err := h.c.sell(h.tp, order{price: pid(h.price)})
	require.NoError(h.w.t, err)
	hostedPaySessions.Store(h.c.id+"/"+key, s)
	return s
}

// pay is the customer paying key's session with a card the page tokenized.
func (h hostedPay) pay(key, token string) (*sessionPaid, error) {
	h.w.t.Helper()
	return h.session(key).buy(h.c, order{rail: "nmi", token: token})
}

// declinedPay is a pay the issuer declined: nothing charged, the page shows
// why and takes another card.
func declinedPay(t *testing.T, paid *sessionPaid, err error) billing.PaymentFailure {
	t.Helper()
	require.NoError(t, err)
	require.Equal(t, "failed", paid.Status, "%+v", paid.CheckoutSessionPayResult)
	require.NotNil(t, paid.Failure, "a decline explains itself: %+v", paid.CheckoutSessionPayResult)
	return *paid.Failure
}

func (h hostedPay) methods() []billing.PaymentMethod {
	h.w.t.Helper()
	page, err := h.w.client[embedded].ListPaymentMethods(h.w.t.Context(), h.c.cid(), billing.PaymentMethodListParams{PageRequest: billing.PageRequest{Limit: 100}})
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

			session, err := h.pay("pay-1", token)
			require.NoError(t, err)
			require.Equal(t, "succeeded", session.Status)
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
			paid := h.session("pay-1")
			option := paid.optionAt(w.server.URL, "nmi")
			outcomes := make(chan error, 2)
			var wg sync.WaitGroup
			for range 2 {
				wg.Go(func() {
					again, err := paid.payAt(t.Context(), w.server.URL, option, h.c, order{token: token})
					if err == nil && (again.Status != "succeeded" || *again.SubscriptionID != *session.SubscriptionID) {
						err = fmt.Errorf("replayed as %+v", again.CheckoutSessionPayResult)
					}
					outcomes <- err
				})
			}
			wg.Wait()
			close(outcomes)
			for err := range outcomes {
				if err != nil {
					require.ErrorIs(t, err, billing.ErrConflict, "a concurrent duplicate is refused, never charged")
				}
			}
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

	paid, err := h.pay("pay-declined", w.nmi.Tokenize(card{Brand: "visa", Last4: "0002", Decline: "202"}))
	failure := declinedPay(t, paid, err)
	require.Equal(t, "insufficient_funds", failure.Reason)
	w.settle()
	require.Empty(t, h.subscriptions())
	require.Empty(t, h.methods(), "the declined card is not kept")
	require.Equal(t, vaults, w.vaultCount(), "its vault is removed at NMI")
	require.False(t, h.c.entitled("content:members"))

	// The next attempt with another card succeeds.
	session, err := h.pay("pay-retry", w.nmi.Tokenize(visa))
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
			c.mustCheckout(embedded, order{price: price.ID, rail: rail, method: method, successURL: "https://e2e.test/return"})
			require.True(t, c.entitled("content:members"))
		})
	}
}

// A purchase the merchant prepares for its customer is handed over as a
// checkout session: the customer pays it on the page with a new card.
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

// A one-time sale on a new card grants its access without a subscription.
func TestHostedNewCardOneTimeSale(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	client := w.client[embedded]
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "post-" + uuid.NewString()[:8], DisplayName: "Paid post", Entitlements: []string{"content:post"}})
	require.NoError(t, err)
	price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 4_990_000, Currency: "USD"})
	require.NoError(t, err)
	h := hostedPay{w: w, c: w.newCustomer(), tp: embedded, price: price.ID.String()}
	session, err := h.pay("sale-1", w.nmi.Tokenize(visa))
	require.NoError(t, err)
	require.Equal(t, "succeeded", session.Status)
	w.settle()
	require.True(t, h.c.entitled("content:post"))
	require.Empty(t, h.subscriptions())
	require.Len(t, w.nmi.ledger(""), 1)
}
