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
	"github.com/open-rails/openrails/internal/config"
)

// An order's payment claims its key in PostgreSQL. Replicas racing one pay
// charge once, and a replica that dies after its provider charge left the
// claim processing: the claim lapses, a pay on another replica reclaims it
// and resumes the same sale operation, never charging again.
func TestReplicasPurchaseIdempotency(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			// Sixteen pays of one order from one address are past its rate
			// limits, which both replicas count together.
			f := startFleet(t, 2, false, nil, func(w *world) {
				w.cfg = func(c *config.Config) { c.RateLimitsDisabled = true }
			})
			a, b := f.replicas[0], f.replicas[1]
			price := a.permanent("content:post")
			charges := func() int {
				if rail == "stripe" {
					return len(f.base.stripe.ledger(""))
				}
				return len(f.base.nmi.ledger(""))
			}
			open := func(c *customer) (sale, order) {
				method := c.saveCard(rail, visa)
				session, err := c.sell(embedded, order{price: price.ID})
				require.NoError(t, err)
				return session, order{method: method}
			}

			// Sixteen concurrent pays of one order under one key, over both
			// replicas.
			racer := a.newCustomer()
			session, pay := open(racer)
			before := charges()
			start := make(chan struct{})
			paid := make([]*paidOrder, 16)
			errs := make([]error, 16)
			var wg sync.WaitGroup
			for i := range 16 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					paid[i], errs[i] = session.payAt(t.Context(), f.replicas[i%2].server.URL, racer, pay)
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
				require.Equal(t, "succeeded", paid[i].Status, "%+v", paid[i].Order)
				if payment == nil {
					payment = paid[i].PaymentID
				}
				require.Equal(t, payment, paid[i].PaymentID, "one payment for the order")
			}
			require.NotNil(t, payment, "one pay ran")
			f.settle()
			require.Equal(t, before+1, charges(), "one provider charge for the order")
			replay, err := session.payAt(t.Context(), b.server.URL, racer, pay)
			require.NoError(t, err)
			require.Equal(t, "succeeded", replay.Status)
			require.Equal(t, payment, replay.PaymentID)
			require.True(t, racer.entitled("content:post"))

			// Replica a dies after the provider charged, before it recorded the
			// answer or completed its claim.
			victim := a.newCustomer()
			session, pay = open(victim)
			before, attempts := charges(), f.submissionCount(rail)
			h := f.hold(rail, submission(rail), true)
			ctx, die := context.WithCancel(t.Context())
			go func() { _, _ = session.payAt(ctx, a.server.URL, victim, pay) }()
			require.Equal(t, a, h.wait())
			f.crash(a) // nothing it does from here is recorded
			die()      // its request dies with it; the provider already took the charge
			h.release()
			require.Eventually(t, func() bool { return charges() == before+1 }, 10*time.Second, 10*time.Millisecond, "the provider charged")
			f.unhold()

			_, err = session.payAt(t.Context(), b.server.URL, victim, pay)
			var status *billing.StatusError
			require.True(t, errors.As(err, &status), "the dead replica's claim holds until its lease lapses: %v", err)
			require.Equal(t, http.StatusConflict, status.Status)

			// The customer pays again: once the dead replica's claim lapses, a
			// survivor reclaims the key and resolves the sale.
			f.lapseOrderClaims(session)
			f.any().rescue()
			f.wake()
			f.settle()
			retried, err := session.payAt(t.Context(), b.server.URL, victim, pay)
			require.NoError(t, err, "a survivor reclaims the key and resolves the sale")
			for range 8 {
				if retried.Status == "succeeded" {
					break
				}
				f.advance(time.Minute)
				f.wake()
				f.settle()
				retried = sale{w: b, order: session.order}.readAt(b.server.URL, victim)
			}
			require.Equal(t, "succeeded", retried.Status, "%v", retried)
			require.Equal(t, before+1, charges(), "the reclaimed request never charges again")
			require.Equal(t, attempts+1, f.submissionCount(rail), "one provider submission")
			owned, err := heldKeys(t.Context(), b.client[embedded], victim.customerID(), time.Time{}, "content:post")
			require.NoError(t, err)
			require.True(t, owned["content:post"])
			var claims int64
			require.NoError(t, f.base.pool.QueryRow(t.Context(), f.q(`SELECT claims FROM billing.idempotency_keys
				WHERE operation = 'order_pay' AND idempotency_key LIKE $1`), session.order.ID.UUID().String()+":%").Scan(&claims))
			require.EqualValues(t, 2, claims, "the dead replica's claim was reclaimed once")
			require.Empty(t, f.base.stripe.Unexpected())
			require.Empty(t, f.base.nmi.Unexpected())
		})
	}
}

// lapseOrderClaims ends the processing pay claims on an order on the
// database clock, as a dead owner's silence or starved renewals would.
func (f *fleet) lapseOrderClaims(s sale) {
	f.t.Helper()
	_, err := f.base.pool.Exec(f.t.Context(), f.q(`UPDATE billing.idempotency_keys SET lease_expires_at = now() - interval '1 millisecond'
		WHERE operation = 'order_pay' AND idempotency_key LIKE $1 AND status = 'processing'`), s.order.ID.UUID().String()+":%")
	require.NoError(f.t, err)
}

func requireStatus(t *testing.T, err error, want int) *billing.StatusError {
	t.Helper()
	var status *billing.StatusError
	require.True(t, errors.As(err, &status), "%v", err)
	require.Equal(t, want, status.Status, "%v", err)
	return status
}

// readingNewCard holds the first read of a new Stripe card: a pay's step
// before its attempt exists.
func readingNewCard(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/payment_methods/")
}

// Lease-lapse interleavings, each asserting the provider's own journal: an
// owner whose lease lapses while it is still alive (reading its new card)
// never charges a key another request settled; a stale pay replays its own
// answer and never runs again; a declined card fails at once.
func TestReplicasPurchaseLeaseLapse(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2)
	a, b := f.replicas[0], f.replicas[1]
	price := a.permanent("content:pass")
	charges := func() int { return len(f.base.stripe.ledger("")) }
	c := a.newCustomer()
	session, err := c.sell(embedded, order{price: price.ID})
	require.NoError(t, err)
	before := charges()

	// Replica a is reading its new card at Stripe when its lease lapses.
	token := a.newCardToken("stripe", visa)
	pay1 := order{rail: "stripe", token: token}
	read := f.hold("stripe", readingNewCard, true)
	owner := make(chan error, 1)
	go func() {
		_, err := session.payAt(context.WithoutCancel(t.Context()), a.server.URL, c, pay1)
		owner <- err
	}()
	require.Equal(t, a, read.wait())
	_, err = session.payAt(t.Context(), b.server.URL, c, pay1)
	requireStatus(t, err, http.StatusConflict) // a page never moves on while the owner works

	f.lapseOrderClaims(session)
	f.base.stripe.SetMethodDecline(token, "generic_decline")
	refused, err := session.payAt(t.Context(), b.server.URL, c, pay1) // reclaims; the card is declined
	require.NoError(t, err)
	require.Equal(t, "failed", refused.Status, "%+v", refused.Order)
	f.base.stripe.SetMethodDecline(token, "")

	// The owner resumes with an approvable card and must not charge the key
	// the other request settled.
	read.release()
	t.Logf("superseded owner: %v", <-owner)
	f.settle()
	require.Equal(t, before, charges(), "the superseded owner never charges")

	// Another card pays under its own key, once.
	s2, err := session.payAt(t.Context(), b.server.URL, c, order{rail: "stripe", token: a.newCardToken("stripe", visa)})
	require.NoError(t, err)
	require.Equal(t, "succeeded", s2.Status, "%+v", s2.Order)
	f.settle()
	require.Equal(t, before+1, charges())

	// A stale pay with the first card replays its decline.
	for _, r := range []*world{a, b} {
		again, err := session.payAt(t.Context(), r.server.URL, c, pay1)
		require.NoError(t, err)
		require.Equal(t, "failed", again.Status)
	}
	f.settle()
	require.Equal(t, before+1, charges(), "a stale pay never charges")

	// A declined card fails at once, on either replica.
	d := b.newCustomer()
	declined, err := d.sell(embedded, order{price: price.ID})
	require.NoError(t, err)
	refusal := order{rail: "nmi", token: f.base.nmi.Tokenize(card{Brand: "visa", Last4: "0002", Decline: "202"})}
	for _, r := range []*world{b, a, b} {
		out, err := declined.payAt(t.Context(), r.server.URL, d, refusal)
		require.NoError(t, err)
		require.Equal(t, "failed", out.Status, "%+v", out.Order)
		require.NotNil(t, out.Failure, "a decline explains itself")
	}
	f.settle()
	require.Empty(t, f.base.nmi.ledger(""))
	require.Empty(t, f.base.stripe.Unexpected())
	require.Empty(t, f.base.nmi.Unexpected())
}

func (f *fleet) submissionCount(rail string) int {
	if rail == "stripe" {
		return f.base.stripe.Attempts("")
	}
	return len(f.base.nmi.Attempts())
}

// An owner frozen past its lease before it creates its attempt cannot create
// or confirm it once another request reclaimed the key: every transaction it
// opens proves the claim first. Here the reclaiming pay's card is declined,
// and the frozen owner then wakes with an approvable card.
func TestReplicasPurchaseFrozenOwnerRefusedAtCommit(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2)
	a, b := f.replicas[0], f.replicas[1]
	price := a.membership("content:members", 9_990_000)
	charges := func() int { return len(f.base.stripe.ledger("")) }
	subscriptions := func(c *customer) int {
		subs, err := b.client[embedded].ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: c.customerID()})
		require.NoError(t, err)
		return len(subs.Items)
	}
	c := a.newCustomer()
	session, err := c.sell(embedded, order{price: price.ID})
	require.NoError(t, err)
	before := charges()

	token := a.newCardToken("stripe", visa)
	pay1 := order{rail: "stripe", token: token}
	read := f.hold("stripe", readingNewCard, true)
	owner := make(chan error, 1)
	go func() {
		_, err := session.payAt(context.WithoutCancel(t.Context()), a.server.URL, c, pay1)
		owner <- err
	}()
	require.Equal(t, a, read.wait()) // frozen before its attempt exists

	f.lapseOrderClaims(session)
	f.base.stripe.SetMethodDecline(token, "generic_decline")
	refused, err := session.payAt(t.Context(), b.server.URL, c, pay1) // reclaims; the card is declined
	require.NoError(t, err)
	require.Equal(t, "failed", refused.Status, "%+v", refused.Order)
	f.base.stripe.SetMethodDecline(token, "")

	// The frozen owner wakes and must not commit anything.
	read.release()
	t.Logf("frozen owner: %v", <-owner)
	f.settle()
	require.Equal(t, before, charges(), "the frozen owner never charges")
	require.Zero(t, subscriptions(c), "nor enrolls")
	var attempts int
	require.NoError(t, f.base.pool.QueryRow(t.Context(), f.q(`SELECT count(*) FROM billing.order_attempts WHERE customer_id = $1 AND status <> 'failed'`), c.id).Scan(&attempts))
	require.Zero(t, attempts, "nor leaves an attempt of its own")

	s2, err := session.payAt(t.Context(), b.server.URL, c, order{rail: "stripe", token: a.newCardToken("stripe", visa)})
	require.NoError(t, err)
	require.Equal(t, "succeeded", s2.Status, "%+v", s2.Order)
	f.settle()
	require.Equal(t, before+1, charges(), "the next key charges once")
	require.Equal(t, 1, subscriptions(c))
	require.Empty(t, f.base.stripe.Unexpected())
}
