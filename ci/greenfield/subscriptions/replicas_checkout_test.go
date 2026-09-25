//go:build greenfield && integration

package subscriptions_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
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
			f.crash(a) // nothing it does from here is recorded
			die()      // its request dies with it; the provider already took the charge
			h.release()
			require.Eventually(t, func() bool { return charges() == before+1 }, 10*time.Second, 10*time.Millisecond, "the provider charged")
			f.unhold()

			_, err = b.client[embedded].CreateCheckoutSession(t.Context(), req)
			var status *openrails.StatusError
			require.True(t, errors.As(err, &status), "the dead replica's claim holds until its lease lapses: %v", err)
			require.Equal(t, http.StatusConflict, status.Status)

			f.lapseCheckoutClaims(victim.id)
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
			require.Empty(t, f.base.nmi.Unexpected())
		})
	}
}

// lapseCheckoutClaims ends a customer's processing checkout claims on the
// database clock, as a dead owner's silence or starved renewals would.
func (f *fleet) lapseCheckoutClaims(customerID string) {
	f.t.Helper()
	_, err := f.base.pool.Exec(f.t.Context(), f.q(`UPDATE openrails.idempotency_keys SET lease_expires_at = now() - interval '1 millisecond'
		WHERE operation = 'checkout_session_create' AND idempotency_key LIKE $1 AND status = 'processing'`), customerID+":%")
	require.NoError(f.t, err)
}

func requireStatus(t *testing.T, err error, want int) *openrails.StatusError {
	t.Helper()
	var status *openrails.StatusError
	require.True(t, errors.As(err, &status), "%v", err)
	require.Equal(t, want, status.Status, "%v", err)
	return status
}

// #1099 interleavings, each asserting the provider's own journal: an owner
// whose lease lapses while it is still alive (inside its vault call) never
// charges a session another request settled; a stale request for an
// abandoned attempt never re-runs it; a declined request replays its decline.
func TestReplicasCheckoutLeaseLapse(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2)
	a, b := f.replicas[0], f.replicas[1]
	price := a.permanent("content:pass")
	charges := func() int { return len(f.base.nmi.ledger("")) }
	request := func(c *customer, key, token string) openrails.CreateCheckoutSessionRequest {
		return openrails.CreateCheckoutSessionRequest{
			Customer: openrails.CheckoutCustomerIdentity{ID: c.id}, PriceID: price.ID, IdempotencyKey: key, Confirm: true,
			PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: a.psp["nmi"], Rail: "nmi", PaymentToken: token, NameOnCard: "Pass Payer", Zip: "10001", Country: "US"},
		}
	}
	firstSessionStatus := func(c *customer) string {
		var status string
		require.NoError(t, f.base.pool.QueryRow(t.Context(), f.q(`SELECT status FROM openrails.checkout_sessions
			WHERE customer_id = $1 ORDER BY created_at LIMIT 1`), c.id).Scan(&status))
		return status
	}
	c := a.newCustomer()
	before := charges()

	// Attempt 1. Replica a is inside its NMI vault call when its lease lapses.
	req1 := request(c, "checkout:"+uuid.NewString()+":1", f.base.nmi.Tokenize(visa))
	vault := f.hold("nmi", func(r *http.Request) bool {
		return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v5/customers")
	}, true)
	owner := make(chan error, 1)
	go func() {
		_, err := a.client[embedded].CreateCheckoutSession(context.WithoutCancel(t.Context()), req1)
		owner <- err
	}()
	require.Equal(t, a, vault.wait())
	_, err := b.client[embedded].LookupCheckoutSession(t.Context(), req1)
	requireStatus(t, err, http.StatusConflict) // a host never moves on while the owner works
	_, err = b.client[embedded].CreateCheckoutSession(t.Context(), req1)
	requireStatus(t, err, http.StatusConflict)

	f.lapseCheckoutClaims(c.id)
	f.base.nmi.DeclineValidations(1)
	_, err = b.client[embedded].CreateCheckoutSession(t.Context(), req1) // reclaims; its card verification is declined
	refused := requireStatus(t, err, http.StatusPaymentRequired)
	require.Equal(t, "failed", firstSessionStatus(c), "a definite refusal fails the session")

	// The owner resumes after its vault call and must not charge the session
	// the other request failed.
	vault.release()
	requireStatus(t, <-owner, http.StatusConflict)
	f.settle()
	require.Equal(t, before, charges(), "the superseded owner never charges")

	// The host moves on to attempt 2, which charges once.
	s2, err := b.client[embedded].CreateCheckoutSession(t.Context(), request(c, "checkout:"+uuid.NewString()+":2", f.base.nmi.Tokenize(visa)))
	require.NoError(t, err)
	require.Equal(t, "succeeded", s2.Status)
	f.settle()
	require.Equal(t, before+1, charges())

	// A stale request for attempt 1 reclaims its key: failed is final.
	for _, r := range []*world{a, b} {
		_, err = r.client[embedded].CreateCheckoutSession(t.Context(), req1)
		again := requireStatus(t, err, refused.Status)
		require.Equal(t, refused.Code, again.Code, "a replay answers as the refusal did")
	}
	f.settle()
	require.Equal(t, before+1, charges(), "a stale attempt never charges after the host moved on")

	// A declined sale settles its claim at once: the replay is the decline.
	d := b.newCustomer()
	req3 := request(d, "checkout:"+uuid.NewString()+":1", f.base.nmi.Tokenize(card{Brand: "visa", Last4: "0002", Decline: "202"}))
	_, err = b.client[embedded].CreateCheckoutSession(t.Context(), req3)
	declined := requireStatus(t, err, http.StatusPaymentRequired)
	require.Equal(t, openrails.CodeCardDeclined, declined.Code)
	for _, r := range []*world{a, b} {
		_, err = r.client[embedded].CreateCheckoutSession(t.Context(), req3)
		again := requireStatus(t, err, http.StatusPaymentRequired)
		require.Equal(t, declined.Code, again.Code)
	}
	f.settle()
	require.Equal(t, before+1, charges())
	require.Empty(t, f.base.nmi.Unexpected())
}

func (f *fleet) submissionCount(rail string) int {
	if rail == "stripe" {
		return f.base.stripe.attempts("")
	}
	return len(f.base.nmi.Attempts())
}
