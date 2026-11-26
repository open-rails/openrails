//go:build integration

package critical_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

// The host queue's public persisted kind is an external integration boundary;
// the actual registered worker decodes the payload and performs the work.
type registeredJob struct {
	KindName string `json:"-"`
}

func (j registeredJob) Kind() string { return j.KindName }

func TestCreditExpiry(t *testing.T) {
	t.Run("OR-J22-S03/expiry-preserves-spent-value-and-what-a-live-gate-has-in-flight", func(t *testing.T) {
		w := newWorld(t)
		meterForCapture(t, w)
		customer := billing.CustomerID(uuid.New())
		expiring, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "expiring", ExpiresAt: ptr(w.clock.Now().Add(time.Hour))}})
		require.NoError(t, err)
		spend(t, w.alpha, customer, "already-spent", 450)
		w.clock.Advance(time.Second)
		_, err = w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 500, Source: "critical", SourceID: "lasting"}})
		require.NoError(t, err)
		// A gate has a request of 400 in flight.
		lease := newLease()
		inFlight := []billing.InFlight{{CustomerID: customer, Limit: "balance", Unit: "USD", Amount: 400}}
		syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, InFlight: inFlight})
		balance(t, w.alpha, customer, 650, 400, 1050)
		w.clock.Advance(time.Hour)
		runRegistered(t, startFleet(t, w), "openrails.billing_pass", 1)
		grant, err := w.alpha.GetCreditGrant(t.Context(), expiring[0].ID)
		require.NoError(t, err)
		require.EqualValues(t, 450, grant.SpentAmount)
		require.EqualValues(t, 550, grant.ExpiredAmount)
		require.Zero(t, grant.RemainingAmount)
		// The gate syncs again with its request still in flight, then settles
		// it at 100.
		syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, InFlight: inFlight})
		balance(t, w.alpha, customer, 100, 400, 500)
		settleUsage(t, w.alpha, lease, numbered(1, billing.LeaseUsage{CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer,
			Currency: "USD", EventType: "compute", Dimensions: map[string]int64{"units": 1}, Amount: ptr(int64(100)), Source: "critical", SourceID: "still-in-flight"})...)
		balance(t, w.alpha, customer, 400, 100, 500)
		ledger, err := w.alpha.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		expiredEntries := 0
		for _, entry := range ledger.Items {
			if entry.Type == billing.BalanceTransactionExpire {
				expiredEntries++
				require.EqualValues(t, -550, entry.Amount)
			}
		}
		require.Equal(t, 1, expiredEntries)
	})
	t.Run("OR-J22-S50/a-view-offers-no-credit-that-lapsed-before-its-watch-looks", func(t *testing.T) {
		w := newWorld(t)
		meterForCapture(t, w)
		customer := billing.CustomerID(uuid.New())
		_, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 500, Source: "critical", SourceID: "lasting"}})
		require.NoError(t, err)
		endsAt := w.clock.Now().Add(time.Hour)
		_, err = w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "lapsing", ExpiresAt: &endsAt}})
		require.NoError(t, err)
		// No balance watch runs: the lapsed lot is still in the ledger balance.
		w.clock.Advance(time.Hour + time.Second)
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Views: []billing.ViewParams{computeView(customer)}})
		require.EqualValues(t, 500, lineOf(t, viewOf(t, out, customer), catalog.BalanceLimit).Remaining, "credit that lapsed is not offered")
		// The ledger holds the lapsed lot until its watch expires it.
		balance(t, w.alpha, customer, 500, 0, 1500)
	})
	t.Run("OR-J22-S06/lapsed-credit-leaves-available-at-its-lapse-and-its-watch-expires-it", func(t *testing.T) {
		w := newWorld(t)
		customer := billing.CustomerID(uuid.New())
		// An arrears customer: its balance is watched only while it holds a lot that lapses.
		_, err := w.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 5000}}})
		require.NoError(t, err)
		_, err = w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 500, Source: "critical", SourceID: "lasting"}})
		require.NoError(t, err)
		endsAt := w.clock.Now().Add(time.Hour)
		lapsing, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "lapsing", ExpiresAt: &endsAt}})
		require.NoError(t, err)
		balance(t, w.alpha, customer, 1500, 0, 1500)
		fleet := hostFleet(t, w)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		w.clock.Advance(endsAt.Add(-time.Second).Sub(w.clock.Now()))
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		balance(t, w.alpha, customer, 1500, 0, 1500)
		w.clock.Advance(time.Second)
		balance(t, w.alpha, customer, 500, 0, 1500)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		grant, err := w.alpha.GetCreditGrant(t.Context(), lapsing[0].ID)
		require.NoError(t, err)
		require.EqualValues(t, 1000, grant.ExpiredAmount)
		require.Zero(t, grant.RemainingAmount)
		balance(t, w.alpha, customer, 500, 0, 500)
	})
}
