//go:build integration

package critical_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func runningChargeWorld(t *testing.T, at ...time.Time) *world {
	t.Helper()
	start := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	if len(at) > 0 {
		start = at[0]
	}
	return newWorld(t, func(w *world) { w.clock.Advance(start.Sub(w.clock.Now())) })
}

func TestRunningChargeContracts(t *testing.T) {
	t.Run("OR-J25-S03", func(t *testing.T) {
		for _, profile := range []string{"before-stop", "after-commit"} {
			t.Run(profile, func(t *testing.T) {
				w := runningChargeWorld(t, time.Date(2026, time.September, 1, 23, 50, 0, 0, time.UTC))
				customer, _ := fundedCustomer(t, w)
				provider := newProviderLedger(t, w)
				input := billing.StartRunningChargeParams{Reference: "crashed-stop", CustomerID: customer, Currency: "USD", RatePerHour: 3600}
				started, err := w.alpha.StartRunningCharge(t.Context(), input)
				require.NoError(t, err)
				w.clock.Advance(900 * time.Second)
				stopAt := w.clock.Now()
				child := startWorkerProcess(t, w, provider, func(c *workerProcessConfig) {
					c.StopReference, c.BeforeStop = input.Reference, profile == "before-stop"
				})
				state, err := w.alpha.GetRunningCharge(t.Context(), input.Reference)
				require.NoError(t, err)
				if profile == "before-stop" {
					require.Equal(t, billing.RunningChargeRunning, state.State)
					require.Zero(t, state.PostedAmount)
				} else {
					require.Equal(t, billing.RunningChargeStopped, state.State)
					require.Zero(t, state.PostedAmount, "a stop posts nothing")
				}
				child.kill(t)
				// A replacement process receives the host's same durable stop time.
				// No application row or financial result is repaired by the fixture.
				w.clock.Advance(time.Hour)
				restarted := startWorkerProcess(t, w, provider, func(c *workerProcessConfig) { c.StopReference, c.At = input.Reference, stopAt })
				final, err := w.alpha.GetRunningCharge(t.Context(), input.Reference)
				require.NoError(t, err)
				require.Equal(t, started.ID, final.ID)
				require.Equal(t, billing.RunningChargeStopped, final.State)
				require.Zero(t, final.PostedAmount)
				require.EqualValues(t, 900, final.ChargedAmount)
				require.WithinDuration(t, stopAt, *final.StoppedAt, time.Microsecond)
				balance(t, w.alpha, customer, 100, 0, 1000)
				restarted.kill(t)
				// Each day's posting takes the charge through its midnight:
				// 23:50-00:05 is ten minutes on the first day's receipt and
				// five on the next, each once.
				fleet := startFleet(t, w)
				postDays(t, fleet)
				require.Equal(t, []int64{-600, 1000}, ledgerAmounts(t, w.alpha, customer))
				balance(t, w.alpha, customer, 100, 0, 400)
				nextDay(w)
				postDays(t, fleet)
				postDays(t, fleet)
				require.Equal(t, []int64{-300, -600, 1000}, ledgerAmounts(t, w.alpha, customer))
				balance(t, w.alpha, customer, 100, 0, 100)
				final, err = w.alpha.GetRunningCharge(t.Context(), input.Reference)
				require.NoError(t, err)
				require.EqualValues(t, 900, final.PostedAmount)
			})
		}
	})
	t.Run("OR-J25-S01/whole-seconds-post-once-at-explicit-stop", func(t *testing.T) {
		w := runningChargeWorld(t)
		customer, _ := fundedCustomer(t, w)
		input := billing.StartRunningChargeParams{Reference: "compute/session-1", CustomerID: customer, Currency: "USD", RatePerHour: 3600, MinimumAmount: 1000, Resource: "gpu"}
		started, err := w.alpha.StartRunningCharge(t.Context(), input)
		require.NoError(t, err)
		require.Equal(t, billing.RunningChargeRunning, started.State)
		require.EqualValues(t, 0, started.ChargedAmount)
		w.clock.Advance(900*time.Second + 500*time.Millisecond)
		accruing, err := w.alpha.GetRunningCharge(t.Context(), input.Reference)
		require.NoError(t, err)
		require.EqualValues(t, 900, accruing.ChargedAmount)
		require.EqualValues(t, 0, accruing.PostedAmount)
		b, err := w.alpha.GetBalance(t.Context(), customer, billing.GetBalanceParams{Currency: "USD"})
		require.NoError(t, err)
		require.EqualValues(t, 1000, b.BalanceAmount)
		require.EqualValues(t, 900, b.AccruedAmount)
		require.EqualValues(t, 100, b.AvailableAmount)
		startedAt := accruing.StartedAt
		stopped, err := w.alpha.StopRunningCharge(t.Context(), input.Reference, billing.StopRunningChargeParams{StoppedAt: ptr(w.clock.Now())})
		require.NoError(t, err)
		require.Equal(t, billing.RunningChargeStopped, stopped.State)
		require.EqualValues(t, 900, stopped.ChargedAmount)
		require.Zero(t, stopped.PostedAmount)
		balance(t, w.alpha, customer, 100, 0, 1000)
		w.clock.Advance(2 * time.Hour)
		final, err := w.alpha.GetRunningCharge(t.Context(), input.Reference)
		require.NoError(t, err)
		require.EqualValues(t, 900, final.ChargedAmount)
		require.Equal(t, stopped.StoppedAt, final.StoppedAt)
		// The day's posting takes the exact segment once, as a charge line of
		// its receipt naming the charge and its resource.
		fleet := startFleet(t, w)
		nextDay(w)
		postDays(t, fleet)
		postDays(t, fleet)
		balance(t, w.alpha, customer, 100, 0, 100)
		ledger, err := w.alpha.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		require.Len(t, ledger.Items, 2)
		require.ElementsMatch(t, []int64{1000, -900}, []int64{ledger.Items[0].Amount, ledger.Items[1].Amount})
		receipt := ledger.Items[0].Usage
		require.NotNil(t, receipt)
		require.Empty(t, receipt.Lines)
		require.Equal(t, []billing.UsageReceiptCharge{{RunningChargeID: started.ID, Reference: input.Reference, Resource: ptr("gpu"),
			StartsAt: startedAt, EndsAt: *stopped.StoppedAt, Amount: 900}}, receipt.Charges)
		final, err = w.alpha.GetRunningCharge(t.Context(), input.Reference)
		require.NoError(t, err)
		require.EqualValues(t, 900, final.PostedAmount)
		_, err = w.beta.GetRunningCharge(t.Context(), input.Reference)
		require.ErrorIs(t, err, billing.ErrNotFound)
	})
	t.Run("OR-J25-S02/concurrent-stops-retain-one-final-segment", func(t *testing.T) {
		w := runningChargeWorld(t)
		customer, _ := fundedCustomer(t, w)
		input := billing.StartRunningChargeParams{Reference: "concurrent-stop", CustomerID: customer, Currency: "USD", RatePerHour: 3600}
		started, err := w.alpha.StartRunningCharge(t.Context(), input)
		require.NoError(t, err)
		w.clock.Advance(300 * time.Second)
		stop := billing.StopRunningChargeParams{StoppedAt: ptr(w.clock.Now())}
		type outcome struct {
			charge *billing.RunningCharge
			err    error
		}
		begin := make(chan struct{})
		result := make(chan outcome, 6)
		for range 6 {
			go func() {
				<-begin
				charge, err := w.alpha.StopRunningCharge(t.Context(), input.Reference, stop)
				result <- outcome{charge, err}
			}()
		}
		close(begin)
		for range 6 {
			done := <-result
			require.NoError(t, done.err)
			require.Equal(t, started.ID, done.charge.ID)
			require.EqualValues(t, 300, done.charge.ChargedAmount)
			require.Zero(t, done.charge.PostedAmount)
		}
		balance(t, w.alpha, customer, 700, 0, 1000)
		late := billing.StopRunningChargeParams{StoppedAt: ptr(w.clock.Now().Add(time.Second))}
		_, err = w.alpha.StopRunningCharge(t.Context(), input.Reference, late)
		require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
		code, ok := billing.ErrorCodeFrom(err)
		require.True(t, ok)
		require.Equal(t, billing.CodeIdempotencyKeyReused, code)
		// The in-process refusal names the same code.
		tx, err := w.pool.Begin(t.Context())
		require.NoError(t, err)
		_, err = w.local.StopRunningChargeTx(t.Context(), tx, input.Reference, late)
		require.NoError(t, tx.Rollback(t.Context()))
		require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
		code, ok = billing.ErrorCodeFrom(err)
		require.True(t, ok)
		require.Equal(t, billing.CodeIdempotencyKeyReused, code)
		replay, err := w.alpha.StartRunningCharge(t.Context(), input)
		require.NoError(t, err)
		require.Equal(t, billing.RunningChargeStopped, replay.State)
		require.EqualValues(t, 300, replay.ChargedAmount)
		input.RatePerHour = 7200
		_, err = w.alpha.StartRunningCharge(t.Context(), input)
		require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
		balance(t, w.alpha, customer, 700, 0, 1000)
		fleet := startFleet(t, w)
		nextDay(w)
		postDays(t, fleet)
		postDays(t, fleet)
		require.Equal(t, []int64{-300, 1000}, ledgerAmounts(t, w.alpha, customer), "one segment, posted once")
		balance(t, w.alpha, customer, 700, 0, 700)
	})
	t.Run("OR-J25-S04/depletion-notifies-host-and-explicit-stop-never-reopens", func(t *testing.T) {
		w := runningChargeWorld(t)
		customer, _ := fundedCustomer(t, w)
		input := billing.StartRunningChargeParams{Reference: "host-work", CustomerID: customer, Currency: "USD", RatePerHour: 3600, MinimumAmount: 1001}
		_, err := w.alpha.StartRunningCharge(t.Context(), input)
		require.ErrorIs(t, err, billing.ErrInsufficientCredits)
		list, err := w.alpha.ListRunningCharges(t.Context(), billing.RunningChargeListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Empty(t, list.Items)
		input.MinimumAmount = 1000
		_, err = w.alpha.StartRunningCharge(t.Context(), input)
		require.NoError(t, err)
		w.clock.Advance(1000 * time.Second)
		b, err := w.alpha.GetBalance(t.Context(), customer, billing.GetBalanceParams{Currency: "USD"})
		require.NoError(t, err)
		require.EqualValues(t, 0, b.AvailableAmount)
		list, err = w.alpha.ListRunningCharges(t.Context(), billing.RunningChargeListParams{CustomerID: customer, Depleted: true})
		require.NoError(t, err)
		require.Len(t, list.Items, 1)
		require.Equal(t, input.Reference, list.Items[0].Reference)
		fleet := startFleet(t, w)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		events, err := w.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{"balance.depleted"}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, events.Events, 1)
		// The host owns stopping the resource. OpenRails reports depletion and
		// continues accounting actual elapsed work until this explicit stop.
		stopped, err := w.alpha.StopRunningCharge(t.Context(), input.Reference, billing.StopRunningChargeParams{})
		require.NoError(t, err)
		require.EqualValues(t, 1000, stopped.ChargedAmount)
		w.clock.Advance(time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		final, err := w.alpha.GetRunningCharge(t.Context(), input.Reference)
		require.NoError(t, err)
		require.Equal(t, billing.RunningChargeStopped, final.State)
		require.EqualValues(t, 1000, final.ChargedAmount)
		// Its accrual is pending until the day's posting takes it.
		balance(t, w.alpha, customer, 0, 0, 1000)
		events, err = w.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{"balance.depleted"}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, events.Events, 1)
	})
}
