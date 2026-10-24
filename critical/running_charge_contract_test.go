//go:build integration

package critical_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestRunningChargeContracts(t *testing.T) {
	t.Run("OR-J25-S01/whole-seconds-post-once-at-explicit-stop", func(t *testing.T) {
		w := newWorld(t)
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
		b, err := w.alpha.GetBalance(t.Context(), customer, "USD")
		require.NoError(t, err)
		require.EqualValues(t, 1000, b.BalanceAmount)
		require.EqualValues(t, 900, b.AccruedAmount)
		require.EqualValues(t, 100, b.AvailableAmount)
		stopped, err := w.alpha.StopRunningCharge(t.Context(), input.Reference, billing.StopRunningChargeParams{StoppedAt: ptr(w.clock.Now())})
		require.NoError(t, err)
		require.Equal(t, billing.RunningChargeStopped, stopped.State)
		require.EqualValues(t, 900, stopped.ChargedAmount)
		require.EqualValues(t, 900, stopped.PostedAmount)
		balance(t, w.alpha, customer, 100, 0, 100)
		w.clock.Advance(2 * time.Hour)
		final, err := w.alpha.GetRunningCharge(t.Context(), input.Reference)
		require.NoError(t, err)
		require.EqualValues(t, 900, final.ChargedAmount)
		require.Equal(t, stopped.StoppedAt, final.StoppedAt)
		ledger, err := w.alpha.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		require.Len(t, ledger.Items, 2)
		require.ElementsMatch(t, []int64{1000, -900}, []int64{ledger.Items[0].Amount, ledger.Items[1].Amount})
		_, err = w.beta.GetRunningCharge(t.Context(), input.Reference)
		require.ErrorIs(t, err, billing.ErrNotFound)
	})
	t.Run("OR-J25-S02/concurrent-stops-retain-one-final-segment", func(t *testing.T) {
		w := newWorld(t)
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
			require.EqualValues(t, 300, done.charge.PostedAmount)
		}
		balance(t, w.alpha, customer, 700, 0, 700)
		ledger, err := w.alpha.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		require.Len(t, ledger.Items, 2)
		_, err = w.alpha.StopRunningCharge(t.Context(), input.Reference, billing.StopRunningChargeParams{StoppedAt: ptr(w.clock.Now().Add(time.Second))})
		require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
		replay, err := w.alpha.StartRunningCharge(t.Context(), input)
		require.NoError(t, err)
		require.Equal(t, billing.RunningChargeStopped, replay.State)
		require.EqualValues(t, 300, replay.ChargedAmount)
		input.RatePerHour = 7200
		_, err = w.alpha.StartRunningCharge(t.Context(), input)
		require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
		balance(t, w.alpha, customer, 700, 0, 700)
	})
	t.Run("OR-J25-S04/depletion-notifies-host-and-explicit-stop-never-reopens", func(t *testing.T) {
		w := newWorld(t)
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
		b, err := w.alpha.GetBalance(t.Context(), customer, "USD")
		require.NoError(t, err)
		require.EqualValues(t, 0, b.AvailableAmount)
		list, err = w.alpha.ListRunningCharges(t.Context(), billing.RunningChargeListParams{CustomerID: customer, Depleted: true})
		require.NoError(t, err)
		require.Len(t, list.Items, 1)
		require.Equal(t, input.Reference, list.Items[0].Reference)
		fleet := startFleet(t, w)
		runRegistered(t, fleet, "openrails.balance_watch", 2)
		events, err := w.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"balance.depleted"}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, events.Events, 1)
		// The host owns stopping the resource. OpenRails reports depletion and
		// continues accounting actual elapsed work until this explicit stop.
		stopped, err := w.alpha.StopRunningCharge(t.Context(), input.Reference, billing.StopRunningChargeParams{})
		require.NoError(t, err)
		require.EqualValues(t, 1000, stopped.ChargedAmount)
		w.clock.Advance(time.Hour)
		runRegistered(t, fleet, "openrails.balance_watch", 2)
		final, err := w.alpha.GetRunningCharge(t.Context(), input.Reference)
		require.NoError(t, err)
		require.Equal(t, billing.RunningChargeStopped, final.State)
		require.EqualValues(t, 1000, final.ChargedAmount)
		balance(t, w.alpha, customer, 0, 0, 0)
		events, err = w.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"balance.depleted"}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, events.Events, 1)
	})
}
