//go:build integration

package critical_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// lowAt600 is a merchant amount line of 6.00 USD-units and no runway line:
// a balance is low at or under 599.
func lowAt600(w *world) {
	m := w.cfg.Merchants["alpha"]
	m.MerchantSettings.BalanceNotices = &billing.BalanceNoticeSettings{RunwayHours: ptr(0), LowAmounts: []billing.BalanceNoticeAmount{{Currency: "USD", Amount: 600}}}
	w.cfg.Merchants["alpha"] = m
}

// looks are the balance looks queued for customer.
func looks(t *testing.T, fleet *river.Client[pgx.Tx], customer billing.CustomerID) int {
	t.Helper()
	jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.balance_look").First(1000))
	require.NoError(t, err)
	n := 0
	for _, job := range jobs.Jobs {
		var args struct {
			CustomerID uuid.UUID `json:"customer_id"`
		}
		if json.Unmarshal(job.EncodedArgs, &args) == nil && args.CustomerID == customer.UUID() {
			n++
		}
	}
	return n
}

// settled waits until no look at customer's balance is outstanding.
func settled(t *testing.T, fleet *river.Client[pgx.Tx], customer billing.CustomerID) {
	t.Helper()
	require.Eventually(t, func() bool {
		jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.balance_look").States(
			rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable).First(1000))
		require.NoError(t, err)
		for _, job := range jobs.Jobs {
			var args struct {
				CustomerID uuid.UUID `json:"customer_id"`
			}
			if json.Unmarshal(job.EncodedArgs, &args) == nil && args.CustomerID == customer.UUID() {
				return false
			}
		}
		return true
	}, 10*time.Second, 20*time.Millisecond)
}

// lowNotices counts the customer's low-balance notices.
func lowNotices(t *testing.T, w *world, customer billing.CustomerID) int {
	t.Helper()
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema, "notification_events"}.Sanitize()+
		" WHERE customer_id = $1 AND event_type = 'balance_low'", customer.UUID()).Scan(&n))
	return n
}

func lowEvents(t *testing.T, w *world, customer billing.CustomerID) int {
	t.Helper()
	events, err := w.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"balance.low"}, Limit: 100})
	require.NoError(t, err)
	n := 0
	for _, e := range events.Events {
		var body struct {
			CustomerID string `json:"customer_id"`
		}
		if json.Unmarshal(e.Data, &body) == nil && body.CustomerID == customer.String() {
			n++
		}
	}
	return n
}

// watchedCustomer is a prepaid customer holding amount, its watch's lines
// stored by the look its deposit queued.
func watchedCustomer(t *testing.T, w *world, fleet *river.Client[pgx.Tx], amount int64, expires *time.Time) (billing.CustomerID, billing.CreditGrantID) {
	t.Helper()
	customer := billing.CustomerID(uuid.New())
	grants, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: amount, Source: "critical", SourceID: "funding-" + customer.String(), ExpiresAt: expires}})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		var stored bool
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT floor_amount IS NOT NULL FROM "+pgx.Identifier{w.schema, "balance_watches"}.Sanitize()+" WHERE customer_id = $1", customer.UUID()).Scan(&stored))
		return stored
	}, 10*time.Second, 20*time.Millisecond, "the funding's look stores the lines")
	settled(t, fleet, customer)
	return customer, grants[0].ID
}

func TestBalanceLooks(t *testing.T) {
	setup := func(t *testing.T) (*world, *river.Client[pgx.Tx]) {
		w := newWorld(t, lowAt600)
		meterForCapture(t, w)
		return w, quietFleet(t, w)
	}
	notified := func(t *testing.T, w *world, customer billing.CustomerID) {
		t.Helper()
		require.Eventually(t, func() bool {
			return lowEvents(t, w, customer) == 1 && lowNotices(t, w, customer) == 1
		}, 5*time.Second, 20*time.Millisecond, "the event-log fact and the customer's notice land within seconds")
	}
	t.Run("OR-J25-S40", func(t *testing.T) {
		t.Run("a-hold-that-crosses-the-line-commits-its-look", func(t *testing.T) {
			w, fleet := setup(t)
			customer, _ := watchedCustomer(t, w, fleet, 1000, nil)
			before := looks(t, fleet, customer)
			results, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(w, customer, "crossing-hold", 500)})
			require.NoError(t, err)
			require.True(t, results[0].Allowed())
			require.Equal(t, before+1, looks(t, fleet, customer), "the admission committed the look with the hold")
			notified(t, w, customer)
		})
		t.Run("a-revocation-that-crosses-the-line-commits-its-look", func(t *testing.T) {
			w, fleet := setup(t)
			customer, lot := watchedCustomer(t, w, fleet, 1000, nil)
			_, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 300, Source: "critical", SourceID: "kept"}})
			require.NoError(t, err)
			before := looks(t, fleet, customer)
			_, err = w.alpha.RevokeCreditGrant(t.Context(), lot, billing.RevokeCreditGrantParams{Reason: "critical adjustment"})
			require.NoError(t, err)
			require.Equal(t, before+1, looks(t, fleet, customer), "the revocation committed the look with its debit")
			notified(t, w, customer)
		})
		t.Run("a-lapse-that-crosses-the-line-commits-its-look", func(t *testing.T) {
			w, fleet := setup(t)
			lapse := w.clock.Now().Add(time.Hour)
			customer, _ := watchedCustomer(t, w, fleet, 1000, &lapse)
			_, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 300, Source: "critical", SourceID: "kept"}})
			require.NoError(t, err)
			settled(t, fleet, customer)
			advanceTo(w, lapse)
			before := looks(t, fleet, customer)
			results, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(w, customer, "after-the-lapse", 10)})
			require.NoError(t, err)
			require.True(t, results[0].Allowed())
			require.Greater(t, looks(t, fleet, customer), before, "the lapse admission posted committed its look")
			notified(t, w, customer)
		})
	})
	t.Run("OR-J25-S41/a-debit-that-crosses-no-line-queues-nothing", func(t *testing.T) {
		w, fleet := setup(t)
		customer, _ := watchedCustomer(t, w, fleet, 1000, nil)
		require.Eventually(t, func() bool { return lowEvents(t, w, customer) == 0 && looks(t, fleet, customer) == 1 }, 5*time.Second, 20*time.Millisecond)
		results, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(w, customer, "above-the-line", 400)})
		require.NoError(t, err)
		require.True(t, results[0].Allowed())
		require.Equal(t, 1, looks(t, fleet, customer), "a hold leaving 600 crosses nothing")
		captured, err := w.alpha.CaptureAdmissions(t.Context(), []billing.CaptureAdmissionParams{capture("above-the-line", 400)})
		require.NoError(t, err)
		require.NoError(t, captured[0].Err())
		require.Equal(t, 1, looks(t, fleet, customer), "its capture, a ledger debit to 600, crosses nothing")
		balance(t, w.alpha, customer, 600, 0, 600)
		require.Zero(t, lowEvents(t, w, customer))
	})
	t.Run("OR-J25-S42/a-look-that-fails-is-taken-by-the-next-billing-pass", func(t *testing.T) {
		w, fleet := setup(t)
		customer, _ := watchedCustomer(t, w, fleet, 1000, nil)
		restore := storageFault(t, w, w.schema, "notification_events", "NEW.event_type = 'balance_low'")
		results, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(w, customer, "crossing-while-failing", 500)})
		require.NoError(t, err)
		require.True(t, results[0].Allowed())
		var failed []*rivertype.JobRow
		require.Eventually(t, func() bool {
			jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.balance_look").States(rivertype.JobStateRetryable))
			require.NoError(t, err)
			failed = jobs.Jobs
			return len(failed) > 0
		}, 10*time.Second, 20*time.Millisecond, "the look fails")
		require.Zero(t, lowEvents(t, w, customer))
		// Its retries spent: the watch stays due for the pass.
		for _, job := range failed {
			_, err := fleet.JobCancel(t.Context(), job.ID)
			require.NoError(t, err)
		}
		restore()
		advanceTo(w, nextHour(w.clock.Now()))
		require.False(t, runPass(t, fleet))
		notified(t, w, customer)
	})
}
