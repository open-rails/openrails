//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

func TestUsageContracts(t *testing.T) {
	t.Run("OR-J24-S01/replay-retains-one-immutable-rated-event", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		lease := newLease()
		items := numbered(1, usageInput(customer, "stable-event", 2))
		settleUsage(t, w.alpha, lease, items...)
		balance(t, w.alpha, customer, 800, 200, 1000)

		// A lost reply resends the item: it answers its receipt and settles
		// nothing again.
		replay := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: items})
		require.EqualValues(t, 1, replay.Usage.ThroughN)
		require.Empty(t, replay.Usage.Refused)
		balance(t, w.alpha, customer, 800, 200, 1000)

		// A changed payload under the settled number is refused.
		for _, change := range []func(*billing.LeaseUsage){
			func(u *billing.LeaseUsage) { u.Dimensions = map[string]int64{"units": 99} },
			func(u *billing.LeaseUsage) { u.Outcome, u.Amount = billing.UsageFailed, ptr(int64(201)) },
		} {
			changed := items[0]
			change(&changed)
			out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: []billing.LeaseUsage{changed}})
			refused := refusedItem(t, out, 1)
			require.Equal(t, http.StatusUnprocessableEntity, refused.Status)
			require.Equal(t, billing.CodeIdempotencyKeyReused, refused.Error.Code)
		}
		balance(t, w.alpha, customer, 800, 200, 1000)

		// The month's usage posts once, after the month ends, with what it was
		// for.
		fleet := startFleet(t, w)
		nextMonth(w)
		postDays(t, fleet)
		balance(t, w.alpha, customer, 800, 0, 800)
		ledger, err := w.alpha.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		require.Len(t, ledger.Items, 2, "the grant and one usage deduction")
		deductions := usageDeductions(t, w.alpha, customer)
		require.Len(t, deductions, 1)
		require.EqualValues(t, -200, deductions[0].Amount)
		require.Equal(t, []billing.UsageDeductionLine{{MeterKey: "compute", Requests: 1, Quantity: 2, Amount: 200}}, deductions[0].Usage.Lines)
		postDays(t, fleet)
		require.Len(t, usageDeductions(t, w.alpha, customer), 1, "a posted month posts nothing again")
	})

	t.Run("OR-J24-S02/late-event-uses-ingest-price-and-prior-event-stays-pinned", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		_, params := usageMeter(t, w.alpha, 100)
		lease := newLease()
		original := numbered(1, usageInput(customer, "before-price-edit", 2))
		settleUsage(t, w.alpha, lease, original...)
		balance(t, w.alpha, customer, 800, 200, 1000)
		params.RateCard.Value.Price.PerUnit.UnitAmount = 300
		_, err := w.alpha.SetMeter(t.Context(), "compute", params)
		require.NoError(t, err)
		late := usageInput(customer, "late-after-price-edit", 2)
		late.OccurredAt = w.clock.Now().Add(-time.Hour)
		settleUsage(t, w.alpha, lease, numbered(2, late)...)
		balance(t, w.alpha, customer, 200, 800, 1000)
		// The first item stays as it settled: its resend charges nothing.
		replay := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: original})
		require.Empty(t, replay.Usage.Refused)
		balance(t, w.alpha, customer, 200, 800, 1000)
		changed := params
		changed.ValueProperty = "different-units"
		_, err = w.alpha.SetMeter(t.Context(), "compute", changed)
		require.ErrorIs(t, err, billing.ErrConflict, "settled usage pins the meter's reading")
		var outside []billing.LeaseUsage
		for i, at := range []time.Time{w.clock.Now().Add(-36 * 24 * time.Hour), w.clock.Now().Add(time.Hour)} {
			invalid := usageInput(customer, fmt.Sprintf("outside-%d", i), 2)
			invalid.OccurredAt = at
			outside = append(outside, invalid)
		}
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(3, outside...)})
		require.EqualValues(t, 4, out.Usage.ThroughN, "a refused item is settled as refused")
		for _, n := range []int64{3, 4} {
			refused := refusedItem(t, out, n)
			require.Equal(t, http.StatusBadRequest, refused.Status)
			require.Equal(t, billing.CodeInvalidParam, refused.Error.Code)
		}
		balance(t, w.alpha, customer, 200, 800, 1000)
	})

	t.Run("OR-J24-S03/batch-item-errors-preserve-neighbors-and-merchant-boundary", func(t *testing.T) {
		a := newAuthorityWorld(t)
		usageMeter(t, a.alphaRemote, 100)
		alice, bob := billing.CustomerID(uuid.MustParse(a.alice.ID)), billing.CustomerID(uuid.MustParse(a.bob.ID))
		lease := newLease()
		items := numbered(1, usageInput(alice, "alice-good", 2), usageInput(billing.CustomerID{}, "invalid-customer", 5), usageInput(bob, "bob-good", 3))
		out := syncLease(t, a.alphaRemote, billing.SyncLeasesParams{LeaseID: lease, Usage: items})
		require.EqualValues(t, 3, out.Usage.ThroughN)
		require.Len(t, out.Usage.Refused, 1)
		refused := refusedItem(t, out, 2)
		require.Equal(t, http.StatusBadRequest, refused.Status)
		require.Equal(t, billing.CodeInvalidParam, refused.Error.Code)

		// Another merchant's credential names another lease under the same id:
		// it sees none of the first merchant's receipts.
		foreign := syncLease(t, a.betaRemote, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(1, usageInput(billing.CustomerID(uuid.New()), "beta-own", 1))})
		require.EqualValues(t, 1, foreign.Usage.ThroughN)
		require.Empty(t, foreign.Usage.Refused)
		status, raw := a.request(t, http.MethodPost, "/v1/app/leases/sync", a.aliceToken, billing.SyncLeasesParams{LeaseID: lease, Usage: items})
		require.Equal(t, http.StatusForbidden, status, string(raw))

		report := func(token string) billing.Usage {
			q := url.Values{"currency": {"USD"}, "group_by": {"event_type"}}
			status, raw := a.request(t, http.MethodGet, "/v1/me/usage?"+q.Encode(), token, nil)
			require.Equal(t, http.StatusOK, status, string(raw))
			var page billing.Usage
			require.NoError(t, json.Unmarshal(raw, &page))
			return page
		}
		own := report(a.aliceToken)
		require.Len(t, own.Rows, 1)
		require.EqualValues(t, 200, own.Rows[0].Amount)
		require.EqualValues(t, 1, own.Rows[0].EventCount)
		require.EqualValues(t, 300, report(a.bobToken).Rows[0].Amount)
	})

	t.Run("OR-J24-S04/grouped-pagination-retains-exact-customer-totals", func(t *testing.T) {
		a := newAuthorityWorld(t)
		usageMeter(t, a.alphaRemote, 10)
		alice, bob := billing.CustomerID(uuid.MustParse(a.alice.ID)), billing.CustomerID(uuid.MustParse(a.bob.ID))
		var items []billing.LeaseUsage
		for i, row := range []struct {
			invoker string
			units   int64
		}{{"alpha", 10}, {"alpha", 20}, {"beta", 40}, {"gamma", 5}} {
			input := usageInput(alice, fmt.Sprintf("report-%d", i), row.units)
			input.Invoker = row.invoker
			items = append(items, input)
		}
		foreign := usageInput(bob, "foreign-report", 90)
		foreign.Invoker = "foreign"
		settleUsage(t, a.alphaRemote, newLease(), numbered(1, append(items, foreign)...)...)
		q := url.Values{"currency": {"USD"}, "group_by": {"invoker"}, "from": {a.clock.Now().Add(-time.Hour).Format(time.RFC3339Nano)}, "to": {a.clock.Now().Add(time.Hour).Format(time.RFC3339Nano)}, "limit": {"1"}}
		var all []billing.UsageRow
		var firstCursor string
		for i := 0; i < 3; i++ {
			status, raw := a.request(t, http.MethodGet, "/v1/me/usage?"+q.Encode(), a.aliceToken, nil)
			require.Equal(t, http.StatusOK, status, string(raw))
			var page billing.Usage
			require.NoError(t, json.Unmarshal(raw, &page))
			require.Equal(t, alice, page.CustomerID)
			require.Len(t, page.Rows, 1)
			all = append(all, page.Rows...)
			if i < 2 {
				require.NotNil(t, page.NextCursor)
				q.Set("cursor", *page.NextCursor)
				if i == 0 {
					firstCursor = *page.NextCursor
				}
			} else {
				require.Nil(t, page.NextCursor)
			}
		}
		require.Equal(t, []string{"beta", "alpha", "gamma"}, []string{all[0].Key, all[1].Key, all[2].Key})
		require.Equal(t, []int64{400, 300, 50}, []int64{all[0].Amount, all[1].Amount, all[2].Amount})
		require.Equal(t, []int64{1, 2, 1}, []int64{all[0].EventCount, all[1].EventCount, all[2].EventCount})
		q.Set("cursor", firstCursor)
		status, raw := a.request(t, http.MethodGet, "/v1/me/usage?"+q.Encode(), a.bobToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var scoped billing.Usage
		require.NoError(t, json.Unmarshal(raw, &scoped))
		require.Equal(t, bob, scoped.CustomerID)
		require.Empty(t, scoped.Rows, "a pagination position cannot select another customer's groups")
		q.Del("cursor")
		status, raw = a.request(t, http.MethodGet, "/v1/me/usage?"+q.Encode(), a.bobToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &scoped))
		require.Len(t, scoped.Rows, 1)
		require.Equal(t, "foreign", scoped.Rows[0].Key)
		require.EqualValues(t, 900, scoped.Rows[0].Amount)
		q.Set("group_by", "resource")
		status, raw = a.request(t, http.MethodGet, "/v1/me/usage?"+q.Encode(), a.aliceToken, nil)
		require.Equal(t, http.StatusBadRequest, status, "usage keeps no free-form resource to group by: %s", raw)
	})

	t.Run("OR-J24-S05/charged-failure-replayed-past-its-key-window-charges-once", func(t *testing.T) {
		w := newWorld(t, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.BillingPolicies = []billing.BillingPolicy{{Name: "grace", Kind: "outstanding_cap", BadSpendWindows: []billing.BudgetWindow{{Key: "hourly", WindowSeconds: 3600, Limit: 100, Currency: "USD"}}}}
			m.BillingPolicyBindings = []billing.BillingPolicyBinding{{PolicyName: "grace"}}
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		customer := billing.CustomerID(uuid.New())
		grant := func(id string, amount int64) {
			_, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: amount, Source: "critical", SourceID: id}})
			require.NoError(t, err)
		}
		failed := func(id string, amount int64) billing.LeaseUsage {
			v := usageInput(customer, id, 0)
			v.Outcome, v.Amount = billing.UsageFailed, ptr(amount)
			return v
		}
		grant("lot-a", 300)
		lease := newLease()
		first := numbered(1, failed("failed-once", 400))
		settleUsage(t, w.alpha, lease, first...)
		// A charged failure posts its own legs at once: 300 past the 100 of grace.
		balance(t, w.alpha, customer, 0, 0, 0)
		ledger, err := w.alpha.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		require.Len(t, ledger.Items, 2, "the grant and the failure's charge")

		// Past the old key window lot A is empty and lot B funds the customer;
		// the lease still knows what it settled.
		w.clock.Advance(36 * 24 * time.Hour)
		grant("lot-b", 1000)
		changed := first[0]
		changed.Amount = ptr(int64(500))
		refused := refusedItem(t, syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: []billing.LeaseUsage{changed}}), 1)
		require.Equal(t, billing.CodeIdempotencyKeyReused, refused.Error.Code)
		replay := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: first})
		require.Empty(t, replay.Usage.Refused)
		require.EqualValues(t, 1, replay.Usage.ThroughN)
		balance(t, w.alpha, customer, 1000, 0, 1000)

		// The resend counted nothing toward this window's grace.
		settleUsage(t, w.alpha, lease, numbered(2, failed("failed-next", 100))...)
		balance(t, w.alpha, customer, 1000, 0, 1000)
	})
	t.Run("OR-J24-S36/a-meter-correction-holds-only-its-own-merchants-usage", func(t *testing.T) {
		w := newWorld(t)
		_, params := usageMeter(t, w.alpha, 100)
		usageMeter(t, w.beta, 100)
		customer, own := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
		_, err := w.beta.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "funded"}})
		require.NoError(t, err)
		_, err = w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: own, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "funded"}})
		require.NoError(t, err)
		// Alpha's correction stops while it holds its usage fence.
		held, release := make(chan struct{}), make(chan struct{})
		var hold, open sync.Once
		t.Cleanup(failpoint.Set(func(ctx context.Context, site failpoint.Site) error {
			if site.Point == failpoint.AfterFence && site.Kind == money.MeterCorrectionFailpointKind {
				hold.Do(func() { close(held) })
				<-release
			}
			return nil
		}))
		t.Cleanup(func() { open.Do(func() { close(release) }) })
		changed := params
		changed.ValueProperty = "corrected-units"
		corrected := make(chan error, 1)
		go func() {
			_, err := w.alpha.SetMeter(t.Context(), "compute", changed)
			corrected <- err
		}()
		select {
		case <-held:
		case err := <-corrected:
			t.Fatalf("the correction finished without holding its fence: %v", err)
		case <-time.After(20 * time.Second):
			t.Fatal("the correction never reached its fence")
		}
		// Alpha's own settlement waits for the correction's transaction (its
		// lease sync first reads the merchant row the correction holds for its
		// catalog revision, before it would reach the fence).
		fence := "openrails.usage_activity:" + w.alpha.MerchantID().UUID().String() + ":compute"
		var correction int
		require.NoError(t, w.pool.QueryRow(t.Context(), `SELECT pid FROM pg_locks WHERE locktype = 'advisory' AND granted
			AND classid::bigint = (hashtextextended($1, 0) >> 32) & 4294967295 AND objid::bigint = hashtextextended($1, 0) & 4294967295`, fence).Scan(&correction))
		settled := make(chan error, 1)
		go func() {
			_, err := w.alpha.SyncLeases(t.Context(), billing.SyncLeasesParams{LeaseID: newLease(), Usage: numbered(1, usageInput(own, "alpha-during-correction", 2))})
			settled <- err
		}()
		require.Eventually(t, func() bool {
			var waiting int
			require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))", correction).Scan(&waiting))
			return waiting > 0
		}, 20*time.Second, 20*time.Millisecond, "alpha's settlement waits for its own merchant's correction")
		select {
		case err := <-settled:
			t.Fatalf("alpha's settlement finished while its correction was open: %v", err)
		default:
		}
		// Beta settles usage while alpha's correction is open.
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		out, err := w.beta.SyncLeases(ctx, billing.SyncLeasesParams{LeaseID: newLease(), Usage: numbered(1, usageInput(customer, "beta-during-correction", 2))})
		require.NoError(t, err, "another merchant's settlement waits for nothing")
		require.Empty(t, out.Usage.Refused)
		require.EqualValues(t, 1, out.Usage.ThroughN)
		balance(t, w.beta, customer, 800, 200, 1000)
		open.Do(func() { close(release) })
		require.NoError(t, <-corrected)
		require.NoError(t, <-settled, "and settles once the correction commits")
		meter, err := w.alpha.GetMeter(t.Context(), "compute")
		require.NoError(t, err)
		require.Equal(t, "corrected-units", meter.ValueProperty)
	})
}
