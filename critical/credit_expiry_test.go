//go:build integration

package critical_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// The host queue's public persisted kind is an external integration boundary;
// the actual registered worker decodes the payload and performs the work.
type registeredJob struct {
	KindName string `json:"-"`
}

func (j registeredJob) Kind() string { return j.KindName }

func TestCreditExpiry(t *testing.T) {
	t.Run("OR-J22-S03/expiry-preserves-spent-value-and-live-funded-reservation", func(t *testing.T) {
		w := newWorld(t)
		meterForCapture(t, w)
		customer := billing.CustomerID(uuid.New())
		expiring, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "expiring", ExpiresAt: ptr(w.clock.Now().Add(time.Hour))}})
		require.NoError(t, err)
		first, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(w, customer, "already-spent", 600)})
		require.NoError(t, err)
		require.True(t, first[0].Allowed())
		spent, err := w.alpha.CaptureAdmissions(t.Context(), []billing.CaptureAdmissionParams{capture("already-spent", 450)})
		require.NoError(t, err)
		require.NoError(t, spent[0].Err())
		_, err = w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 500, Source: "critical", SourceID: "lasting"}})
		require.NoError(t, err)
		pending := reservation(w, customer, "still-authorized", 400)
		pending.ExpiresAt = ptr(w.clock.Now().Add(2 * time.Hour))
		admitted, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{pending})
		require.NoError(t, err)
		require.True(t, admitted[0].Allowed())
		balance(t, w.alpha, customer, 650, 400, 1050)
		fleet := hostFleet(t, w)
		done, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
		defer unsubscribe()
		w.clock.Advance(time.Hour)
		job, err := fleet.Insert(t.Context(), registeredJob{KindName: "openrails.balance_watch"}, &river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
		require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		timer := time.NewTimer(15 * time.Second)
		defer timer.Stop()
		finished := false
		for !finished {
			select {
			case event := <-done:
				if event.Job.ID != job.Job.ID {
					continue
				}
				require.Equal(t, river.EventKindJobCompleted, event.Kind)
				finished = true
			case <-timer.C:
				t.Fatal("registered balance watch worker did not finish")
			}
		}
		grant, err := w.alpha.GetCreditGrant(t.Context(), expiring[0].ID)
		require.NoError(t, err)
		require.EqualValues(t, 450, grant.SpentAmount)
		require.EqualValues(t, 550, grant.ExpiredAmount)
		require.Zero(t, grant.RemainingAmount)
		balance(t, w.alpha, customer, 100, 400, 500)
		held, err := w.alpha.GetAdmission(t.Context(), "still-authorized")
		require.NoError(t, err)
		require.Equal(t, billing.AdmissionOpen, *held.State)
		captured, err := w.alpha.CaptureAdmissions(t.Context(), []billing.CaptureAdmissionParams{capture("still-authorized", 100)})
		require.NoError(t, err)
		require.NoError(t, captured[0].Err())
		balance(t, w.alpha, customer, 400, 0, 400)
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
	t.Run("OR-J22-S50/admission-refuses-credit-that-lapsed-before-its-watch-looks", func(t *testing.T) {
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
		refused, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(w, customer, "past-the-lapse", 1000)})
		require.NoError(t, err)
		require.False(t, refused[0].Allowed(), "credit that lapsed is not spent")
		admitted, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(w, customer, "within-what-lasts", 500)})
		require.NoError(t, err)
		require.True(t, admitted[0].Allowed())
		balance(t, w.alpha, customer, 0, 500, 500)
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
		runRegistered(t, fleet, "openrails.balance_watch", 1)
		w.clock.Advance(endsAt.Add(-time.Second).Sub(w.clock.Now()))
		runRegistered(t, fleet, "openrails.balance_watch", 1)
		balance(t, w.alpha, customer, 1500, 0, 1500)
		w.clock.Advance(time.Second)
		balance(t, w.alpha, customer, 500, 0, 1500)
		runRegistered(t, fleet, "openrails.balance_watch", 1)
		grant, err := w.alpha.GetCreditGrant(t.Context(), lapsing[0].ID)
		require.NoError(t, err)
		require.EqualValues(t, 1000, grant.ExpiredAmount)
		require.Zero(t, grant.RemainingAmount)
		balance(t, w.alpha, customer, 500, 0, 500)
	})
}
