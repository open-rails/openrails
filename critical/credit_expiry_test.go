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
	t.Run("OR-J22-S03/usage-before-a-lapse-spends-the-lot-before-it-expires", func(t *testing.T) {
		w := newWorld(t)
		meterForCapture(t, w)
		customer := billing.CustomerID(uuid.New())
		expiring, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "expiring", ExpiresAt: ptr(w.clock.Now().Add(time.Hour))}})
		require.NoError(t, err)
		spend(t, w.alpha, customer, "already-spent", 450)
		w.clock.Advance(time.Second)
		_, err = w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 500, Source: "critical", SourceID: "lasting"}})
		require.NoError(t, err)
		// The usage is pending; the grant did not post it.
		balance(t, w.alpha, customer, 1050, 450, 1500)
		w.clock.Advance(time.Hour)
		runRegistered(t, startFleet(t, w), "openrails.billing_pass", 1)
		// The expiry posted the usage first, through the lapse: it drew the
		// expiring lot, and only the rest expired.
		grant, err := w.alpha.GetCreditGrant(t.Context(), expiring[0].ID)
		require.NoError(t, err)
		require.EqualValues(t, 450, grant.SpentAmount)
		require.EqualValues(t, 550, grant.ExpiredAmount)
		require.Zero(t, grant.RemainingAmount)
		balance(t, w.alpha, customer, 500, 0, 500)
		// Usage after the lapse is pending against what is left.
		spend(t, w.alpha, customer, "after-the-lapse", 100)
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
	t.Run("OR-J22-S50/a-lapsed-lot-counts-until-its-look-expires-it", func(t *testing.T) {
		w := newWorld(t)
		meterForCapture(t, w)
		customer := billing.CustomerID(uuid.New())
		_, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 500, Source: "critical", SourceID: "lasting"}})
		require.NoError(t, err)
		endsAt := w.clock.Now().Add(time.Hour)
		_, err = w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "lapsing", ExpiresAt: &endsAt}})
		require.NoError(t, err)
		// No look has run: the lapsed lot is in the balance, and a view
		// offers it, until its expiry posts.
		w.clock.Advance(time.Hour + time.Second)
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Views: []billing.ViewParams{computeView(customer)}})
		require.EqualValues(t, 1500, lineOf(t, viewOf(t, out, customer), catalog.BalanceLimit).Remaining)
		balance(t, w.alpha, customer, 1500, 0, 1500)
		runRegistered(t, startFleet(t, w), "openrails.billing_pass", 1)
		out = syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Views: []billing.ViewParams{computeView(customer)}})
		require.EqualValues(t, 500, lineOf(t, viewOf(t, out, customer), catalog.BalanceLimit).Remaining)
		balance(t, w.alpha, customer, 500, 0, 500)
	})
	t.Run("OR-J22-S06/a-lapsed-lot-leaves-the-balance-when-its-watch-expires-it", func(t *testing.T) {
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
		// At the lapse the lot is still in the balance; its look expires it.
		w.clock.Advance(time.Second)
		balance(t, w.alpha, customer, 1500, 0, 1500)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		grant, err := w.alpha.GetCreditGrant(t.Context(), lapsing[0].ID)
		require.NoError(t, err)
		require.EqualValues(t, 1000, grant.ExpiredAmount)
		require.Zero(t, grant.RemainingAmount)
		balance(t, w.alpha, customer, 500, 0, 500)
	})
}
