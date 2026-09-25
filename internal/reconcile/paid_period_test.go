package reconcile

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A renewal advances exactly the periods its charges paid (audit 8); NMI's
// next billing date is adopted only when it lands on that boundary.
func TestPaidPeriod(t *testing.T) {
	start := time.Date(2030, 1, 1, 18, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 30)
	cutoff := end.Add(-24 * time.Hour)
	charge := func(id string, at time.Time) RemoteTransaction {
		return RemoteTransaction{TransactionID: id, SubscriptionID: "rs", Type: TransactionTypeSale, Success: true, OccurredAt: at}
	}
	roster := func(next time.Time) *RemoteSubscription { return &RemoteSubscription{NextBillingAt: &next} }
	one := []RemoteTransaction{charge("a", end)}
	for _, c := range []struct {
		name   string
		txns   []RemoteTransaction
		remote *RemoteSubscription
		want   time.Time
	}{
		{"NMI's date on the paid boundary's day", one, roster(end.AddDate(0, 0, 30).Truncate(24 * time.Hour)), end.AddDate(0, 0, 30).Truncate(24 * time.Hour)},
		{"a calendar month within half a cycle", one, roster(end.AddDate(0, 1, 0)), end.AddDate(0, 1, 0)},
		{"a date a decline moved on is not payment", one, roster(end.AddDate(0, 0, 60)), end.AddDate(0, 0, 30)},
		{"no roster: one cycle per charge", one, nil, end.AddDate(0, 0, 30)},
		{"three charges, three cycles", []RemoteTransaction{charge("a", end), charge("b", end.AddDate(0, 0, 30)), charge("c", end.AddDate(0, 0, 60)), charge("c", end.AddDate(0, 0, 60))},
			roster(end.AddDate(0, 0, 90)), end.AddDate(0, 0, 90)},
		{"an older charge paid an earlier period", []RemoteTransaction{charge("old", start), charge("a", end)}, nil, end.AddDate(0, 0, 30)},
	} {
		t.Run(c.name, func(t *testing.T) {
			from, to := paidPeriod(c.txns, cutoff, &start, &end, c.remote)
			require.Equal(t, end, *from)
			require.Equal(t, c.want, *to)
		})
	}
	next := end.AddDate(0, 0, 30)
	from, to := paidPeriod(one, cutoff, nil, &end, roster(next))
	require.Equal(t, end, *from, "an unknown cycle takes the provider's date")
	require.Equal(t, next, *to)
	from, to = paidPeriod(one, cutoff, nil, &end, nil)
	require.Nil(t, from)
	require.Nil(t, to, "no cycle and no provider date: inconclusive")
}
