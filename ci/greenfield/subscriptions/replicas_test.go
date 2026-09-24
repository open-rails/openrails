//go:build greenfield && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// Multi-replica exactly-once rebilling (#1075). Every scenario runs N
// embedded replicas over one database and asserts, from the providers' own
// journals, one charge and one local payment per membership period.

// Scenario 1: every replica runs its due pass at once over many memberships
// that fell due together; each is renewed once, none twice, none lost.
func TestReplicasManyDueAtOnce(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 3)
	var cases []*engineCase
	for i := range 6 {
		cases = append(cases, enroll(t, f.replicas[i%3], rails[i%2], embedded))
	}
	for period := 2; period <= 3; period++ {
		f.toDue(cases...)
		f.passes()
		f.passes() // a second round in the same period admits nothing
		for _, e := range cases {
			f.requireExactlyOnce(e, period-1, period)
		}
	}
	// A remote Client host reading through another replica sees the same book.
	sub, err := f.replicas[2].client[remote].GetSubscription(t.Context(), cases[0].sub)
	require.NoError(t, err)
	require.Equal(t, "active", sub.Status)
	require.Empty(t, f.base.stripe.unexpected())
	require.Empty(t, f.base.nmi.unexpected())
}

// Scenario 2: two replicas admit the same due renewal at the same instant,
// forced by holding the customer's spend lock until both are waiting on it.
// One admits; the other finds the accepted operation and does nothing.
func TestReplicasAdmissionRace(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t, 2)
			e := enroll(t, f.replicas[0], rail, embedded)
			f.toDue(e)
			lock := f.lockCustomer(e)
			passes := f.startPasses()
			lock.awaitWaiters(f.replicas...)
			lock.release()
			f.awaitPasses(passes)
			f.requireExactlyOnce(e, 1, 2)
			require.Len(t, f.collections(e), 1, "one accepted renewal operation")
			f.requireDatabaseRefusesSecondWriter(e)
		})
	}
}

// requireDatabaseRefusesSecondWriter: whatever an application path does,
// PostgreSQL itself refuses a second operation for a retry slot and a second
// unresolved operation for a subscription.
func (f *fleet) requireDatabaseRefusesSecondWriter(e *engineCase) {
	t := f.t
	clone := func(attempt int, status string) error {
		payload := "payload"
		if attempt >= 0 {
			payload = fmt.Sprintf("jsonb_set(payload, '{attempt}', '%d'::jsonb)", attempt)
		}
		_, err := f.base.pool.Exec(t.Context(), f.q(`INSERT INTO openrails.rail_intents (merchant_id, rail, intent_type, subscription_id, price_id, payload, idempotency_key, status, origin, psp_id)
			SELECT merchant_id, rail, intent_type, subscription_id, price_id, `+payload+`, idempotency_key || ':' || gen_random_uuid()::text, $2, origin, psp_id
			FROM openrails.rail_intents WHERE subscription_id = $1 AND intent_type = 'subscription_collection' AND status = 'succeeded'`), subUUID(e.sub), status)
		return err
	}
	refused := func(err error, constraint string) {
		t.Helper()
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		require.Equal(t, "23505", pgErr.Code)
		require.Equal(t, constraint, pgErr.ConstraintName)
	}
	refused(clone(-1, "failed_terminal"), "uq_rail_intents_subscription_collection_slot")
	require.NoError(t, clone(98, "pending"))
	refused(clone(99, "pending"), "uq_rail_intents_open_subscription_collection")
	_, err := f.base.pool.Exec(t.Context(), f.q(`DELETE FROM openrails.rail_intents WHERE subscription_id = $1 AND status = 'pending'`), subUUID(e.sub))
	require.NoError(t, err)
}

// Scenario 2b: a pass reads a membership as due, then another replica
// settles it (here the member pays on another replica) before the pass
// reaches it. The pass does nothing and raises no operator finding.
func TestReplicasSettledBeforeAdmission(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t, 2)
			a, b := f.replicas[0], f.replicas[1]
			late := enroll(t, a, rail, embedded)
			f.advance(day)
			first := enroll(t, a, rail, embedded)
			late.setDecline(visa.Last4, "insufficient_funds", "202")
			f.toDue(late)
			f.passes()
			retry := f.subscription(late)
			require.Equal(t, "past_due", retry.Status)
			late.setDecline(visa.Last4, "", "") // the member's bank now approves
			// first falls due before late's retry, so a pass reaches it first.
			require.True(t, f.periodEnd(first).Before(*retry.NextRetryAt))
			lock := f.lockCustomer(first)
			f.advance(retry.NextRetryAt.Sub(f.base.clock.Now()) + time.Second)
			passes := f.startPasses(b)
			lock.awaitWaiters(b) // b has read both as due and waits on the first
			paid := unwrap(late.c.must(http.MethodPost, "/subscriptions/"+late.sub.String()+"/retry-now", "retry-"+uuid.NewString(), map[string]any{}))
			t.Logf("member retry: %v", paid)
			f.until(func() bool { return f.subscription(late).Status == "active" }, "the member's payment settles the late membership")
			lock.release()
			f.awaitPasses(passes)
			f.passes()
			f.requireExactlyOnce(first, 1, 2)
			f.requireExactlyOnce(late, 1, 3)
		})
	}
}

// crashPoint arranges one replica's death at a point in a renewal and
// returns the replica it killed.
type crashPoint struct {
	name  string
	rails []string
	// submissions is the provider charge requests after recovery.
	submissions int
	kill        func(f *fleet, e *engineCase) *world
}

// Scenario 3: the replica running a renewal dies at each point; a survivor
// takes it over and the membership is charged exactly once. A submission
// lost before the provider is re-sent once, only after the provider's read
// proves it never executed.
func TestReplicasCrashMidRenewal(t *testing.T) {
	t.Parallel()
	points := []crashPoint{
		{name: "before_operation_persisted", rails: rails, submissions: 2, kill: func(f *fleet, e *engineCase) *world {
			lock := f.lockCustomer(e)
			a := f.replicas[0]
			f.startPasses(a)
			lock.awaitWaiters(a)
			f.crash(a) // its admission transaction dies uncommitted
			require.Empty(f.t, f.collections(e))
			lock.release()
			return a
		}},
		// Stripe's first request of a renewal is its submission, made only
		// after the fence; NMI first reads the vault.
		{name: "persisted_before_submit", rails: []string{"nmi"}, submissions: 2, kill: func(f *fleet, e *engineCase) *world {
			vault := f.providerCustomers(e)[0]
			h := f.hold("nmi", func(r *http.Request) bool {
				return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v5/customers/"+vault)
			}, false)
			f.startPasses()
			r := h.wait()
			f.crash(r)
			h.release()
			ops := f.collections(e)
			require.Len(f.t, ops, 1)
			require.NotContains(f.t, ops[0].Evidence, "submitted_at", "the dead executor never fenced a submission")
			return r
		}},
		{name: "submitted_before_receipt", rails: rails, submissions: 2, kill: func(f *fleet, e *engineCase) *world {
			h := f.hold(e.rail, submission(e.rail), true)
			f.startPasses()
			r := h.wait()
			f.crash(r) // the provider executes the charge; its answer is lost
			h.release()
			require.Eventually(f.t, func() bool { return len(f.charges(e)) == 2 }, 10*time.Second, 10*time.Millisecond, "the provider charged")
			return r
		}},
		{name: "lost_before_provider", rails: rails, submissions: 2, kill: func(f *fleet, e *engineCase) *world {
			h := f.hold(e.rail, submission(e.rail), false)
			f.startPasses()
			r := h.wait()
			f.crash(r) // the request dies with the process
			h.release()
			require.Len(f.t, f.charges(e), 1)
			return r
		}},
		{name: "receipt_before_finalize", rails: rails, submissions: 2, kill: func(f *fleet, e *engineCase) *world {
			h := f.hold(e.rail, receiptRead(f, e), false)
			f.startPasses()
			r := h.wait()
			f.crash(r)
			h.release()
			ops := f.collections(e)
			require.Len(f.t, ops, 1)
			require.Equal(f.t, "in_flight", ops[0].Status, "the charge is not finalized")
			require.Contains(f.t, ops[0].Evidence, "collection_candidate", "the dead executor held the provider's answer")
			return r
		}},
	}
	for _, point := range points {
		for _, rail := range point.rails {
			t.Run(point.name+"/"+rail, func(t *testing.T) {
				t.Parallel()
				f := newFleet(t, 2)
				e := enroll(t, f.replicas[0], rail, embedded)
				end := f.periodEnd(e)
				f.toDue(e)
				dead := point.kill(f, e)
				f.unhold()
				f.recover()
				f.passes()
				f.until(func() bool { return f.periodEnd(e).After(end) }, "a survivor completes the renewal")
				f.advance(time.Hour)
				f.wake()
				f.passes()
				f.requireExactlyOnce(e, 1, point.submissions)
				require.True(t, end.Add(monthHours*time.Hour).Equal(f.periodEnd(e)), "exactly one period added")
				// The dead replica's next generation joins without re-running anything.
				f.revive(dead)
				f.passes()
				f.requireExactlyOnce(e, 1, point.submissions)
			})
		}
	}
}

func submission(rail string) func(*http.Request) bool {
	if rail == "stripe" {
		return func(r *http.Request) bool { return r.Method == http.MethodPost && r.URL.Path == "/v1/payment_intents" }
	}
	return func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/transact.php") }
}

// receiptRead matches the executor's provider read of a renewal charge that
// has already been approved: the replica holds the answer, not yet recorded.
func receiptRead(f *fleet, e *engineCase) func(*http.Request) bool {
	ref := f.providerCustomers(e)[0]
	if e.rail == "stripe" {
		fk := f.base.stripe
		return func(r *http.Request) bool {
			if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/v1/payment_intents/") {
				return false
			}
			pi := fk.intents[strings.TrimPrefix(r.URL.Path, "/v1/payment_intents/")]
			if pi == nil || pi["customer"] != ref || pi["status"] != "succeeded" {
				return false
			}
			paid := 0
			for _, id := range fk.order {
				if fk.intents[id]["customer"] == ref && fk.intents[id]["status"] == "succeeded" {
					paid++
				}
			}
			return paid >= 2
		}
	}
	fk := f.base.nmi
	return func(r *http.Request) bool {
		read := strings.HasSuffix(r.URL.Path, "/query.php") || (r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v5/"))
		return read && fk.approvedLocked(ref) >= 2
	}
}

// Scenario 4: the River leader dies while its due pass is mid-run. Another
// replica is elected and its own schedule (the due pass runs when a replica
// takes leadership) renews every membership, exactly once, with no manual
// pass: nothing stalls behind the dead leader.
func TestReplicasLeaderCrash(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 3)
	var cases []*engineCase
	for i := range 4 {
		cases = append(cases, enroll(t, f.replicas[i%3], rails[i%2], embedded))
	}
	f.toDue(cases...)
	leader := f.awaitLeader(nil)
	lock := f.lockCustomer(cases[0])
	f.startPasses(leader)
	lock.awaitWaiters(leader)
	f.crash(leader)
	lock.release()
	require.NotEqual(t, leader, f.awaitLeader(leader))
	f.recover()
	f.until(func() bool {
		for _, e := range cases {
			if len(f.charges(e)) < 2 {
				return false
			}
		}
		return true
	}, "the new leader's schedule renews every membership")
	f.passes()
	for _, e := range cases {
		f.requireExactlyOnce(e, 1, 2)
	}
}

// Scenario 5: replica clocks disagree by ±30s. Only the replica already past
// the boundary admits; the others neither admit early nor again, and an
// executor whose clock is behind the admission waits for it.
func TestReplicasClockSkew(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 3, -30*time.Second, 0, 30*time.Second)
	cases := []*engineCase{enroll(t, f.replicas[1], "nmi", embedded), enroll(t, f.replicas[1], "stripe", embedded)}
	end := f.periodEnd(cases[0])
	require.True(t, end.Equal(f.periodEnd(cases[1])))
	f.advance(end.Sub(f.base.clock.Now()) - 10*time.Second)
	f.passes()
	for _, e := range cases {
		require.Len(t, f.collections(e), 1, "only the replica past the boundary admitted")
	}
	f.passes()
	f.advance(2 * time.Minute)
	f.wake()
	f.passes()
	f.until(func() bool { return f.periodEnd(cases[0]).After(end) && f.periodEnd(cases[1]).After(end) }, "both renew")
	f.passes()
	for _, e := range cases {
		f.requireExactlyOnce(e, 1, 2)
		require.Len(t, f.collections(e), 1, "one operation for the period")
	}
}

// Scenario 6: the provider's notice of a renewal charge reaches one replica
// while another is finalizing that same payment. Both orders are idempotent.
func TestReplicasNoticeDuringFinalize(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t, 2)
			e := enroll(t, f.replicas[0], rail, embedded)
			end := f.periodEnd(e)
			f.toDue(e)
			h := f.hold(rail, receiptRead(f, e), false)
			f.startPasses()
			finalizer := h.wait()
			notice := f.renewalNotice(e)
			status, err := f.other(finalizer).post(rail, notice)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, status)
			h.release()
			f.settle()
			f.until(func() bool { return f.periodEnd(e).After(end) }, "the renewal finalizes")
			// Late duplicates to every replica at once change nothing.
			f.postAll(rail, notice)
			f.passes()
			f.requireExactlyOnce(e, 1, 2)
			require.True(t, end.Add(monthHours*time.Hour).Equal(f.periodEnd(e)), "exactly one period added")
		})
	}
}

// renewalNotice is the provider's own notice of the case's approved renewal.
func (f *fleet) renewalNotice(e *engineCase) obj {
	ref := f.providerCustomers(e)[0]
	if e.rail == "stripe" {
		fk := f.base.stripe
		fk.mu.Lock()
		defer fk.mu.Unlock()
		for i := len(fk.order) - 1; i >= 0; i-- {
			pi := fk.intents[fk.order[i]]
			if pi["customer"] == ref && pi["status"] == "succeeded" {
				copied := obj{}
				for k, v := range pi {
					copied[k] = v
				}
				return stripeEvent("payment_intent.succeeded", copied)
			}
		}
		f.t.Fatal("no Stripe renewal to notify")
	}
	fk := f.base.nmi
	fk.mu.Lock()
	defer fk.mu.Unlock()
	for i := len(fk.sales) - 1; i >= 0; i-- {
		s := fk.sales[i]
		if s.Vault == ref && s.Declined == "" {
			return nmiEvent("transaction.sale.success", obj{"transaction_id": s.TransactionID, "transaction_type": "cc", "condition": "pendingsettlement", "amount": s.Amount, "currency": "USD",
				"order_id": s.OrderID, "customer_vault_id": s.Vault, "action": obj{"action_type": "sale", "amount": s.Amount, "success": "1", "response_code": "100"}})
		}
	}
	f.t.Fatal("no NMI renewal to notify")
	return nil
}

// postAll delivers one notice to every live replica at the same instant.
func (f *fleet) postAll(rail string, notice obj) {
	f.t.Helper()
	start := make(chan struct{})
	var wg sync.WaitGroup
	live := f.live()
	statuses := make([]int, len(live))
	errs := make([]error, len(live))
	for i, r := range live {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			statuses[i], errs[i] = r.post(rail, notice)
		}()
	}
	close(start)
	wg.Wait()
	for i := range live {
		require.NoError(f.t, errs[i])
		require.Equal(f.t, http.StatusOK, statuses[i], "replica %s", live[i].replica.name)
	}
	f.settle()
}

// Scenario 7: one attempt per retry slot. Two replicas race the scheduled
// retry; then a new card's immediate member retry on one replica races the
// scheduled pass on the other.
func TestReplicasDunningRetryRace(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t, 2)
			a, b := f.replicas[0], f.replicas[1]
			e := enroll(t, a, rail, embedded)
			e.setDecline(visa.Last4, "insufficient_funds", "202")
			end := f.periodEnd(e)
			f.toDue(e)
			f.passes()
			sub := f.subscription(e)
			require.Equal(t, "past_due", sub.Status)
			require.NotNil(t, sub.NextRetryAt)
			require.Equal(t, 2, f.submissions(e))

			f.advance(sub.NextRetryAt.Sub(f.base.clock.Now()) + time.Second)
			lock := f.lockCustomer(e)
			passes := f.startPasses(a, b)
			lock.awaitWaiters(a, b)
			lock.release()
			f.awaitPasses(passes)
			require.Equal(t, 3, f.submissions(e), "one attempt for the first retry slot")
			require.Equal(t, "past_due", f.subscription(e).Status)

			e.replaceCard(mastercard)
			lock = f.lockCustomer(e)
			passes = f.startPasses(b)
			type answer struct {
				status int
				body   string
				err    error
			}
			member := make(chan answer, 1)
			go func() {
				status, body, err := a.retryNow(e.c, e.sub, "retry-"+uuid.NewString())
				member <- answer{status, body, err}
			}()
			lock.awaitWaiters(a, b)
			lock.release()
			got := <-member
			require.NoError(t, got.err)
			t.Logf("member retry: %d %s", got.status, got.body)
			require.Contains(t, []int{http.StatusOK, http.StatusAccepted, http.StatusConflict}, got.status, got.body)
			f.awaitPasses(passes)
			f.until(func() bool { return f.periodEnd(e).After(end) }, "the new card recovers the period")
			f.passes()
			sub = f.subscription(e)
			require.Equal(t, "active", sub.Status)
			require.True(t, end.Add(monthHours*time.Hour).Equal(*sub.CurrentPeriodEndsAt))
			f.requireExactlyOnce(e, 1, 4)
			attempts := map[string]int{}
			for _, c := range f.collections(e) {
				attempts[c.Attempt]++
			}
			require.Equal(t, map[string]int{"0": 1, "1": 1, "2": 1}, attempts, "one operation per retry slot")
		})
	}
}

// Scenario 8: a cancel (or a refund that revokes access) lands on one
// replica while another renews. Before the submission fence nothing is
// charged; after it, the one charge already sent is recorded, never lost,
// and nothing is charged afterwards.
func TestReplicasCancelRacesRenewal(t *testing.T) {
	t.Parallel()
	type race struct {
		name   string
		rails  []string
		gate   func(f *fleet, e *engineCase) (func(*http.Request) bool, bool)
		act    func(t *testing.T, f *fleet, r *world, e *engineCase)
		charge bool
	}
	preFence := func(f *fleet, e *engineCase) (func(*http.Request) bool, bool) {
		vault := f.providerCustomers(e)[0]
		return func(r *http.Request) bool {
			return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v5/customers/"+vault)
		}, false
	}
	afterFence := func(f *fleet, e *engineCase) (func(*http.Request) bool, bool) { return submission(e.rail), true }
	cancel := func(t *testing.T, f *fleet, r *world, e *engineCase) {
		require.NoError(t, r.client[remote].CancelSubscription(t.Context(), e.sub, openrails.CancelSubscriptionRequest{Reason: "member left"}))
	}
	refund := func(t *testing.T, f *fleet, r *world, e *engineCase) {
		page, err := r.client[remote].ListPayments(t.Context(), openrails.PaymentFilter{CustomerID: e.c.id, PageOptions: openrails.PageOptions{Limit: 10}})
		require.NoError(t, err)
		paid := completed(page.Data)
		require.Len(t, paid, 1)
		_, err = r.client[remote].RefundPayment(t.Context(), paid[0].ID, openrails.RefundPaymentParams{Full: true, Reason: "requested_by_customer", RevokeAccess: true, IdempotencyKey: "refund-" + paid[0].ID.String()})
		require.NoError(t, err)
	}
	races := []race{
		{name: "cancel_before_fence", rails: []string{"nmi"}, gate: preFence, act: cancel},
		{name: "refund_revoke_before_fence", rails: []string{"nmi"}, gate: preFence, act: refund},
		{name: "cancel_after_fence", rails: rails, gate: afterFence, act: cancel, charge: true},
	}
	for _, rc := range races {
		for _, rail := range rc.rails {
			t.Run(rc.name+"/"+rail, func(t *testing.T) {
				t.Parallel()
				f := newFleet(t, 2)
				e := enroll(t, f.replicas[0], rail, embedded)
				end := f.periodEnd(e)
				f.toDue(e)
				match, commit := rc.gate(f, e)
				h := f.hold(rail, match, commit)
				f.startPasses()
				renewing := h.wait()
				rc.act(t, f, f.other(renewing), e)
				h.release()
				f.settle()
				f.until(func() bool {
					for _, c := range f.collections(e) {
						if c.Status != "succeeded" && c.Status != "failed_terminal" {
							return false
						}
					}
					return true
				}, "the raced renewal resolves")
				sub := f.subscription(e)
				require.Equal(t, "cancelled", sub.Status)
				require.True(t, sub.CurrentPeriodEndsAt.Before(end.Add(monthHours*time.Hour)), "a cancelled membership is not extended")
				want := 1
				if rc.charge {
					want = 2 // sent before the cancel committed; recorded for review, never lost
				}
				require.Len(t, f.charges(e), want)
				require.Equal(t, want, f.submissions(e), "nothing is sent after the cancel")
				page, err := f.any().client[embedded].ListPayments(t.Context(), openrails.PaymentFilter{CustomerID: e.c.id, PageOptions: openrails.PageOptions{Limit: 10}})
				require.NoError(t, err)
				require.Len(t, completed(page.Data), want, "every provider charge is a local payment")
				for range 2 {
					f.advance(monthHours * time.Hour)
					f.passes()
				}
				require.Equal(t, want, f.submissions(e), "a cancelled membership is never charged again")
			})
		}
	}
}

// Scenario 9: provider-owned (imported) memberships under three replicas.
// Every replica runs due passes and convergence; OpenRails sends no charge.
// The provider's renewal notice, delivered to all replicas at once, is
// mirrored once. An NMI schedule with OpenRails dunning recovers its failed
// period with exactly one charge.
func TestReplicasProviderOwned(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t, 3)
			l := importLegacy(t, f.replicas[0], rail, embedded)
			for _, r := range f.live() {
				r.converge()
			}
			charges := l.engineCharges()
			end := l.periodEnd()
			f.advance(end.Sub(f.base.clock.Now()) + time.Hour)
			f.passes()
			f.passes()
			for _, r := range f.live() {
				r.converge()
			}
			require.Equal(t, charges, l.engineCharges(), "OpenRails never charges a provider-owned schedule")
			before := len(completed(f.replicas[0].payments(embedded, l.c.id)))
			notice := l.providerRenewal(true)
			f.postAll(rail, notice)
			renewed := l.periodEnd()
			require.True(t, renewed.After(end), "the provider's renewal is mirrored")
			require.Len(t, completed(f.replicas[0].payments(embedded, l.c.id)), before+1, "mirrored once")
			f.postAll(rail, notice)
			f.passes()
			require.True(t, l.periodEnd().Equal(renewed))
			require.Len(t, completed(f.replicas[0].payments(embedded, l.c.id)), before+1)
			require.Equal(t, charges, l.engineCharges())
			require.True(t, l.c.entitled(l.ent))
		})
	}
	t.Run("nmi/provider_dunning", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t, 3)
		l := importLegacy(t, f.replicas[0], "nmi", embedded, func(book *openrails.DeclaredBilling) {
			book.Subscriptions[0].CollectionPolicy = "provider_dunning"
		})
		f.replicas[1].converge()
		end := l.periodEnd()
		f.advance(end.Sub(f.base.clock.Now()) + time.Hour)
		f.postAll("nmi", l.providerRenewal(false))
		sub := f.replicas[2].subscription(embedded, l.sub)
		require.Equal(t, "past_due", sub.Status)
		require.NotNil(t, sub.NextRetryAt)
		f.advance(sub.NextRetryAt.Sub(f.base.clock.Now()) + time.Second)
		f.passes()
		f.passes()
		f.until(func() bool { return l.periodEnd().After(end) }, "OpenRails recovers the failed period")
		f.passes()
		require.Equal(t, 1, f.base.nmi.saleAttempts(), "one recovery charge")
		require.Len(t, completed(f.replicas[0].payments(embedded, l.c.id)), 2)
		require.True(t, f.base.nmi.scheduleLive(l.railSub), "NMI still owns the schedule")
	})
}

// Scenario 10: a rolling deploy restarts every replica, one at a time, while
// the due window is being worked; every membership still renews once.
func TestReplicasRollingDeploy(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 3)
	var cases []*engineCase
	for i := range 6 {
		cases = append(cases, enroll(t, f.replicas[i%3], rails[i%2], embedded))
	}
	ends := map[*engineCase]time.Time{}
	for _, e := range cases {
		ends[e] = f.periodEnd(e)
	}
	f.toDue(cases...)
	f.latency.Store(int64(150 * time.Millisecond))
	requests := func() int {
		n := 0
		for _, e := range cases {
			n += f.submissions(e)
		}
		return n
	}
	start := requests()
	passes := f.startPasses()
	for i, r := range f.replicas {
		require.Eventually(t, func() bool { return requests() > start+i }, 30*time.Second, 10*time.Millisecond, "renewals in flight before restarting %s", r.replica.name)
		f.restart(r)
	}
	f.latency.Store(0)
	for _, p := range passes {
		_, _ = f.any().jobs.JobRetry(t.Context(), p.id)
	}
	f.awaitPasses(passes)
	f.recover()
	f.passes()
	f.until(func() bool {
		for _, e := range cases {
			if !f.periodEnd(e).After(ends[e]) {
				return false
			}
		}
		return true
	}, fmt.Sprintf("all %d memberships renew", len(cases)))
	f.advance(time.Hour)
	f.wake()
	f.passes()
	for _, e := range cases {
		f.requireExactlyOnce(e, 1, -1)
		require.True(t, ends[e].Add(monthHours*time.Hour).Equal(f.periodEnd(e)), "exactly one period added")
	}
}
