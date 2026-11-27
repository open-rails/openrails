package money

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
)

// Usage counts in the lapse segment it occurred in: the latest lapse at or
// before it, else its period's start. A lapse at the usage's own instant has
// passed; one before its period is the period's start.
func TestUsageCountsInTheLapseSegmentItOccurredIn(t *testing.T) {
	nov := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	at := func(day int) time.Time { return nov.AddDate(0, 0, day-1) }
	lapses := []time.Time{at(1).Add(-time.Hour), at(5), at(20)}
	require.Equal(t, nov, lapseSegment(lapses, nov, at(3)), "a lapse before the period is its start")
	require.Equal(t, at(5), lapseSegment(lapses, nov, at(5)), "a lapse at the instant has passed")
	require.Equal(t, at(5), lapseSegment(lapses, nov, at(19)))
	require.Equal(t, at(20), lapseSegment(lapses, nov, at(25)))
	require.Equal(t, nov, lapseSegment(nil, nov, at(25)))
}

// A posting prices a period's posted row and the unposted rows it takes as
// one total per line, less what the period's receipts took: two leases'
// usage at a graduated card crosses the band once, however it was split, and
// a volume tier's fall comes back as a negative line.
func TestAPostingPricesThePeriodsTotalLessItsReceipts(t *testing.T) {
	nov := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	row := func(lease uuid.UUID, quantity, requests int64) gen.BillingUsageTotal {
		return gen.BillingUsageTotal{PeriodStartsAt: nov, MeterKey: "work", Print: "a", LeaseID: lease, Quantity: quantity, Requests: requests}
	}
	taken := periodTotals{}
	taken.add(nov, row(uuid.Nil, 80, 8))
	taken.add(nov, row(uuid.New(), 15, 3))
	taken.add(nov, row(uuid.New(), 15, 2))
	require.Equal(t, []periodQuantity{{period: nov, meter: "work", print: "a", quantity: 110, requests: 13}}, taken.quantities(nov))

	lines, err := json.Marshal([]PostingLine{{MeterKey: "work", Print: "a", Requests: 8, Quantity: 80, Amount: 800}})
	require.NoError(t, err)
	posted, forgiven, err := receiptLines([]gen.BillingUsageReceipt{{PeriodStartsAt: nov, Lines: lines}})
	require.NoError(t, err)
	require.Empty(t, forgiven)

	target, err := prints(graduated(10, 5, 100)).priceQuantities(taken.quantities(nov))
	require.NoError(t, err)
	delta, amount := postingDelta(target[nov], posted[nov])
	require.EqualValues(t, 20*10+10*5, amount, "the band is crossed once, at the period's total")
	require.Equal(t, []PostingLine{{MeterKey: "work", Print: "a", Requests: 5, Quantity: 30, Amount: 250}}, delta)

	target, err = prints(volume(10, 5, 100)).priceQuantities(taken.quantities(nov))
	require.NoError(t, err)
	_, amount = postingDelta(target[nov], posted[nov])
	require.EqualValues(t, 110*5-800, amount, "the fall is credited at the posting that takes it")
}
