//go:build greenfield && integration

package subscriptions_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// #1099: an embedded CreateCheckoutSession claims its request key in
// PostgreSQL. Replicas racing one key charge once, and a replica that dies
// after its provider charge left the claim processing: the claim lapses, a
// retry on another replica reclaims it and resumes the same sale operation,
// never charging again.
func TestReplicasCheckoutIdempotency(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t, 2)
			a, b := f.replicas[0], f.replicas[1]
			price := a.permanent("content:post")
			charges := func() int {
				if rail == "stripe" {
					return len(f.base.stripe.ledger(""))
				}
				return len(f.base.nmi.ledger(""))
			}
			request := func(c *customer, method, key string) openrails.CreateCheckoutSessionRequest {
				return openrails.CreateCheckoutSessionRequest{
					OfferKind: openrails.OfferPermanent, Customer: openrails.CheckoutCustomerIdentity{ID: c.id}, Entitlement: "content:post", PriceID: price.ID,
					IdempotencyKey: key, PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: a.psp[rail], Rail: rail, PaymentMethodID: method},
					SuccessURL: "https://greenfield.test/return", CancelURL: "https://greenfield.test/return",
				}
			}

			// Sixteen concurrent requests for one key, over both replicas.
			racer := a.newCustomer()
			req := request(racer, racer.saveCard(rail, visa), "checkout:"+uuid.NewString()+":1")
			before := charges()
			start := make(chan struct{})
			sessions := make([]*openrails.CheckoutSession, 16)
			errs := make([]error, 16)
			var wg sync.WaitGroup
			for i := range 16 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					sessions[i], errs[i] = f.replicas[i%2].client[embedded].CreateCheckoutSession(t.Context(), req)
				}()
			}
			close(start)
			wg.Wait()
			var id string
			for i := range 16 {
				if errs[i] != nil {
					var status *openrails.StatusError
					require.True(t, errors.As(errs[i], &status), "%v", errs[i])
					require.Equal(t, http.StatusConflict, status.Status, "only in-progress refusals: %v", errs[i])
					continue
				}
				if id == "" {
					id = sessions[i].ID
				}
				require.Equal(t, id, sessions[i].ID, "one session for the key")
			}
			require.NotEmpty(t, id, "one request ran")
			f.settle()
			require.Equal(t, before+1, charges(), "one provider charge for the key")
			replay, err := b.client[embedded].CreateCheckoutSession(t.Context(), req)
			require.NoError(t, err)
			require.Equal(t, id, replay.ID)
			require.Equal(t, "succeeded", replay.Status)
			require.True(t, racer.entitled("content:post"))

			// Replica a dies after the provider charged, before it recorded the
			// answer or completed its claim.
			victim := a.newCustomer()
			req = request(victim, victim.saveCard(rail, visa), "checkout:"+uuid.NewString()+":1")
			before, attempts := charges(), f.submissionCount(rail)
			h := f.hold(rail, submission(rail), true)
			ctx, die := context.WithCancel(t.Context())
			go func() { _, _ = a.client[embedded].CreateCheckoutSession(ctx, req) }()
			require.Equal(t, a, h.wait())
			die() // the process dies with its request; the provider already took the charge
			f.crash(a)
			h.release()
			require.Eventually(t, func() bool { return charges() == before+1 }, 10*time.Second, 10*time.Millisecond, "the provider charged")
			f.unhold()

			_, err = b.client[embedded].CreateCheckoutSession(t.Context(), req)
			var status *openrails.StatusError
			require.True(t, errors.As(err, &status), "the dead replica's claim holds until its lease lapses: %v", err)
			require.Equal(t, http.StatusConflict, status.Status)

			f.recover()
			f.settle()
			var retried *openrails.CheckoutSession
			require.Eventually(t, func() bool {
				retried, err = b.client[embedded].CreateCheckoutSession(t.Context(), req)
				if err == nil && retried.Status == "succeeded" {
					return true
				}
				f.advance(time.Minute)
				f.wake()
				f.settle()
				return false
			}, 60*time.Second, 50*time.Millisecond, "a survivor reclaims the key and resolves the sale")
			require.Equal(t, before+1, charges(), "the reclaimed request never charges again")
			require.Equal(t, attempts+1, f.submissionCount(rail), "one provider submission")
			owned, err := b.client[embedded].CheckEntitlements(t.Context(), victim.id, []string{"content:post"}, time.Time{})
			require.NoError(t, err)
			require.True(t, owned["content:post"])
			var claims int64
			require.NoError(t, f.base.pool.QueryRow(t.Context(), f.q(`SELECT claims FROM openrails.idempotency_keys
				WHERE operation = 'checkout_session_create' AND idempotency_key LIKE $1`), victim.id+":%").Scan(&claims))
			require.EqualValues(t, 2, claims, "the dead replica's claim was reclaimed once")
			require.Empty(t, f.base.stripe.unexpected())
			require.Empty(t, f.base.nmi.unexpected())
		})
	}
}

func (f *fleet) submissionCount(rail string) int {
	if rail == "stripe" {
		return f.base.stripe.attempts("")
	}
	return f.base.nmi.saleAttempts()
}
