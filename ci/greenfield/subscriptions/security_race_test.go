//go:build greenfield && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// raw is a goroutine-safe customer request: it reports instead of failing.
func (c *customer) raw(server, method, path string, body any) (int, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(c.w.t.Context(), method, server+mountPrefix+"/v1/me"+path, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	res.Body.Close()
	return res.StatusCode, nil
}

// chargeGate parks the rail's next charge request at the provider.
func (w *world) chargeGate(rail string) *gate {
	if rail == "stripe" {
		return w.stripe.hold(newGate(func(r *http.Request) bool {
			return r.Method == http.MethodPost && r.URL.Path == "/v1/payment_intents"
		}, false))
	}
	return w.nmi.hold(newGate(func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/transact.php") }, false))
}

func (w *world) refundGate(rail string) *gate {
	if rail == "stripe" {
		return w.stripe.hold(newGate(func(r *http.Request) bool { return r.Method == http.MethodPost && r.URL.Path == "/v1/refunds" }, false))
	}
	return w.nmi.hold(newGate(func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/refund") }, false))
}

// SEC: double spend on confirmation. While the first confirmation's charge is
// in flight at the provider, the same customer confirms the same checkout
// again on another replica and on the same one. The provider sees one charge
// and exactly one membership is created.
func TestSecurityConcurrentConfirmChargesOnce(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			replica := w.sibling()
			price := w.membership("content:members", 9_990_000)
			c := w.newCustomer()
			method := c.saveCard(rail, visa)
			session, err := w.client[embedded].CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
				OfferKind: openrails.OfferRecurring, Customer: openrails.CheckoutCustomerIdentity{ID: c.id}, Entitlement: "content:members", PriceID: price.ID,
				IdempotencyKey: "race-" + uuid.NewString(), PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: w.psp[rail], Rail: rail, PaymentMethodID: method},
				SuccessURL: "https://greenfield.test/return", CancelURL: "https://greenfield.test/return",
			})
			require.NoError(t, err)
			g := w.chargeGate(rail)
			confirm := map[string]any{"payment": map[string]string{"rail": rail}}
			var wg sync.WaitGroup
			statuses := make(chan int, 8)
			send := func(server string) {
				defer wg.Done()
				status, err := c.raw(server, http.MethodPost, "/checkout/"+session.ID+"/confirm", confirm)
				if err == nil {
					statuses <- status
				}
			}
			wg.Add(1)
			go send(w.server.URL)
			select {
			case <-g.arrived:
			case <-time.After(20 * time.Second):
				t.Fatal("the first confirmation never reached the provider")
			}
			for _, server := range []string{w.server.URL, replica.server.URL, replica.server.URL, w.server.URL} {
				wg.Add(1)
				go send(server)
			}
			time.Sleep(500 * time.Millisecond)
			close(g.release)
			wg.Wait()
			close(statuses)
			for status := range statuses {
				t.Logf("confirm -> %d", status)
			}
			w.stripe.unhold()
			w.nmi.unhold()
			w.settle()
			require.Len(t, w.railLedger(rail), 1, "one provider charge")
			subs, err := w.client[embedded].ListSubscriptions(t.Context(), openrails.SubscriptionFilter{CustomerID: c.id})
			require.NoError(t, err)
			require.Len(t, subs.Data, 1, "one membership")
			require.Len(t, completed(w.payments(embedded, c.id)), 1, "one recorded payment")
			require.True(t, c.entitled("content:members"))
		})
	}
}

// SEC: concurrent refunds. Merchant refunds of one payment racing across
// replicas and topologies, under distinct idempotency keys, never return
// more than was paid, at the provider or in the ledger.
func TestSecurityConcurrentRefundsNeverExceedPayment(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		for _, partial := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/partial=%t", rail, partial), func(t *testing.T) {
				t.Parallel()
				w := newWorld(t)
				replica := w.sibling()
				e := enroll(t, w, rail, embedded)
				payment := completed(w.payments(embedded, e.c.id))[0]
				params := openrails.RefundPaymentParams{Full: true, Reason: "requested_by_customer"}
				if partial {
					params = openrails.RefundPaymentParams{Amount: 6_000_000, Reason: "requested_by_customer"}
				}
				g := w.refundGate(rail)
				clients := []*openrails.Client{w.client[embedded], w.client[remote], replica.client, w.client[embedded], replica.client}
				var wg sync.WaitGroup
				var mu sync.Mutex
				accepted := 0
				refund := func(i int, client *openrails.Client) {
					defer wg.Done()
					p := params
					p.IdempotencyKey = fmt.Sprintf("race-refund-%d", i)
					if _, err := client.RefundPayment(context.WithoutCancel(t.Context()), payment.ID, p); err == nil {
						mu.Lock()
						accepted++
						mu.Unlock()
					}
				}
				wg.Add(1)
				go refund(0, clients[0])
				select {
				case <-g.arrived:
				case <-time.After(20 * time.Second):
					t.Fatal("the first refund never reached the provider")
				}
				for i, client := range clients[1:] {
					wg.Add(1)
					go refund(i+1, client)
				}
				time.Sleep(500 * time.Millisecond)
				close(g.release)
				wg.Wait()
				w.stripe.unhold()
				w.nmi.unhold()
				w.settle()
				t.Logf("accepted refunds: %d", accepted)
				var refunded int64
				for _, entry := range e.providerLedger() {
					require.LessOrEqual(t, entry.Refunded, entry.Amount, "the provider never refunds more than it charged")
					refunded += entry.Refunded
				}
				want := e.amount
				if partial {
					want = 600
				}
				require.EqualValues(t, want, refunded, "exactly one refund reached the provider")
				got, err := w.client[embedded].GetPayment(t.Context(), payment.ID)
				require.NoError(t, err)
				require.LessOrEqual(t, got.AmountRefunded, got.Amount)
				require.EqualValues(t, want*10_000, got.AmountRefunded)
			})
		}
	}
}

// SEC: card testing. One customer submitting card after card is throttled
// per customer and per client address before the gateway sees most of them;
// throttled requests never reach the provider.
func TestSecurityCardTestingIsThrottled(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	vaults := func() int {
		w.nmi.mu.Lock()
		defer w.nmi.mu.Unlock()
		return len(w.nmi.vaults)
	}
	before := vaults()
	limited := 0
	for i := range 60 {
		token := w.nmi.tokenize(card{Brand: "visa", Last4: fmt.Sprintf("%04d", i)})
		status, err := c.raw(w.server.URL, http.MethodPost, "/payment-methods", map[string]any{"provider": "nmi", "psp_id": w.psp["nmi"], "payment_token": token, "name_on_card": "Card Tester"})
		require.NoError(t, err)
		if status == http.StatusTooManyRequests {
			limited++
		}
	}
	saved := vaults() - before
	t.Logf("saved %d cards, %d requests throttled", saved, limited)
	require.Positive(t, limited, "card submissions are throttled")
	require.LessOrEqual(t, saved, 40, "throttled submissions never reach the gateway")
}
