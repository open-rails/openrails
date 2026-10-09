//go:build e2e && integration

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
	"github.com/open-rails/openrails/billing"
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

// releaseAfterRacers lets the held request through once every racer has been
// answered, so each racer is decided while the first is still at the
// provider. A racer that waits on the held request fails the test.
func releaseAfterRacers(t *testing.T, g *gate, racers *sync.WaitGroup) {
	t.Helper()
	answered := make(chan struct{})
	go func() { racers.Wait(); close(answered) }()
	select {
	case <-answered:
		close(g.release)
	case <-time.After(30 * time.Second):
		close(g.release)
		t.Fatal("a racing request waited on the held one instead of being answered")
	}
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

// SEC: double spend on one checkout. While the first request's charge is in
// flight at the provider, the same checkout (one idempotency key) is sent
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
			request := billing.CreateCheckoutAttemptParams{
				OfferKind: billing.OfferRecurring, Customer: c.identity(), Entitlement: "content:members", PriceID: price.ID,
				IdempotencyKey: "race-" + uuid.NewString(), PaymentOptions: billing.CheckoutPaymentOptions{PSP: rail, PaymentMethodID: pmid(method)},
				SuccessURL: "https://e2e.test/return", CancelURL: "https://e2e.test/return",
			}
			g := w.chargeGate(rail)
			var first, racers sync.WaitGroup
			outcomes := make(chan error, 8)
			send := func(wg *sync.WaitGroup, client *openrails.Client) {
				defer wg.Done()
				_, err := createCheckoutAttempt(context.WithoutCancel(t.Context()), client, request)
				outcomes <- err
			}
			first.Add(1)
			go send(&first, w.client[embedded])
			select {
			case <-g.arrived:
			case <-time.After(20 * time.Second):
				t.Fatal("the first request never reached the provider")
			}
			for _, client := range []*openrails.Client{w.client[embedded], replica.client, replica.client, w.client[remote]} {
				racers.Add(1)
				go send(&racers, client)
			}
			releaseAfterRacers(t, g, &racers)
			first.Wait()
			close(outcomes)
			for err := range outcomes {
				t.Logf("create -> %v", err)
			}
			w.stripe.unhold()
			w.nmi.unhold()
			w.settle()
			require.Len(t, w.railLedger(rail), 1, "one provider charge")
			subs, err := w.client[embedded].ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: c.customerID()})
			require.NoError(t, err)
			require.Len(t, subs.Items, 1, "one membership")
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
				params := billing.RefundPaymentParams{Full: true, Reason: "requested_by_customer"}
				if partial {
					params = billing.RefundPaymentParams{Amount: 6_000_000, Reason: "requested_by_customer"}
				}
				g := w.refundGate(rail)
				clients := []*openrails.Client{w.client[embedded], w.client[remote], replica.client, w.client[embedded], replica.client}
				var first, racers sync.WaitGroup
				var mu sync.Mutex
				accepted := 0
				refund := func(wg *sync.WaitGroup, i int, client *openrails.Client) {
					defer wg.Done()
					p := params
					p.IdempotencyKey = fmt.Sprintf("race-refund-%d", i)
					if _, err := client.RefundPayment(context.WithoutCancel(t.Context()), payment.ID, p); err == nil {
						mu.Lock()
						accepted++
						mu.Unlock()
					}
				}
				first.Add(1)
				go refund(&first, 0, clients[0])
				select {
				case <-g.arrived:
				case <-time.After(20 * time.Second):
					t.Fatal("the first refund never reached the provider")
				}
				for i, client := range clients[1:] {
					racers.Add(1)
					go refund(&racers, i+1, client)
				}
				releaseAfterRacers(t, g, &racers)
				first.Wait()
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
	vaults := func() int { return len(w.nmi.Vaults()) }
	before := vaults()
	limited := 0
	for i := range 60 {
		token := w.nmi.Tokenize(card{Brand: "visa", Last4: fmt.Sprintf("%04d", i)})
		status, err := c.raw(w.server.URL, http.MethodPost, "/payment-methods", map[string]any{"psp_id": w.psp["nmi"], "payment_token": token, "billing_details": map[string]any{"name": "Card Tester"}})
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
