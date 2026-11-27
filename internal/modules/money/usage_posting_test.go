package money

import (
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
// one total per line, less the price of the posted row alone, which is what
// the period's earlier receipts took: two leases' usage at a graduated card
// crosses the band once, however it was split, and a volume tier's fall is a
// negative line.
func TestAPostingPricesThePeriodsTotalLessWhatPosted(t *testing.T) {
	nov := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	row := func(lease uuid.UUID, quantity, requests int64) gen.BillingUsageTotal {
		return gen.BillingUsageTotal{PeriodStartsAt: nov, MeterKey: "work", Print: "a", LeaseID: lease, Quantity: quantity, Requests: requests}
	}
	posted, taken := periodTotals{}, periodTotals{}
	posted.add(nov, row(uuid.Nil, 80, 8))
	taken.add(nov, row(uuid.Nil, 80, 8))
	taken.add(nov, row(uuid.New(), 15, 3))
	taken.add(nov, row(uuid.New(), 15, 2))
	require.Equal(t, []periodQuantity{{period: nov, meter: "work", print: "a", quantity: 110, requests: 13}}, taken.quantities(nov))
	ends := map[time.Time]time.Time{nov: nov.AddDate(0, 1, 0)}

	r := &receipt{}
	require.NoError(t, r.priceUsage(prints(graduated(10, 5, 100)), posted, taken, ends))
	require.Equal(t, []receiptLine{{PeriodStartsAt: nov, PeriodEndsAt: ends[nov],
		PostingLine: PostingLine{MeterKey: "work", Print: "a", Requests: 5, Quantity: 30, Amount: 20*10 + 10*5}}}, r.lines, "the band is crossed once, at the period's total")

	r = &receipt{}
	require.NoError(t, r.priceUsage(prints(volume(10, 5, 100)), posted, taken, ends))
	spent, credited := r.spent()
	require.Equal(t, [2]int64{0, 800 - 110*5}, [2]int64{spent, credited}, "the fall is credited at the posting that takes it")
}

// One receipt holds both signs, and its falls are settled first: a period
// whose price fell is credited, and the rises and running charges are spent
// from the lots and that credit. In a merchant's unit a falling period first
// takes back at most what it forgave before, from its negative lines, and
// what neither the lots nor the credit can take is forgiven across the rising
// periods by their amounts and each period's across its positive lines.
func TestAReceiptCreditsFallsBeforeItSpendsRises(t *testing.T) {
	oct, nov := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	line := func(period time.Time, meter string, amount int64) receiptLine {
		return receiptLine{PeriodStartsAt: period, PeriodEndsAt: period.AddDate(0, 1, 0), PostingLine: PostingLine{MeterKey: meter, Amount: amount}}
	}
	both := func(charges ...receiptCharge) *receipt {
		return &receipt{
			lines:   []receiptLine{line(oct, "images", -30), line(nov, "images", 60), line(nov, "words", -20), line(nov, "words", 40)},
			periods: []periodNet{{start: oct, first: 0, last: 1, amount: -30}, {start: nov, first: 1, last: 4, amount: 80}},
			charges: charges,
		}
	}
	r := both(receiptCharge{Amount: 20, StartsAt: nov.Add(time.Hour)})
	spend, credit := r.settle(false, 0, nil)
	require.Equal(t, [2]int64{100, 30}, [2]int64{spend, credit}, "a registry currency forgives nothing")
	require.Equal(t, []int64{0, 0, 0, 0}, forgiven(r))

	// October forgave 12 before: its fall of 30 takes it back first and
	// credits 18; the lots' 20 and that 18 pay 38 of November's 80, and
	// 42 is forgiven over its positive lines, 60:40, to the largest remainder.
	r = both()
	spend, credit = r.settle(true, 20, map[time.Time]int64{oct: 12})
	require.Equal(t, [2]int64{38, 18}, [2]int64{spend, credit})
	require.Equal(t, []int64{-12, 25, 0, 17}, forgiven(r))

	// With empty lots, a fall of 20 pays a rise of 15 and nothing is forgiven.
	small := &receipt{
		lines:   []receiptLine{line(oct, "images", -20), line(nov, "words", 15)},
		periods: []periodNet{{start: oct, first: 0, last: 1, amount: -20}, {start: nov, first: 1, last: 2, amount: 15}},
	}
	spend, credit = small.settle(true, 0, nil)
	require.Equal(t, [2]int64{15, 20}, [2]int64{spend, credit})
	require.Equal(t, []int64{0, 0}, forgiven(small), "the fall's credit pays the rise")

	r = both(receiptCharge{Amount: 20, StartsAt: nov.Add(time.Hour)})
	cut, through := nov.AddDate(0, 0, 3), nov.AddDate(0, 0, 2)
	shares := r.owedShares(func(end time.Time) time.Time { return earlier(end, cut).Add(-time.Microsecond) }, through.Add(-time.Microsecond))
	require.Equal(t, []owedShare{{source: "metered:images", weight: 60, from: nov, at: cut.Add(-time.Microsecond)},
		{source: "metered:words", weight: 40, from: nov, at: cut.Add(-time.Microsecond)},
		{source: runningChargeOwedSource, weight: 20, from: nov.Add(time.Hour), at: through.Add(-time.Microsecond)}}, shares,
		"usage owed per rising period and meter, dated by its period's end and the cut; running charges by their own instant")
}

func forgiven(r *receipt) []int64 {
	out := make([]int64, len(r.lines))
	for i, l := range r.lines {
		out[i] = l.Forgiven
	}
	return out
}
