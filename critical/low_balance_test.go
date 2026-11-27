//go:build integration

package critical_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// balanceOutbox is the data of the customer's host events of eventType, in
// the order they were recorded: written in the look's transaction.
func balanceOutbox(t *testing.T, w *world, customer billing.CustomerID, eventType string) []map[string]any {
	t.Helper()
	rows, err := w.pool.Query(t.Context(), "SELECT data FROM "+pgx.Identifier{w.schema, "host_outbox"}.Sanitize()+
		" WHERE subject_id = $1 AND event_type = $2 ORDER BY recorded_at", customer.UUID(), eventType)
	require.NoError(t, err)
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		require.NoError(t, rows.Scan(&raw))
		var data map[string]any
		require.NoError(t, json.Unmarshal(raw, &data))
		out = append(out, data)
	}
	require.NoError(t, rows.Err())
	return out
}

// watchNext is when the customer's balance watch next looks; nil with
// nothing scheduled.
func watchNext(t *testing.T, w *world, customer billing.CustomerID) *time.Time {
	t.Helper()
	var next *time.Time
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT next_check_at FROM "+pgx.Identifier{w.schema, "balance_watches"}.Sanitize()+
		" WHERE customer_id = $1", customer.UUID()).Scan(&next))
	return next
}

// atNoon starts the world's clock at a noon (UTC), hours from any midnight.
func atNoon(w *world) {
	noon := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	w.clock.Advance(noon.Sub(w.clock.Now()))
}

func TestLowBalanceAmounts(t *testing.T) {
	t.Run("OR-J25-S47/usage-that-leaves-the-amount-is-low", func(t *testing.T) {
		w := newWorld(t, lowAt600)
		meterForCapture(t, w)
		fleet := quietFleet(t, w)
		customer, _ := watchedCustomer(t, w, fleet, 1000, nil)
		spend(t, w.alpha, customer, "to-the-amount", 400)
		settled(t, fleet, customer)
		low := balanceOutbox(t, w, customer, "balance.low")
		require.Len(t, low, 1, "a balance at its amount is low")
		require.Equal(t, "600", low[0]["available_amount"])
		require.Equal(t, "600", low[0]["low_balance_amount"])
	})
	t.Run("OR-J25-S48/a-running-charge-is-low-at-the-amount-at-its-exact-instant-not-a-runway-ahead", func(t *testing.T) {
		w := newWorld(t, atNoon, lowAt600)
		fleet := quietFleet(t, w)
		customer, _ := fundedCustomer(t, w)
		start := w.clock.Now()
		_, err := w.alpha.StartRunningCharge(t.Context(), billing.StartRunningChargeParams{Reference: "low-at-the-amount", CustomerID: customer, Currency: "USD", RatePerHour: 100})
		require.NoError(t, err)
		settled(t, fleet, customer)
		require.Empty(t, balanceOutbox(t, w, customer, "balance.low"), "a day of spend at its rate is no line: 1000 is over 600")
		// 100 an hour takes 1000 to 600 in four hours exactly.
		lowAt := start.Add(4 * time.Hour)
		next := watchNext(t, w, customer)
		require.NotNil(t, next)
		require.True(t, lowAt.Equal(*next), "the look is scheduled for the crossing's instant: %s", next)
		advanceTo(w, lowAt.Add(-time.Second))
		require.False(t, runPass(t, fleet))
		require.Empty(t, balanceOutbox(t, w, customer, "balance.low"), "a second before it, nothing")
		advanceTo(w, lowAt)
		require.False(t, runPass(t, fleet))
		low := balanceOutbox(t, w, customer, "balance.low")
		require.Len(t, low, 1, "at the instant, once")
		require.Equal(t, "600", low[0]["available_amount"])
		require.Equal(t, "600", low[0]["low_balance_amount"])
		require.Equal(t, "100", low[0]["accrual_rate_per_hour"])
	})
	t.Run("OR-J25-S49/with-no-amount-set-a-running-charge-is-told-only-when-depleted-at-its-exact-instant", func(t *testing.T) {
		w := newWorld(t, atNoon)
		fleet := quietFleet(t, w)
		customer, _ := fundedCustomer(t, w)
		start := w.clock.Now()
		_, err := w.alpha.StartRunningCharge(t.Context(), billing.StartRunningChargeParams{Reference: "no-amount", CustomerID: customer, Currency: "USD", RatePerHour: 100})
		require.NoError(t, err)
		settled(t, fleet, customer)
		require.Empty(t, balanceOutbox(t, w, customer, "balance.low"), "no amount, no low line")
		depletesAt := start.Add(10 * time.Hour)
		next := watchNext(t, w, customer)
		require.NotNil(t, next)
		require.True(t, depletesAt.Equal(*next), "the only look scheduled is the depletion's instant: %s", next)
		advanceTo(w, depletesAt)
		require.False(t, runPass(t, fleet))
		depleted := balanceOutbox(t, w, customer, "balance.depleted")
		require.Len(t, depleted, 1)
		require.Equal(t, "0", depleted[0]["available_amount"])
		require.Equal(t, "0", depleted[0]["low_balance_amount"])
		require.Empty(t, balanceOutbox(t, w, customer, "balance.low"))
		require.Zero(t, lowNotices(t, w, customer), "the customer has no low notice either")
	})
}
