package money

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/eventstore"
)

// A posting cut behind now can find usage at or after its cut in two
// periods, in whatever order the store groups them: the unposted mark stays
// at the earlier, or that period's tail is never posted.
func TestUnpostedMarkStaysAtTheEarliestPeriodWithLaterUsage(t *testing.T) {
	nov := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	dec := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	rows := []eventstore.PostingQuantity{
		{PeriodStartsAt: dec, Meter: "work", Print: "a", Later: 3},
		{PeriodStartsAt: nov, Meter: "work", Print: "a", Read: eventstore.Quantities{Quantity: 5, Requests: 5}, Later: 2},
		{PeriodStartsAt: nov, Meter: "other", Print: "b", Read: eventstore.Quantities{Quantity: 1, Requests: 1}},
	}
	first, ok := earliestLater(rows)
	require.True(t, ok)
	require.Equal(t, nov, first)

	_, ok = earliestLater(rows[2:])
	require.False(t, ok, "nothing at or after the cut leaves no mark")
}

// Usage an earlier posting may have read is taken at no less than what
// posted: events the store lost are not a price that fell, and usage
// ingested since is priced on top. With nothing lost it is the store's
// total.
func TestTakenQuantitiesNeverFallBelowWhatPosted(t *testing.T) {
	nov := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	posted := periodLines{nov: {lineKey{"work", "", "a"}: {MeterKey: "work", Print: "a", Requests: 10, Quantity: 1000, Stated: 7, Amount: 1000}}}
	taken := func(rows ...eventstore.PostingQuantity) []eventstore.PeriodQuantity {
		return takenQuantities(rows, posted)
	}
	line := func(q, s, r int64) eventstore.PeriodQuantity {
		return eventstore.PeriodQuantity{PeriodStartsAt: nov, Meter: "work", Print: "a", Quantity: q, Stated: s, Requests: r}
	}

	// Nothing lost: what was read and what is fresh, as the store has them.
	whole := eventstore.PostingQuantity{PeriodStartsAt: nov, Meter: "work", Print: "a",
		Read: eventstore.Quantities{Quantity: 1000, Stated: 7, Requests: 10}, Fresh: eventstore.Quantities{Quantity: 50, Stated: 1, Requests: 2}}
	require.Equal(t, []eventstore.PeriodQuantity{line(1050, 8, 12)}, taken(whole))

	// A restore lost 900 of the 1,000 posted, and 100 arrived since: the 100
	// are priced on top of what posted, not taken as the period's total.
	restored := eventstore.PostingQuantity{PeriodStartsAt: nov, Meter: "work", Print: "a",
		Read: eventstore.Quantities{Quantity: 100, Requests: 1}, Fresh: eventstore.Quantities{Quantity: 100, Requests: 1}}
	require.Equal(t, []eventstore.PeriodQuantity{line(1100, 7, 11)}, taken(restored))

	// Another store attached, or the merchant's usage deleted: nothing read
	// is what posted.
	require.Equal(t, []eventstore.PeriodQuantity{line(1000, 7, 10)}, taken())

	// A line no posting took is the store's.
	other := eventstore.PostingQuantity{PeriodStartsAt: nov, Meter: "other", Print: "b", Fresh: eventstore.Quantities{Quantity: 4, Requests: 4}}
	require.Equal(t, []eventstore.PeriodQuantity{
		{PeriodStartsAt: nov, Meter: "other", Print: "b", Quantity: 4, Requests: 4},
		line(1000, 7, 10),
	}, taken(other))

	// Usage only after the cut is nothing to price.
	require.Equal(t, []eventstore.PeriodQuantity{line(1000, 7, 10)}, taken(eventstore.PostingQuantity{PeriodStartsAt: nov, Meter: "late", Print: "c", Later: 3}))
}

// A volume tier's price may still fall on a total no smaller than what
// posted: that is the credit a period carries.
func TestAVolumeCrossingStillCreditsWhatPosted(t *testing.T) {
	nov := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	p := prints(volume(10, 5, 10))
	posted := periodLines{nov: {lineKey{"work", "", "a"}: {MeterKey: "work", Print: "a", Requests: 1, Quantity: 8, Amount: 80}}}
	rows := []eventstore.PostingQuantity{{PeriodStartsAt: nov, Meter: "work", Print: "a",
		Read: eventstore.Quantities{Quantity: 8, Requests: 1}, Fresh: eventstore.Quantities{Quantity: 4, Requests: 1}}}
	target, err := p.priceQuantities(takenQuantities(rows, posted))
	require.NoError(t, err)
	_, amount := postingDelta(target[nov], posted[nov])
	require.EqualValues(t, 12*5-80, amount)

	// The same fall in the store's total from lost events is not one.
	lost := []eventstore.PostingQuantity{{PeriodStartsAt: nov, Meter: "work", Print: "a", Fresh: eventstore.Quantities{Quantity: 1, Requests: 1}}}
	target, err = p.priceQuantities(takenQuantities(lost, posted))
	require.NoError(t, err)
	_, amount = postingDelta(target[nov], posted[nov])
	require.EqualValues(t, 9*10-80, amount, "9 units at the first band: the new unit is charged, nothing is credited")
}
