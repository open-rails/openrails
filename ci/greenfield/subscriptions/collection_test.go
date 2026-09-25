//go:build greenfield && integration

package subscriptions_test

import (
	"context"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/failpoint"
)

// Exactly-once collection (#1092): one NMI order per obligation, a resend
// only on a clean vault-wide absence, dup_seconds as NMI's backstop, and
// failpoint interleavings across replicas.

// A lost NMI submission is not re-sent while the vault shows any transaction
// since its fence, even one under another order: it may be this charge.
func TestEngineNMILostSubmissionVaultActivity(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	end := e.periodEnd()
	e.toPeriodEnd()
	vault := w.nmi.lastSale().Vault
	w.nmi.loseSubmissions(1)
	w.runRenewals()
	w.nmi.approveUnder("another-order", vault, "1.00")
	w.until(func() bool { return len(w.openFindings("life.submission.unresolved")) == 1 }, "the unexplained vault charge is an operator finding")
	for range 6 {
		w.advance(time.Hour)
		w.wake()
	}
	require.Equal(t, 1, w.lostSubmissions("nmi"), "the lost submission was never re-sent")
	require.False(t, w.subscription(embedded, e.sub).CurrentPeriodEndsAt.After(end))
}

// An original the Query API has not indexed yet looks lost; the resend under
// the same order carries dup_seconds back to the fence, so NMI refuses it.
// Once the original is searchable it pays the period: one charge.
func TestEngineNMIResendDupSecondsBackstop(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	end := e.periodEnd()
	e.toPeriodEnd()
	before := len(w.nmi.saleOrders())
	w.nmi.dropResponses(1)
	w.nmi.hideSales(1)
	w.runRenewals()
	w.until(func() bool { return len(w.nmi.saleOrders()) == before+2 }, "the apparently lost submission is re-sent")
	w.until(func() bool { return len(w.openFindings("life.submission.unresolved")) == 1 }, "the refused resend is an operator finding")
	orders := w.nmi.saleOrders()[before:]
	require.Equal(t, orders[0], orders[1], "the resend reuses the obligation's order")
	require.NotEmpty(t, w.nmi.lastAttempt().Get("dup_seconds"), "the resend sets NMI's duplicate window")
	require.Len(t, e.providerLedger(), 2, "the resend charged nothing")

	w.nmi.reveal()
	w.until(func() bool { return w.subscription(embedded, e.sub).CurrentPeriodEndsAt.After(end) }, "the indexed original pays the period")
	require.Len(t, e.providerLedger(), 2, "exactly one renewal charge")
	require.Len(t, completed(w.payments(embedded, e.c.id)), 2)
	require.Empty(t, w.openFindings("life.submission.unresolved"))
}

// Every attempt for one period shares its NMI order, so a later attempt
// finds an earlier attempt's charge before sending anything.
func TestEngineNMISharedOrderFindsEarlierCharge(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	end := e.periodEnd()
	e.toPeriodEnd()
	w.nmi.setDecline(visa.Last4, "202")
	w.runRenewals()
	w.until(func() bool { return w.subscription(embedded, e.sub).Status == "past_due" }, "the first attempt is declined")
	declined := w.nmi.lastDeclined()
	require.NotNil(t, declined)
	w.nmi.setDecline(visa.Last4, "")
	// The processor approved the period under the same order after all.
	paid := w.nmi.approveUnder(declined.OrderID, declined.Vault, declined.Amount)
	attempts := w.nmi.saleAttempts()

	sub := w.subscription(embedded, e.sub)
	require.NotNil(t, sub.NextRetryAt)
	w.advance(sub.NextRetryAt.Sub(w.clock.Now()) + time.Second)
	w.runRenewals()
	w.until(func() bool { return w.subscription(embedded, e.sub).CurrentPeriodEndsAt.After(end) }, "the retry completes from the earlier charge")
	require.Equal(t, attempts, w.nmi.saleAttempts(), "the retry sent nothing")
	require.Len(t, e.providerLedger(), 2)
	var ids []string
	for _, p := range completed(w.payments(embedded, e.c.id)) {
		ids = append(ids, p.TransactionID)
	}
	require.Contains(t, ids, paid.TransactionID)
	require.Len(t, ids, 2)
}

// Replica A reaches a failpoint in a renewal and dies there; replica B takes
// the renewal over. Whatever A had done, the period is charged exactly once.
func TestReplicasFailpointInterleavings(t *testing.T) {
	t.Parallel()
	cases := map[failpoint.Point][]string{
		failpoint.AfterFence:     rails,
		failpoint.BeforeProvider: {"nmi"},
		failpoint.AfterProvider:  rails,
		failpoint.BeforeComplete: {"nmi"},
	}
	for point, railsAt := range cases {
		for _, rail := range railsAt {
			t.Run(string(point)+"/"+rail, func(t *testing.T) {
				t.Parallel()
				f := newFleet(t, 2)
				e := enroll(t, f.replicas[0], rail, embedded)
				end := f.periodEnd(e)
				f.toDue(e)
				p := pauseAt(t, point, uuid.MustParse(subUUID(e.sub)))
				f.startPasses()
				p.wait(t)
				a := f.runningReplica()
				f.crash(a)
				p.release()
				f.recover()
				f.passes()
				f.until(func() bool { return f.periodEnd(e).After(end) }, "replica B completes the renewal")
				f.advance(time.Hour)
				f.wake()
				f.passes()
				f.requireExactlyOnce(e, 1, -1)
				require.True(t, end.Add(monthHours*time.Hour).Equal(f.periodEnd(e)), "exactly one period added")
				f.revive(a)
				f.passes()
				f.requireExactlyOnce(e, 1, -1)
			})
		}
	}
}

// Replica A stalls between its fence and the provider call; its lease
// expires and another executor claims the renewal. A re-checks its claim
// immediately before the provider call and sends nothing; the renewal is
// charged once, by the executor that holds it.
func TestReplicasStalledExecutorSendsNothing(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t, 2)
			e := enroll(t, f.replicas[0], rail, embedded)
			end := f.periodEnd(e)
			f.toDue(e)
			sub := uuid.MustParse(subUUID(e.sub))
			var once sync.Once
			stalled := make(chan struct{})
			remove := failpoint.Set(func(ctx context.Context, s failpoint.Site) error {
				if s.Point != failpoint.BeforeProvider || s.Subscription != sub || s.Kind != "subscription_collection" {
					return nil
				}
				once.Do(func() {
					// The stall outlives the lease; another executor claims
					// the row exactly as ClaimRailIntentByID does.
					_, err := f.base.pool.Exec(context.Background(), f.q(`UPDATE openrails.rail_intents
						SET attempts = attempts + 1, claimed_until = $2 WHERE id = $1 AND status = 'in_flight'`),
						s.Operation, f.base.clock.Now().Add(2*time.Minute))
					if err != nil {
						t.Errorf("take over the claim: %v", err)
					}
					close(stalled)
				})
				return nil
			})
			defer remove()
			before := f.submissions(e)
			f.startPasses()
			select {
			case <-stalled:
			case <-time.After(30 * time.Second):
				t.Fatal("the renewal never reached the provider call")
			}
			f.until(func() bool { return f.periodEnd(e).After(end) }, "the holding executor completes the renewal")
			f.advance(time.Hour)
			f.wake()
			f.passes()
			require.Equal(t, before+1, f.submissions(e), "the stalled executor sent nothing; one charge request in all")
			f.requireExactlyOnce(e, 1, -1)
		})
	}
}

// paused holds the first hit of a failpoint for one subscription's renewal
// until release or until its executor's context ends.
type paused struct {
	arrived, released chan struct{}
	hit, done         sync.Once
}

func pauseAt(t *testing.T, point failpoint.Point, sub uuid.UUID) *paused {
	p := &paused{arrived: make(chan struct{}), released: make(chan struct{})}
	remove := failpoint.Set(func(ctx context.Context, s failpoint.Site) error {
		if s.Point != point || s.Subscription != sub || s.Kind != "subscription_collection" {
			return nil
		}
		first := false
		p.hit.Do(func() { first = true; close(p.arrived) })
		if !first {
			return nil
		}
		select {
		case <-p.released:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	t.Cleanup(func() { p.release(); remove() })
	return p
}

func (p *paused) wait(t *testing.T) {
	t.Helper()
	select {
	case <-p.arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the renewal never reached the failpoint")
	}
}

func (p *paused) release() { p.done.Do(func() { close(p.released) }) }

// runningReplica is the replica executing the only running operation job.
func (f *fleet) runningReplica() *world {
	f.t.Helper()
	var id string
	require.NoError(f.t, f.base.pool.QueryRow(f.t.Context(), f.q(`SELECT attempted_by[array_length(attempted_by, 1)] FROM openrails.river_job
		WHERE state = 'running' AND split_part(kind, '.', 2) IN ('provider_operation', 'dunning') ORDER BY split_part(kind, '.', 2) = 'provider_operation' DESC, attempted_at DESC LIMIT 1`)).Scan(&id))
	for _, r := range f.live() {
		if r.replica.id == id {
			return r
		}
	}
	f.t.Fatalf("no live replica runs %s", id)
	return nil
}

// lastAttempt is the form of the latest sale request.
func (f *nmiFake) lastAttempt() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[len(f.attempts)-1]
}
