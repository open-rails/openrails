//go:build integration

package critical_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	embedded "github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/modules/leases"
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

	t.Run("OR-J24-S85/usage-reported-after-a-lapse-is-split-at-it-and-only-what-came-before-draws-the-lot", func(t *testing.T) {
		w := newWorld(t, earlyInMonth)
		usageMeter(t, w.alpha, 1)
		customer := billing.CustomerID(uuid.New())
		lapse := w.clock.Now().UTC().Add(time.Hour).Truncate(time.Second)
		grants, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 100,
			Source: "critical", SourceID: "lapsing", ExpiresAt: &lapse}})
		require.NoError(t, err)
		// One sync after the lapse, before any look expired the lot, carries
		// usage from either side of it.
		advanceTo(w, lapse.Add(30*time.Minute))
		before := usageInput(customer, "before", 60)
		before.OccurredAt = lapse.Add(-30 * time.Minute)
		after := usageInput(customer, "after", 25)
		after.OccurredAt = lapse.Add(10 * time.Minute)
		settleUsage(t, w.alpha, newLease(), numbered(1, before, after)...)
		segments := map[time.Time]int64{}
		rows, err := w.pool.Query(t.Context(), "SELECT lapse_at, sum(quantity)::bigint FROM "+pgx.Identifier{w.schema, "usage_totals"}.Sanitize()+
			" WHERE merchant_id = $1 AND customer_id = $2 GROUP BY lapse_at", w.alpha.MerchantID().UUID(), customer.UUID())
		require.NoError(t, err)
		for rows.Next() {
			var at time.Time
			var quantity int64
			require.NoError(t, rows.Scan(&at, &quantity))
			segments[at.UTC()] = quantity
		}
		require.NoError(t, rows.Err())
		require.Len(t, segments, 2, "the running totals split at the lapse")
		require.EqualValues(t, 25, segments[lapse], "what came after the lapse is its own segment")

		fleet := quietFleet(t, w)
		require.False(t, runPass(t, fleet))
		require.Eventually(t, func() bool { return lotExpired(t, w.alpha, grants[0].ID) > 0 }, 15*time.Second, 50*time.Millisecond, "the watch expires the lot")
		grant, err := w.alpha.GetCreditGrant(t.Context(), grants[0].ID)
		require.NoError(t, err)
		require.EqualValues(t, 60, grant.SpentAmount, "the posting before the lapse took only the usage before it")
		require.EqualValues(t, 40, grant.ExpiredAmount)
		nextMonth(w)
		require.False(t, runPass(t, fleet))
		require.EqualValues(t, 25, owed(t, w.alpha, customer), "the usage after the lapse is owed")
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
		day := w.clock.Now().UTC()
		settleUsage(t, w.alpha, lease, numbered(1, usageInput(customer, "posted", 3))...)
		fleet := startFleet(t, w)
		nextDay(w)
		postDays(t, fleet)
		balance(t, w.alpha, customer, 700, 0, 700)
		// Fixture: the store is restored from a backup older than that usage.
		for _, table := range []string{"usage_events", "limit_usage", "usage_history"} {
			require.NoError(t, w.deps.ClickHouse.Exec(t.Context(), "ALTER TABLE `"+w.schema+"`."+table+" DELETE WHERE merchant_id = toUUID(?) SETTINGS mutations_sync = 2",
				w.alpha.MerchantID().String()))
		}
		// Usage of the same day, reported after the restore.
		after := usageInput(customer, "after-the-restore", 2)
		after.OccurredAt = day.Add(time.Minute)
		settleUsage(t, w.alpha, lease, numbered(2, after)...)
		balance(t, w.alpha, customer, 500, 200, 700)
		nextDay(w)
		postDays(t, fleet)
		balance(t, w.alpha, customer, 500, 0, 500)
		deductions := usageDeductions(t, w.alpha, customer)
		require.Len(t, deductions, 2)
		require.EqualValues(t, -200, deductions[0].Amount, "the usage since the restore is charged on top of what posted")
		// Its late window closed: nothing that posted is credited back.
		w.clock.Advance(70 * 24 * time.Hour)
		postDays(t, fleet)
		balance(t, w.alpha, customer, 500, 0, 500)
		require.Len(t, usageDeductions(t, w.alpha, customer), 2)
	})

	t.Run("OR-J24-S63/an-erasure-run-again-after-stopping-part-way-clears-the-events-and-leaves-history-alone", func(t *testing.T) {
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

		// The erasure's job stops at the events' update; River runs it again.
		history := func() [3]uint64 {
			return [3]uint64{storeCount(t, w, "SELECT count() FROM {db}.usage_history WHERE merchant_id = {m}"),
				storeCount(t, w, "SELECT toUInt64(sum(requests)) FROM {db}.usage_history WHERE merchant_id = {m} AND meter = ''"),
				storeCount(t, w, "SELECT toUInt64(sum(quantity)) FROM {db}.usage_history WHERE merchant_id = {m}")}
		}
		before := history()
		events := erasureEvents(t, w)
		stop := ".usage_events UPDATE"
		conn.fail.Store(&stop)
		status, raw = a.request(t, "DELETE", "/v1/app/scim/v2/Users/"+id, provisioner.Token, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		require.Equal(t, river.EventKindJobFailed, awaitErasure(t, events), "the first run stops part way")
		require.Equal(t, river.EventKindJobCompleted, awaitErasure(t, events), "the next finishes")

		require.Zero(t, storeCount(t, w, "SELECT count() FROM {db}.usage_events FINAL WHERE merchant_id = {m} AND (invoker != '' OR metadata != '')"))
		require.Equal(t, before, history(), "history names no invoker: erasure leaves it as it was")
		require.EqualValues(t, [3]uint64{before[0], 2, 5}, before, "each event once, quantities once")
		balance(t, a.alpha, customer, 500, 500, 1000)
	})

	t.Run("OR-J24-S72/a-resend-after-a-lost-reply-adds-nothing-to-pending", func(t *testing.T) {
		w := newWorld(t, earlyInMonth)
		usageMeter(t, w.alpha, 100)
		customer, _ := fundedCustomer(t, w)
		lease := newLease()
		items := numbered(1, usageInput(customer, "a", 1), usageInput(customer, "b", 2))
		settleUsage(t, w.alpha, lease, items...)
		balance(t, w.alpha, customer, 700, 300, 1000)

		// The reply was lost: the gate resends, and the lease's receipts answer.
		out := settleUsage(t, w.alpha, lease, items...)
		require.EqualValues(t, 2, out.Usage.ThroughN)
		require.EqualValues(t, 2, storeCount(t, w, "SELECT count() FROM {db}.usage_events WHERE merchant_id = {m}"), "the resend inserts nothing")
		balance(t, w.alpha, customer, 700, 300, 1000)

		// Resent with the next item, only the next settles.
		settleUsage(t, w.alpha, lease, append(items, numbered(3, usageInput(customer, "c", 1))...)...)
		balance(t, w.alpha, customer, 600, 400, 1000)
	})

	t.Run("OR-J24-S73/a-sync-that-failed-after-its-insert-leaves-its-usage-unsettled-and-the-resend-adds-it-once", func(t *testing.T) {
		w := newWorld(t, earlyInMonth)
		// The first 100 units at 2, the rest at 1.
		pricedMeter(t, w.alpha, tiers(catalog.TierModeGraduated, 2, 1, 100))
		customer, _ := fundedCustomer(t, w)
		lease := newLease()
		items := unitItems(customer, 1, leases.UsageChunkItems+1)
		failAfterFirstChunk(t, lease)
		_, err := w.alpha.SyncLeases(t.Context(), billing.SyncLeasesParams{LeaseID: lease, Usage: items})
		require.Error(t, err)
		require.EqualValues(t, leases.UsageChunkItems, storeCount(t, w, "SELECT count() FROM {db}.usage_events FINAL WHERE merchant_id = {m}"), "its first chunk reached the store")
		balance(t, w.alpha, customer, 1000, 0, 1000)

		// The resend settles all of it: each item's price is pending once,
		// priced from the period's start, as if the failed sync never ran.
		out := settleUsage(t, w.alpha, lease, items...)
		require.EqualValues(t, leases.UsageChunkItems+1, out.Usage.ThroughN)
		require.EqualValues(t, leases.UsageChunkItems+1, storeCount(t, w, "SELECT count() FROM {db}.usage_events FINAL WHERE merchant_id = {m}"))
		balance(t, w.alpha, customer, 649, 351, 1000)
		fleet := startFleet(t, w)
		nextDay(w)
		postDays(t, fleet)
		balance(t, w.alpha, customer, 649, 0, 649)
	})

	t.Run("OR-J24-S74/a-posting-before-the-resend-leaves-unsettled-usage-to-it", func(t *testing.T) {
		w := newWorld(t, earlyInMonth)
		usageMeter(t, w.alpha, 1)
		customer, _ := fundedCustomer(t, w)
		lease := newLease()
		settleUsage(t, w.alpha, lease, numbered(1, usageInput(customer, "settled", 10))...)
		items := unitItems(customer, 2, leases.UsageChunkItems+1)
		failAfterFirstChunk(t, lease)
		_, err := w.alpha.SyncLeases(t.Context(), billing.SyncLeasesParams{LeaseID: lease, Usage: items})
		require.Error(t, err)
		balance(t, w.alpha, customer, 990, 10, 1000)

		// The day's posting takes what settled and leaves what the failed
		// sync inserted.
		fleet := startFleet(t, w)
		nextDay(w)
		postDays(t, fleet)
		balance(t, w.alpha, customer, 990, 0, 990)

		// The resend settles it: pending once, then posted once.
		settleUsage(t, w.alpha, lease, items...)
		balance(t, w.alpha, customer, 739, 251, 990)
		nextDay(w)
		postDays(t, fleet)
		balance(t, w.alpha, customer, 739, 0, 739)
	})

	t.Run("OR-J24-S75/a-resend-older-than-the-kept-receipts-settles-nothing-again", func(t *testing.T) {
		w := newWorld(t, earlyInMonth)
		usageMeter(t, w.alpha, 1)
		customer, _ := fundedCustomer(t, w)
		lease := newLease()
		first := numbered(1, usageInput(customer, "first", 1))
		settleUsage(t, w.alpha, lease, first...)
		// One settlement per sync until the lease no longer keeps the first's
		// receipt.
		table := pgx.Identifier{w.schema, "leases"}.Sanitize()
		n := int64(1)
		for kept := int64(1); kept == 1; {
			n++
			settleUsage(t, w.alpha, lease, numbered(n, usageInput(customer, fmt.Sprintf("item-%d", n), 1))...)
			require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT min((r->>'from_n')::bigint) FROM "+table+", jsonb_array_elements(receipts) r WHERE merchant_id = $1 AND id = $2",
				w.alpha.MerchantID().UUID(), lease.UUID()).Scan(&kept))
			require.Less(t, n, int64(1000), "the lease keeps a bounded number of receipts")
		}
		balance(t, w.alpha, customer, 1000-n, n, 1000)

		// Its settled number still covers it: nothing settles again.
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: first})
		require.Equal(t, n, out.Usage.ThroughN)
		require.Empty(t, out.Usage.Refused)
		require.EqualValues(t, n, storeCount(t, w, "SELECT count() FROM {db}.usage_events WHERE merchant_id = {m}"))
		balance(t, w.alpha, customer, 1000-n, n, 1000)
	})
}

// unitItems are count one-unit reports for a customer, numbered from first.
func unitItems(customer billing.CustomerID, first int64, count int) []billing.LeaseUsage {
	items := make([]billing.LeaseUsage, count)
	for i := range items {
		items[i] = usageInput(customer, fmt.Sprintf("unit-%d", first+int64(i)), 1)
	}
	return numbered(first, items...)
}

// failAfterFirstChunk fails the lease's next sync once its first chunk is in
// the store and its transaction holds the chunk's settlement: the sync rolls
// back, and the store keeps the events.
func failAfterFirstChunk(t *testing.T, lease billing.LeaseID) {
	t.Helper()
	var failed atomic.Bool
	t.Cleanup(failpoint.Set(func(_ context.Context, site failpoint.Site) error {
		if site.Point == failpoint.AfterChunk && site.Kind == leases.FailpointKind && site.Operation == lease.UUID() && failed.CompareAndSwap(false, true) {
			return errors.New("the sync fails after its first chunk")
		}
		return nil
	}))
}
