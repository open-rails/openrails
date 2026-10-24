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
		job, err := fleet.Insert(t.Context(), registeredJob{KindName: "openrails.credit_expiry"}, &river.InsertOpts{Queue: openrails.QueueBilling})
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
				t.Fatal("registered credit expiry worker did not finish")
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
}
