//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// #1099: a checkout session's payment claims its attempt's key in
// PostgreSQL. Replicas racing one session charge once, and a replica that dies
// after its provider charge left the claim processing: the claim lapses, a
// pay on another replica reclaims it and resumes the same sale operation,
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
			open := func(c *customer) (hostedSession, string, order) {
				method := c.saveCard(rail, visa)
				session, err := c.sell(embedded, order{price: price.ID})
				require.NoError(t, err)
				return session, session.optionAt(a.server.URL, rail), order{method: method}
			}

			// Sixteen concurrent pays of one session, over both replicas.
			racer := a.newCustomer()
			session, option, pay := open(racer)
			before := charges()
			start := make(chan struct{})
			paid := make([]*sessionPaid, 16)
			errs := make([]error, 16)
			var wg sync.WaitGroup
			for i := range 16 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					paid[i], errs[i] = session.payAt(t.Context(), f.replicas[i%2].server.URL, option, racer, pay)
				}()
			}
			close(start)
			wg.Wait()
			var payment *billing.PaymentID
			for i := range 16 {
				if errs[i] != nil {
					var status *billing.StatusError
					require.True(t, errors.As(errs[i], &status), "%v", errs[i])
					require.Equal(t, http.StatusConflict, status.Status, "only in-progress refusals: %v", errs[i])
					continue
				}
				require.Equal(t, "succeeded", paid[i].Status, "%+v", paid[i].CheckoutSessionPayResult)
				if payment == nil {
					payment = paid[i].PaymentID
				}
				require.Equal(t, payment, paid[i].PaymentID, "one payment for the session")
			}
			require.NotNil(t, payment, "one pay ran")
			f.settle()
			require.Equal(t, before+1, charges(), "one provider charge for the session")
			replay, err := session.payAt(t.Context(), b.server.URL, option, racer, pay)
			require.NoError(t, err)
			require.Equal(t, "succeeded", replay.Status)
			require.Equal(t, payment, replay.PaymentID)
			require.True(t, racer.entitled("content:post"))

			// Replica a dies after the provider charged, before it recorded the
			// answer or completed its claim.
			victim := a.newCustomer()
			session, option, pay = open(victim)
			before, attempts := charges(), f.submissionCount(rail)
			h := f.hold(rail, submission(rail), true)
			ctx, die := context.WithCancel(t.Context())
			go func() { _, _ = session.payAt(ctx, a.server.URL, option, victim, pay) }()
			require.Equal(t, a, h.wait())
			f.crash(a) // nothing it does from here is recorded
			die()      // its request dies with it; the provider already took the charge
			h.release()
			require.Eventually(t, func() bool { return charges() == before+1 }, 10*time.Second, 10*time.Millisecond, "the provider charged")
			f.unhold()

			_, err = session.payAt(t.Context(), b.server.URL, option, victim, pay)
			var status *billing.StatusError
			require.True(t, errors.As(err, &status), "the dead replica's claim holds until its lease lapses: %v", err)
			require.Equal(t, http.StatusConflict, status.Status)

			// The page pays again while its session lives: once the dead
			// replica's claim lapses, a survivor reclaims the key and resolves
			// the sale. A session takes ten pays a minute.
			f.lapseCheckoutClaims(victim.id)
			f.any().rescue()
			f.wake()
			f.settle()
			var retried *sessionPaid
			for range 8 {
				if retried, err = session.payAt(t.Context(), b.server.URL, option, victim, pay); err == nil && retried.Status == "succeeded" {
					break
				}
				t.Logf("retry: %v %+v", err, retried)
				f.advance(time.Minute)
				f.wake()
				f.settle()
			}
			require.NoError(t, err, "a survivor reclaims the key and resolves the sale")
			require.Equal(t, "succeeded", retried.Status, "%+v", retried.CheckoutSessionPayResult)
			require.Equal(t, before+1, charges(), "the reclaimed request never charges again")
			require.Equal(t, attempts+1, f.submissionCount(rail), "one provider submission")
			owned, err := heldKeys(t.Context(), b.client[embedded], victim.customerID(), time.Time{}, "content:post")
			require.NoError(t, err)
			require.True(t, owned["content:post"])
			var claims int64
			require.NoError(t, f.base.pool.QueryRow(t.Context(), f.q(`SELECT claims FROM billing.idempotency_keys
				WHERE operation = 'checkout_attempt_create' AND idempotency_key LIKE $1`), victim.id+":%").Scan(&claims))
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
	_, err := f.base.pool.Exec(f.t.Context(), f.q(`UPDATE billing.idempotency_keys SET lease_expires_at = now() - interval '1 millisecond'
		WHERE operation = 'checkout_attempt_create' AND idempotency_key LIKE $1 AND status = 'processing'`), customerID+":%")
	require.NoError(f.t, err)
}

func requireStatus(t *testing.T, err error, want int) *billing.StatusError {
	t.Helper()
	var status *billing.StatusError
	require.True(t, errors.As(err, &status), "%v", err)
	require.Equal(t, want, status.Status, "%v", err)
	return status
}

// #1099 interleavings, each asserting the provider's own journal: an owner
// whose lease lapses while it is still alive (inside its vault call) never
// charges a session another request settled; a session whose attempt failed
// moves to its next attempt, and a stale pay never re-runs the failed one; a
// declined card fails its attempt at once.
func TestReplicasCheckoutLeaseLapse(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2)
	a, b := f.replicas[0], f.replicas[1]
	price := a.permanent("content:pass")
	charges := func() int { return len(f.base.nmi.ledger("")) }
	attemptStatus := func(c *customer) string {
		var status string
		require.NoError(t, f.base.pool.QueryRow(t.Context(), f.q(`SELECT status FROM billing.checkout_attempts
			WHERE customer_id = $1 ORDER BY created_at LIMIT 1`), c.id).Scan(&status))
		return status
	}
	c := a.newCustomer()
	session, err := c.sell(embedded, order{price: price.ID})
	require.NoError(t, err)
	option := session.optionAt(a.server.URL, "nmi")
	before := charges()

	// Attempt 1. Replica a is inside its NMI vault call when its lease lapses.
	pay1 := order{token: f.base.nmi.Tokenize(visa)}
	vault := f.hold("nmi", func(r *http.Request) bool {
		return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v5/customers")
	}, true)
	owner := make(chan error, 1)
	go func() {
		_, err := session.payAt(context.WithoutCancel(t.Context()), a.server.URL, option, c, pay1)
		owner <- err
	}()
	require.Equal(t, a, vault.wait())
	_, err = session.payAt(t.Context(), b.server.URL, option, c, pay1)
	requireStatus(t, err, http.StatusConflict) // a page never moves on while the owner works

	f.lapseCheckoutClaims(c.id)
	f.base.nmi.DeclineValidations(1)
	refused, err := session.payAt(t.Context(), b.server.URL, option, c, pay1) // reclaims; its card verification is declined
	require.NoError(t, err)
	require.Equal(t, "failed", refused.Status, "%+v", refused.CheckoutSessionPayResult)
	require.Equal(t, "failed", attemptStatus(c), "a definite refusal fails the attempt")

	// The owner resumes after its vault call and must not charge the attempt
	// the other request failed.
	vault.release()
	t.Logf("superseded owner: %v", <-owner)
	f.settle()
	require.Equal(t, before, charges(), "the superseded owner never charges")

	// The session moved on to attempt 2, which charges once.
	s2, err := session.payAt(t.Context(), b.server.URL, option, c, order{token: f.base.nmi.Tokenize(visa)})
	require.NoError(t, err)
	require.Equal(t, "succeeded", s2.Status, "%+v", s2.CheckoutSessionPayResult)
	f.settle()
	require.Equal(t, before+1, charges())

	// A stale pay with attempt 1's card answers the paid attempt.
	for _, r := range []*world{a, b} {
		again, err := session.payAt(t.Context(), r.server.URL, option, c, pay1)
		require.NoError(t, err)
		require.Equal(t, "succeeded", again.Status)
		require.Equal(t, s2.PaymentID, again.PaymentID)
	}
	f.settle()
	require.Equal(t, before+1, charges(), "a stale pay never charges after the session moved on")

	// A declined card fails its attempt at once, on either replica.
	d := b.newCustomer()
	declined, err := d.sell(embedded, order{price: price.ID})
	require.NoError(t, err)
	refusal := order{token: f.base.nmi.Tokenize(card{Brand: "visa", Last4: "0002", Decline: "202"})}
	for _, r := range []*world{b, a, b} {
		out, err := declined.payAt(t.Context(), r.server.URL, declined.optionAt(r.server.URL, "nmi"), d, refusal)
		require.NoError(t, err)
		require.Equal(t, "failed", out.Status, "%+v", out.CheckoutSessionPayResult)
		require.NotNil(t, out.Failure, "a decline explains itself")
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

// #1099: an owner frozen past its lease before it creates its attempt cannot
// create or confirm it once another request reclaimed the key: every
// transaction it opens proves the claim first. Here the reclaiming pay's new
// card is refused and fails the attempt, the session moves on, and the frozen
// owner then wakes with an approvable card.
func TestReplicasCheckoutFrozenOwnerRefusedAtCommit(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2)
	a, b := f.replicas[0], f.replicas[1]
	price := a.membership("content:members", 9_990_000)
	charges := func() int { return len(f.base.nmi.Ledger("")) }
	subscriptions := func(c *customer) int {
		subs, err := b.client[embedded].ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: c.customerID()})
		require.NoError(t, err)
		return len(subs.Items)
	}
	c := a.newCustomer()
	session, err := c.sell(embedded, order{price: price.ID})
	require.NoError(t, err)
	option := session.optionAt(a.server.URL, "nmi")
	before := charges()

	pay1 := order{token: f.base.nmi.Tokenize(visa)}
	vault := f.hold("nmi", func(r *http.Request) bool {
		return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v5/customers")
	}, true)
	owner := make(chan error, 1)
	go func() {
		_, err := session.payAt(context.WithoutCancel(t.Context()), a.server.URL, option, c, pay1)
		owner <- err
	}()
	require.Equal(t, a, vault.wait()) // frozen before its attempt exists

	f.lapseCheckoutClaims(c.id)
	f.base.nmi.DeclineValidations(1)
	refused, err := session.payAt(t.Context(), b.server.URL, option, c, pay1) // reclaims; the card save is refused
	require.NoError(t, err)
	require.Equal(t, "failed", refused.Status, "%+v", refused.CheckoutSessionPayResult)

	// The session moves on; the frozen owner wakes and must not commit anything.
	vault.release()
	t.Logf("frozen owner: %v", <-owner)
	f.settle()
	require.Equal(t, before, charges(), "the frozen owner never charges")
	require.Zero(t, subscriptions(c), "nor enrolls")
	var attempts int
	require.NoError(t, f.base.pool.QueryRow(t.Context(), f.q(`SELECT count(*) FROM billing.checkout_attempts WHERE customer_id = $1`), c.id).Scan(&attempts))
	require.Zero(t, attempts, "nor creates its attempt")

	s2, err := session.payAt(t.Context(), b.server.URL, option, c, order{token: f.base.nmi.Tokenize(visa)})
	require.NoError(t, err)
	require.Equal(t, "succeeded", s2.Status, "%+v", s2.CheckoutSessionPayResult)
	f.settle()
	require.Equal(t, before+1, charges(), "the attempt the session moved to charges once")
	require.Equal(t, 1, subscriptions(c))
	require.Empty(t, f.base.nmi.Unexpected())
}
