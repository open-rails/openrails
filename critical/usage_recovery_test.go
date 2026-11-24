//go:build integration

package critical_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	embedded "github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/usageerasure"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// Recovery from what goes wrong between the event store and PostgreSQL: an
// intake whose transaction never committed, a store that lost events, an
// erasure stopped part way.

// erasureEvents starts the world's fleet and follows its job outcomes.
func erasureEvents(t *testing.T, w *world) <-chan *river.Event {
	t.Helper()
	events, unsubscribe := quietFleet(t, w).Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	t.Cleanup(unsubscribe)
	return events
}

// awaitErasure is the outcome of the next run of a usage erasure job.
func awaitErasure(t *testing.T, events <-chan *river.Event) river.EventKind {
	t.Helper()
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if event != nil && event.Job.Kind == usageerasure.Kind {
				return event.Kind
			}
		case <-deadline.C:
			t.Fatal("no usage erasure ran")
		}
	}
}

// loseLeaseCommit is the fixture of a sync whose events reached the store
// and whose transaction never committed: the lease settled nothing and the
// customer has no unposted usage.
func loseLeaseCommit(t *testing.T, w *world, lease billing.LeaseID) {
	t.Helper()
	leases, unposted := pgx.Identifier{w.schema, "leases"}.Sanitize(), pgx.Identifier{w.schema, "usage_unposted"}.Sanitize()
	_, err := w.pool.Exec(t.Context(), "UPDATE "+leases+" SET settled_through_n = 0, receipts = '[]'::jsonb WHERE merchant_id = $1 AND id = $2",
		w.alpha.MerchantID().UUID(), lease.UUID())
	require.NoError(t, err)
	_, err = w.pool.Exec(t.Context(), "DELETE FROM "+unposted+" WHERE merchant_id = $1", w.alpha.MerchantID().UUID())
	require.NoError(t, err)
}

// abandoningConn is the world's ClickHouse; armed, the usage insert's caller
// gives up as the insert is sent, and an insert still bound to the caller's
// context is cut short as the driver cuts it.
type abandoningConn struct {
	driver.Conn
	abandon atomic.Pointer[context.CancelFunc]
}

func (c *abandoningConn) PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error) {
	batch, err := c.Conn.PrepareBatch(ctx, query, opts...)
	if err != nil || !strings.Contains(query, "usage_events") {
		return batch, err
	}
	if cancel := c.abandon.Swap(nil); cancel != nil {
		return &abandonedBatch{Batch: batch, ctx: ctx, cancel: *cancel}, nil
	}
	return batch, nil
}

type abandonedBatch struct {
	driver.Batch
	ctx    context.Context
	cancel context.CancelFunc
}

func (b *abandonedBatch) Send() error {
	b.cancel()
	if err := b.ctx.Err(); err != nil {
		_ = b.Batch.Abort()
		return err
	}
	return b.Batch.Send()
}

// failingConn is the world's ClickHouse failing, once armed, the next
// statement that names fail.
type failingConn struct {
	driver.Conn
	fail atomic.Pointer[string]
}

func (c *failingConn) Exec(ctx context.Context, query string, args ...any) error {
	if fail := c.fail.Load(); fail != nil && strings.Contains(query, *fail) && c.fail.CompareAndSwap(fail, nil) {
		return errStoreDown
	}
	return c.Conn.Exec(ctx, query, args...)
}

func TestUsageRecovery(t *testing.T) {
	t.Run("OR-J24-S58/an-event-keeps-its-identity-across-attempts", func(t *testing.T) {
		t.Run("a-capture-retried-after-a-lost-commit-charges-once", func(t *testing.T) {
			w := newWorld(t, earlyInMonth)
			meterForCapture(t, w)
			customer, _ := fundedCustomer(t, w)
			admitted, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(w, customer, "retried", 300)})
			require.NoError(t, err)
			require.True(t, admitted[0].Allowed())
			captured, err := w.alpha.CaptureAdmissions(t.Context(), []billing.CaptureAdmissionParams{capture("retried", 250)})
			require.NoError(t, err)
			require.NoError(t, captured[0].Err())
			// Fixture: the capture's event reached the store, its transaction
			// never committed.
			holds, unposted := pgx.Identifier{w.schema, "limit_holds"}.Sanitize(), pgx.Identifier{w.schema, "usage_unposted"}.Sanitize()
			_, err = w.pool.Exec(t.Context(), "UPDATE "+holds+" SET state = 'open', capture_terms = NULL, captured_amount = NULL, captured_charges = NULL, captured_at = NULL "+
				"WHERE merchant_id = $1 AND request_id = $2", w.alpha.MerchantID().UUID(), "retried")
			require.NoError(t, err)
			_, err = w.pool.Exec(t.Context(), "DELETE FROM "+unposted+" WHERE merchant_id = $1", w.alpha.MerchantID().UUID())
			require.NoError(t, err)

			// The host retries the exact capture a minute later.
			w.clock.Advance(time.Minute)
			retried, err := w.alpha.CaptureAdmissions(t.Context(), []billing.CaptureAdmissionParams{capture("retried", 250)})
			require.NoError(t, err)
			require.NoError(t, retried[0].Err())
			require.EqualValues(t, 1, storeCount(t, w, "SELECT count() FROM {db}.usage_events WHERE merchant_id = {m}"), "the retry inserts nothing")
			require.EqualValues(t, 1, storeCount(t, w, "SELECT count() FROM {db}.usage_events FINAL WHERE merchant_id = {m}"))
			balance(t, w.alpha, customer, 750, 250, 1000)
			fleet := startFleet(t, w)
			nextMonth(w)
			closeMonths(t, fleet)
			balance(t, w.alpha, customer, 750, 0, 750)
		})
		t.Run("a-lease-item-without-occurred-at-resent-after-a-lost-commit-charges-once", func(t *testing.T) {
			w := newWorld(t, earlyInMonth)
			usageMeter(t, w.alpha, 100)
			customer, _ := fundedCustomer(t, w)
			lease := newLease()
			items := numbered(1, usageInput(customer, "a", 1), usageInput(customer, "b", 2))
			settleUsage(t, w.alpha, lease, items...)
			loseLeaseCommit(t, w, lease)

			// The batch reporter resends from its cursor two minutes later: its
			// items name no time.
			w.clock.Advance(2 * time.Minute)
			settleUsage(t, w.alpha, lease, items...)
			require.EqualValues(t, 2, storeCount(t, w, "SELECT count() FROM {db}.usage_events WHERE merchant_id = {m}"), "the resend inserts nothing")
			require.EqualValues(t, 2, storeCount(t, w, "SELECT count() FROM {db}.usage_events FINAL WHERE merchant_id = {m}"))
			balance(t, w.alpha, customer, 700, 300, 1000)
			fleet := startFleet(t, w)
			nextMonth(w)
			closeMonths(t, fleet)
			balance(t, w.alpha, customer, 700, 0, 700)
		})
	})

	t.Run("OR-J24-S59/a-resent-item-counts-in-the-session-its-first-attempt-opened", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, `schema_version: 1
meters:
  work: {event_type: work, value_property: units, aggregation: sum}
limits:
  session: {unit: units, reset: {kind: session, window_seconds: 3600}, counts: [{meter: work}]}
products:
  plan: {display_name: Plan, limits: {session: "100"}}
`)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "plan", 24*30)
		lease := newLease()
		opened := w.clock.Now().UTC().Truncate(time.Second)
		item := workReport(customer, "first", 10)
		item.OccurredAt = w.clock.Now().UTC()
		items := numbered(1, item)
		settleUsage(t, w.alpha, lease, items...)
		require.EqualValues(t, 10, customerLimit(t, w.alpha, customer, "session").Used)
		// Fixture: the sync that opened the session reached the store and never
		// committed.
		loseLeaseCommit(t, w, lease)
		_, err := w.pool.Exec(t.Context(), "DELETE FROM "+pgx.Identifier{w.schema, "limit_windows"}.Sanitize()+" WHERE merchant_id = $1 AND customer_id = $2",
			w.alpha.MerchantID().UUID(), customer.UUID())
		require.NoError(t, err)

		w.clock.Advance(10 * time.Minute)
		settleUsage(t, w.alpha, lease, items...)
		session := customerLimit(t, w.alpha, customer, "session")
		require.NotNil(t, session.WindowStartsAt)
		require.True(t, opened.Equal(*session.WindowStartsAt), "the resend opens the session its units were counted in: %s", session.WindowStartsAt)
		require.EqualValues(t, 10, session.Used)
	})

	t.Run("OR-J24-S60/an-insert-whose-caller-gave-up-finishes-before-the-resend-looks", func(t *testing.T) {
		var conn *abandoningConn
		w := newWorld(t, func(w *world) {
			conn = &abandoningConn{Conn: w.deps.ClickHouse}
			w.deps.ClickHouse = conn
		})
		usageMeter(t, w.alpha, 100)
		customer, _ := fundedCustomer(t, w)
		lease := newLease()
		items := numbered(1, usageInput(customer, "a", 1), usageInput(customer, "b", 2))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		conn.abandon.Store(&cancel)
		_, err := w.alpha.SyncLeases(ctx, billing.SyncLeasesParams{LeaseID: lease, Usage: items})
		require.Error(t, err, "the caller gave up")
		require.EqualValues(t, 2, storeCount(t, w, "SELECT count() FROM {db}.usage_events WHERE merchant_id = {m}"),
			"the insert finished before the sync let go of its locks")

		settleUsage(t, w.alpha, lease, items...)
		require.EqualValues(t, 2, storeCount(t, w, "SELECT count() FROM {db}.usage_events WHERE merchant_id = {m}"), "the resend inserts nothing")
		require.EqualValues(t, 3, storeCount(t, w, "SELECT toUInt64(sum(quantity)) FROM {db}.usage_quantities WHERE merchant_id = {m}"), "the rollups count each once")
		require.EqualValues(t, 2, storeCount(t, w, "SELECT toUInt64(sum(requests)) FROM {db}.usage_history WHERE merchant_id = {m} AND meter = ''"))
		balance(t, w.alpha, customer, 700, 300, 1000)
	})

	t.Run("OR-J24-S61/a-lot-lapsing-mid-month-funds-exactly-the-usage-before-its-lapse", func(t *testing.T) {
		lapsing := func(t *testing.T, w *world, customer billing.CustomerID, amount int64) (billing.CreditGrantID, time.Time) {
			t.Helper()
			lapse := w.clock.Now().UTC().Add(time.Hour)
			grants, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: amount,
				Source: "critical", SourceID: "lapsing", ExpiresAt: &lapse}})
			require.NoError(t, err)
			return grants[0].ID, lapse
		}
		expired := func(t *testing.T, w *world, lot billing.CreditGrantID) billing.CreditGrant {
			t.Helper()
			require.Eventually(t, func() bool { return lotExpired(t, w.alpha, lot) > 0 }, 15*time.Second, 50*time.Millisecond, "the watch expires the lot")
			grant, err := w.alpha.GetCreditGrant(t.Context(), lot)
			require.NoError(t, err)
			return *grant
		}
		t.Run("usage-after-the-lapse-holds-back-nothing", func(t *testing.T) {
			w := newWorld(t, earlyInMonth)
			usageMeter(t, w.alpha, 1)
			customer := billing.CustomerID(uuid.New())
			lot, lapse := lapsing(t, w, customer, 100)
			lease := newLease()
			settleUsage(t, w.alpha, lease, numbered(1, usageInput(customer, "before", 70))...)
			advanceTo(w, lapse.Add(10*time.Minute))
			settleUsage(t, w.alpha, lease, numbered(2, usageInput(customer, "after", 50))...)
			fleet := quietFleet(t, w)
			require.False(t, runPass(t, fleet))
			grant := expired(t, w, lot)
			require.EqualValues(t, 70, grant.SpentAmount, "the lot funds all the usage before its lapse")
			require.EqualValues(t, 30, grant.ExpiredAmount)
			nextMonth(w)
			require.False(t, runPass(t, fleet))
			require.EqualValues(t, 50, owed(t, w.alpha, customer), "only the usage after the lapse is owed")
		})
		t.Run("a-running-charge-posted-at-the-lapse-draws-it-too", func(t *testing.T) {
			w := newWorld(t, earlyInMonth)
			usageMeter(t, w.alpha, 1)
			customer := billing.CustomerID(uuid.New())
			lot, lapse := lapsing(t, w, customer, 100)
			_, err := w.alpha.StartRunningCharge(t.Context(), billing.StartRunningChargeParams{Reference: "lapsing", CustomerID: customer, Currency: "USD", RatePerHour: 30})
			require.NoError(t, err)
			lease := newLease()
			settleUsage(t, w.alpha, lease, numbered(1, usageInput(customer, "before", 50))...)
			advanceTo(w, lapse.Add(10*time.Minute))
			settleUsage(t, w.alpha, lease, numbered(2, usageInput(customer, "after", 40))...)
			fleet := quietFleet(t, w)
			require.False(t, runPass(t, fleet))
			grant := expired(t, w, lot)
			require.EqualValues(t, 80, grant.SpentAmount, "50 of usage and 30 accrued before the lapse")
			require.EqualValues(t, 20, grant.ExpiredAmount)
		})
	})

	t.Run("OR-J24-S65/a-merchants-configuration-first-read-inside-a-balance-look-loads", func(t *testing.T) {
		w := newWorld(t, earlyInMonth)
		usageMeter(t, w.alpha, 1)
		customer := billing.CustomerID(uuid.New())
		lapse := w.clock.Now().UTC().Add(time.Hour)
		grants, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 100,
			Source: "critical", SourceID: "lapsing", ExpiresAt: &lapse}})
		require.NoError(t, err)
		settleUsage(t, w.alpha, newLease(), numbered(1, usageInput(customer, "before", 30))...)
		advanceTo(w, lapse.Add(10*time.Minute))
		fleet := quietFleet(t, w)
		// The process forgets the merchant's configuration, as one that has
		// not read it since it started: the look reads it first, inside its
		// transaction, and the load runs on its own.
		embedded.Graph(w.engine).Runtime.MerchantConfig.Forget(w.alpha.MerchantID())
		require.False(t, runPass(t, fleet), "every look of the pass succeeds")
		grant, err := w.alpha.GetCreditGrant(t.Context(), grants[0].ID)
		require.NoError(t, err)
		require.EqualValues(t, 30, grant.SpentAmount, "the lot funds the usage before its lapse")
		require.EqualValues(t, 70, grant.ExpiredAmount, "the look expired the rest")
	})

	t.Run("OR-J24-S62/usage-the-store-lost-is-not-a-price-that-fell", func(t *testing.T) {
		w := newWorld(t, earlyInMonth)
		usageMeter(t, w.alpha, 100)
		customer, _ := fundedCustomer(t, w)
		lease := newLease()
		settleUsage(t, w.alpha, lease, numbered(1, usageInput(customer, "posted", 3))...)
		w.clock.Advance(time.Minute)
		fund(t, w.alpha, customer, "top-up", 100)
		balance(t, w.alpha, customer, 800, 0, 800)
		// Fixture: the store is restored from a backup older than that usage.
		for _, table := range []string{"usage_events", "limit_usage", "usage_quantities", "usage_minutes", "usage_history"} {
			require.NoError(t, w.deps.ClickHouse.Exec(t.Context(), "ALTER TABLE `"+w.schema+"`."+table+" DELETE WHERE merchant_id = toUUID(?) SETTINGS mutations_sync = 2",
				w.alpha.MerchantID().String()))
		}
		settleUsage(t, w.alpha, lease, numbered(2, usageInput(customer, "after-the-restore", 2))...)
		fleet := startFleet(t, w)
		nextMonth(w)
		closeMonths(t, fleet)
		balance(t, w.alpha, customer, 600, 0, 600)
		deductions := usageDeductions(t, w.alpha, customer)
		require.Len(t, deductions, 2)
		require.EqualValues(t, -200, deductions[0].Amount, "the usage since the restore is charged on top of what posted")
		// Its late window closed: nothing that posted is credited back.
		w.clock.Advance(70 * 24 * time.Hour)
		closeMonths(t, fleet)
		balance(t, w.alpha, customer, 600, 0, 600)
		require.Len(t, usageDeductions(t, w.alpha, customer), 2)
	})

	t.Run("OR-J24-S63/an-erasure-run-again-after-stopping-part-way-keeps-history-whole", func(t *testing.T) {
		var conn *failingConn
		a := newAuthorityWorld(t, func(w *world) {
			w.deps.UserInfo = nil
			conn = &failingConn{Conn: w.deps.ClickHouse}
			w.deps.ClickHouse = conn
		})
		usageMeter(t, a.alpha, 100)
		provisioner, err := a.alphaRemote.CreateProvisioningToken(t.Context(), billing.CreateProvisioningTokenParams{Name: "erasure"})
		require.NoError(t, err)
		id := uuid.NewString()
		user := map[string]any{"schemas": []string{hostSCIMUserSchema}, "externalId": id, "userName": "erased", "displayName": "Erased", "active": true}
		status, raw := a.request(t, "POST", "/v1/app/scim/v2/Users", provisioner.Token, user)
		require.Equal(t, http.StatusCreated, status, string(raw))
		customer := billing.CustomerID(uuid.MustParse(id))
		fund(t, a.alpha, customer, "erased-funds", 1000)
		settleUsage(t, a.alpha, newLease(), numbered(1, usageInput(customer, "first", 2), usageInput(customer, "second", 3))...)
		w := a.world

		// The erasure's job stops between folding the history and deleting
		// its invokers' rows; River runs it again.
		events := erasureEvents(t, w)
		stop := ".usage_history DELETE"
		conn.fail.Store(&stop)
		status, raw = a.request(t, "DELETE", "/v1/app/scim/v2/Users/"+id, provisioner.Token, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		require.Equal(t, river.EventKindJobFailed, awaitErasure(t, events), "the first run stops part way")
		require.Equal(t, river.EventKindJobCompleted, awaitErasure(t, events), "the next finishes")

		require.Zero(t, storeCount(t, w, "SELECT count() FROM {db}.usage_history WHERE merchant_id = {m} AND invoker != ''"))
		require.EqualValues(t, 2, storeCount(t, w, "SELECT toUInt64(sum(requests)) FROM {db}.usage_history WHERE merchant_id = {m} AND meter = ''"), "each event once")
		require.EqualValues(t, 5, storeCount(t, w, "SELECT toUInt64(sum(quantity)) FROM {db}.usage_history WHERE merchant_id = {m}"), "quantities once")
		balance(t, a.alpha, customer, 500, 500, 1000)
	})

	t.Run("OR-J24-S64/usage-whose-card-print-never-committed-holds-up-no-posting", func(t *testing.T) {
		// orphan settles usage under a revised card and forgets that card's
		// print, as an intake that inserted its events and never committed.
		orphan := func(t *testing.T) (*world, billing.CustomerID, *billing.SetMeterParams) {
			t.Helper()
			w := newWorld(t, earlyInMonth)
			_, params := usageMeter(t, w.alpha, 100)
			customer, _ := fundedCustomer(t, w)
			lease := newLease()
			settleUsage(t, w.alpha, lease, numbered(1, usageInput(customer, "first-card", 2))...)
			w.clock.Advance(time.Minute)
			params.RateCard.Value.Price.PerUnit.UnitAmount = 300
			_, err := w.alpha.SetMeter(t.Context(), "compute", params)
			require.NoError(t, err)
			settleUsage(t, w.alpha, lease, numbered(2, usageInput(customer, "second-card", 1))...)
			prints := pgx.Identifier{w.schema, "usage_card_prints"}.Sanitize()
			removed, err := w.pool.Exec(t.Context(), "DELETE FROM "+prints+" WHERE merchant_id = $1 AND created_at = (SELECT max(created_at) FROM "+prints+" WHERE merchant_id = $1)",
				w.alpha.MerchantID().UUID())
			require.NoError(t, err)
			require.EqualValues(t, 1, removed.RowsAffected())
			return w, customer, &params
		}
		t.Run("a-print-a-card-in-force-names-is-recorded-again", func(t *testing.T) {
			w, customer, _ := orphan(t)
			fleet := startFleet(t, w)
			nextMonth(w)
			closeMonths(t, fleet)
			deductions := usageDeductions(t, w.alpha, customer)
			require.Len(t, deductions, 1)
			require.EqualValues(t, -500, deductions[0].Amount, "2 at 100, then 1 at 300")
		})
		t.Run("one-nothing-names-is-left-out", func(t *testing.T) {
			w, customer, params := orphan(t)
			params.RateCard.Value.Price.PerUnit.UnitAmount = 500
			_, err := w.alpha.SetMeter(t.Context(), "compute", *params)
			require.NoError(t, err)
			fleet := startFleet(t, w)
			nextMonth(w)
			closeMonths(t, fleet)
			deductions := usageDeductions(t, w.alpha, customer)
			require.Len(t, deductions, 1)
			require.EqualValues(t, -200, deductions[0].Amount, "the usage that can be priced posts")
		})
	})
}
