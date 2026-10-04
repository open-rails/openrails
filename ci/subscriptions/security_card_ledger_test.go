//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// saveFrom submits one NMI card save for c to replica r from client address ip.
func (c *customer) saveFrom(r *world, ip string, cd card) int {
	c.w.t.Helper()
	body, err := json.Marshal(map[string]any{"psp_id": r.psp["nmi"], "payment_token": r.nmi.Tokenize(cd), "billing_details": map[string]any{"name": "Card Tester"}})
	require.NoError(c.w.t, err)
	req, err := http.NewRequestWithContext(c.w.t.Context(), http.MethodPost, r.server.URL+mountPrefix+"/v1/me/payment-methods", bytes.NewReader(body))
	require.NoError(c.w.t, err)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	res, err := http.DefaultClient.Do(req)
	require.NoError(c.w.t, err)
	res.Body.Close()
	return res.StatusCode
}

func (f *fleet) refusedSaves() int {
	return f.base.nmi.RefusedSaves()
}

var refusedCard = card{Brand: "visa", Last4: "0119", Decline: "vault"}

// SEC-30: card testing is counted in PostgreSQL, so the blocks hold across
// replicas with no Redis and no captcha. Per customer and per client address,
// six refused cards within fifteen minutes block further attempts on every
// replica, before the gateway sees them; the block covers checkout through
// the embedded and remote Client too, and lifts when the window passes. Ten
// in a day block for the day. A merchant-wide wave of refusals is attack mode:
// any subject with a recent refusal is blocked, clean customers are not.
func TestSecurityCardTestingLedgerAcrossReplicas(t *testing.T) {
	t.Parallel()
	t.Run("customer", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t, 2)
		a, b := f.replicas[0], f.replicas[1]
		price := a.membership("content:members", 9_990_000)
		c := a.newCustomer()
		for i := range 6 {
			r := f.replicas[i%2]
			require.Equal(t, http.StatusBadGateway, c.saveFrom(r, fmt.Sprintf("198.51.100.%d", i+1), refusedCard), "attempt %d reaches the gateway", i)
		}
		require.Equal(t, 6, f.refusedSaves())
		for _, r := range []*world{a, b} {
			require.Equal(t, http.StatusTooManyRequests, c.saveFrom(r, "198.51.100.99", refusedCard), "blocked on replica %s", r.replica.name)
			require.Equal(t, http.StatusTooManyRequests, c.saveFrom(r, "198.51.100.99", visa), "a good card is refused while blocked")
			for _, tp := range []topology{embedded, remote} {
				_, err := r.client[tp].CreateCheckoutAttempt(t.Context(), billing.CreateCheckoutAttemptRequest{
					OfferKind: billing.OfferRecurring, Customer: billing.CheckoutCustomerIdentity{ID: cid(c.id)}, Entitlement: "content:members", PriceID: price.ID,
					IdempotencyKey: "blocked-" + uuid.NewString(), PaymentOptions: billing.CheckoutPaymentOptions{PSPID: r.psp["nmi"], Rail: "nmi"},
					SuccessURL: "https://e2e.test/return", CancelURL: "https://e2e.test/return?canceled=1",
				})
				require.Error(t, err, "%s checkout for a blocked customer", tp)
			}
		}
		require.Equal(t, 6, f.refusedSaves(), "blocked attempts never reach the gateway")

		f.advance(16 * time.Minute)
		require.Equal(t, http.StatusCreated, c.saveFrom(b, "198.51.100.99", visa), "the burst block lifts with its window")
		for i := range 4 {
			require.Equal(t, http.StatusBadGateway, c.saveFrom(f.replicas[i%2], "203.0.113.7", refusedCard))
			f.advance(4 * time.Minute)
		}
		f.advance(16 * time.Minute)
		require.Equal(t, http.StatusTooManyRequests, c.saveFrom(a, "203.0.113.8", visa), "ten refusals in a day block for the day")
		require.Equal(t, 10, f.refusedSaves())
	})
	t.Run("address", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t, 2)
		for i := range 6 {
			c := f.replicas[0].newCustomer()
			require.Equal(t, http.StatusBadGateway, c.saveFrom(f.replicas[i%2], "192.0.2.10", refusedCard))
		}
		fresh := f.replicas[1].newCustomer()
		require.Equal(t, http.StatusTooManyRequests, fresh.saveFrom(f.replicas[1], "192.0.2.10", visa), "one address testing cards through many accounts is blocked")
		require.Equal(t, http.StatusCreated, fresh.saveFrom(f.replicas[0], "192.0.2.11", visa), "another address is not")
		require.Equal(t, 6, f.refusedSaves())
	})
	t.Run("merchant_attack", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t, 2)
		var last *customer
		for i := range 100 {
			last = f.replicas[0].newCustomer()
			require.Equal(t, http.StatusBadGateway, last.saveFrom(f.replicas[i%2], fmt.Sprintf("10.%d.%d.1", i/200, i%200), refusedCard))
		}
		require.Equal(t, http.StatusTooManyRequests, last.saveFrom(f.replicas[1], "10.9.9.9", visa), "in attack mode one recent refusal blocks")
		clean := f.replicas[1].newCustomer()
		require.Equal(t, http.StatusCreated, clean.saveFrom(f.replicas[0], "10.9.9.10", visa), "a customer with no refusals still saves a card")
		require.Equal(t, 100, f.refusedSaves())
	})
}

// SEC: a host that checks out in-process through the Client passes each
// customer's client address, and the decline ledger counts it as it counts the
// customer routes. One address testing cards through many accounts is
// blocked; a card-testing wave from many accounts and addresses is attack
// mode, where a tester's next card is refused before the gateway while a clean
// buyer still pays. A client_ip that is not an IP address is refused.
func TestSecurityCardTestingThroughTheHost(t *testing.T) {
	t.Parallel()
	declined := card{Brand: "visa", Last4: "0002", Decline: "202"}
	pay := func(w *world, tp topology, price string, c *customer, ip string, cd card) error {
		_, err := w.client[tp].CreateCheckoutAttempt(t.Context(), billing.CreateCheckoutAttemptRequest{
			Customer: billing.CheckoutCustomerIdentity{ID: cid(c.id), ClientIP: ip}, PriceID: pid(price), IdempotencyKey: "host-" + uuid.NewString(),
			PaymentOptions: billing.CheckoutPaymentOptions{PSPID: w.psp["nmi"], Rail: "nmi", PaymentToken: w.nmi.Tokenize(cd), NameOnCard: "Host Payer", Zip: "10001", Country: "US"},
		})
		return err
	}
	refused := func(t *testing.T, err error, status int) {
		t.Helper()
		var se *billing.StatusError
		require.ErrorAs(t, err, &se)
		require.Equal(t, status, se.Status, "%v", err)
		if status == http.StatusTooManyRequests {
			require.Equal(t, "card_attempts_blocked", se.Code)
		}
	}
	t.Run("address", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		price := w.membership("content:members", 9_990_000).ID
		for range 6 {
			require.ErrorIs(t, pay(w, embedded, price.String(), w.newCustomer(), "192.0.2.20", declined), billing.ErrPaymentRefused)
		}
		sales := len(w.nmi.Sales())
		refused(t, pay(w, embedded, price.String(), w.newCustomer(), "192.0.2.20", visa), http.StatusTooManyRequests)
		require.Len(t, w.nmi.Sales(), sales, "a blocked attempt never reaches the gateway")
		require.NoError(t, pay(w, embedded, price.String(), w.newCustomer(), "192.0.2.21", visa), "another address pays")
	})
	t.Run("wave", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		price := w.membership("content:members", 9_990_000).ID
		var tester *customer
		for i := range 100 {
			tester = w.newCustomer()
			require.ErrorIs(t, pay(w, embedded, price.String(), tester, fmt.Sprintf("2001:db8:77:%x::1", i+1), declined), billing.ErrPaymentRefused)
		}
		sales := len(w.nmi.Sales())
		refused(t, pay(w, embedded, price.String(), tester, "2001:db8:77:64::1", visa), http.StatusTooManyRequests)
		require.Len(t, w.nmi.Sales(), sales, "in attack mode one recent decline blocks, before the gateway")
		require.NoError(t, pay(w, embedded, price.String(), w.newCustomer(), "2001:db8:77:1000::1", visa), "a clean buyer still pays")
	})
	t.Run("invalid", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		price := w.membership("content:members", 9_990_000).ID
		for _, tp := range []topology{embedded, remote} {
			refused(t, pay(w, tp, price.String(), w.newCustomer(), "not-an-ip", visa), http.StatusBadRequest)
		}
		require.Empty(t, w.nmi.Sales())
	})
}
