package money

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/eventstore"
)

func graduated(first, rest int64, upTo int64) catalog.RatePrice {
	return catalog.RatePrice{Model: catalog.ModelTiered, Tiered: &catalog.TieredPrice{Mode: catalog.TierModeGraduated,
		Tiers: []catalog.RateTier{{UpTo: &upTo, UnitAmount: first}, {UnitAmount: rest}}}}
}

func volume(first, rest int64, upTo int64) catalog.RatePrice {
	return catalog.RatePrice{Model: catalog.ModelTiered, Tiered: &catalog.TieredPrice{Mode: catalog.TierModeVolume,
		Tiers: []catalog.RateTier{{UpTo: &upTo, UnitAmount: first}, {UnitAmount: rest}}}}
}

func perUnit(amount int64) catalog.RatePrice {
	return catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{UnitAmount: amount, DivideBy: 1}}
}

func prints(cards ...catalog.RatePrice) usagePrints {
	out := usagePrints{}
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i, price := range cards {
		out[string(rune('a'+i))] = printCard{card: ratingCard{MeterKey: "work", Aggregation: catalog.AggregationSum, Price: price}, order: base.Add(time.Duration(i) * time.Hour)}
	}
	return out
}

// A period's price is of its total, however its usage arrived: a graduated
// card prices the same total the same, a later print prices its own
// quantity on top of the earlier ones', and a volume tier reprices the
// period when its total reaches a cheaper band.
func TestCellPriceIsThePriceOfTheTotal(t *testing.T) {
	p := prints(graduated(10, 5, 100))
	one, err := p.cellPrice("", []printQuantity{{print: "a", quantity: 150}})
	require.NoError(t, err)
	require.Equal(t, []int64{100*10 + 50*5}, one)

	// A card revision mid-period: the second print starts where the first
	// left the period, segment by segment.
	revised := prints(graduated(10, 5, 100), perUnit(2))
	amounts, err := revised.cellPrice("", []printQuantity{{print: "b", quantity: 30}, {print: "a", quantity: 120}})
	require.NoError(t, err)
	require.Equal(t, []int64{100*10 + 20*5, 30 * 2}, amounts, "print order, not the order given")

	// A volume crossing: 90 units cost 90 each; at 110 the whole period
	// costs the cheaper band.
	v := prints(volume(10, 5, 100))
	before, err := v.cellPrice("", []printQuantity{{print: "a", quantity: 90}})
	require.NoError(t, err)
	after, err := v.cellPrice("", []printQuantity{{print: "a", quantity: 110}})
	require.NoError(t, err)
	require.EqualValues(t, 900, before[0])
	require.EqualValues(t, 550, after[0])

	// A dynamic card charges what was stated.
	d := prints(catalog.RatePrice{Model: catalog.ModelDynamic})
	stated, err := d.cellPrice("", []printQuantity{{print: "a", quantity: 7, stated: 450}})
	require.NoError(t, err)
	require.Equal(t, []int64{450}, stated)

	_, err = p.cellPrice("", []printQuantity{{print: "unknown", quantity: 1}})
	require.Error(t, err, "a print with no card is not priced")
}

// A posting takes the price less what earlier postings took, line by line;
// a volume crossing leaves a negative remainder.
func TestPostingDeltaTakesWhatIsLeft(t *testing.T) {
	period := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	p := prints(volume(10, 5, 100))
	target, err := p.priceQuantities([]eventstore.PeriodQuantity{{PeriodStartsAt: period, Meter: "work", Print: "a", Quantity: 90, Requests: 3}})
	require.NoError(t, err)
	lines, amount := postingDelta(target[period], nil)
	require.EqualValues(t, 900, amount)
	require.Len(t, lines, 1)
	posted := map[lineKey]*PostingLine{{"work", "", "a"}: &lines[0]}
	again, err := p.priceQuantities([]eventstore.PeriodQuantity{{PeriodStartsAt: period, Meter: "work", Print: "a", Quantity: 90, Requests: 3}})
	require.NoError(t, err)
	none, amount := postingDelta(again[period], posted)
	require.Empty(t, none)
	require.Zero(t, amount, "a repeated read posts nothing")
	crossed, err := p.priceQuantities([]eventstore.PeriodQuantity{{PeriodStartsAt: period, Meter: "work", Print: "a", Quantity: 110, Requests: 4}})
	require.NoError(t, err)
	reversal, amount := postingDelta(crossed[period], posted)
	require.EqualValues(t, 550-900, amount)
	require.Equal(t, []PostingLine{{MeterKey: "work", Print: "a", Requests: 1, Quantity: 20, Amount: -350}}, reversal)
}

// A day's amount is the price through the day less the price through the
// day before, so the days of a period sum to what it posts; a day's rows
// share it by quantity.
func TestHistoryDaysSumToThePeriodsPrice(t *testing.T) {
	period := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	day := func(d int) time.Time { return period.AddDate(0, 0, d) }
	customer := billing.CustomerID{1}
	row := func(d int, invoker string, quantity int64) eventstore.UsageDay {
		return eventstore.UsageDay{CustomerID: customer, Day: day(d), Currency: "USD", EventType: "work", Meter: "work", Print: "a",
			PeriodStartsAt: period, Invoker: invoker, Outcome: eventstore.UsageSucceeded, Requests: 1, Quantity: quantity}
	}
	p := prints(graduated(10, 5, 100))
	prior := []eventstore.UsageDay{row(0, "x", 80)}
	days := []eventstore.UsageDay{row(1, "x", 10), row(1, "y", 30), row(2, "x", 40),
		{CustomerID: customer, Day: day(2), Currency: "USD", EventType: "work", PeriodStartsAt: period, Invoker: "x", Outcome: eventstore.UsageFailed, Requests: 1, Charged: 7}}
	priced, err := p.priceHistory(days, prior)
	require.NoError(t, err)
	amounts := map[string]int64{}
	var total int64
	for _, r := range priced {
		amounts[r.Day.Format("02")+"/"+r.Invoker+"/"+string(r.Outcome)] += r.Amount
		total += r.Amount
	}
	// Through November 1: 80 units, 800. November 2 takes the period to 120
	// units (1100), November 3 to 160 (1300).
	require.EqualValues(t, 75, amounts["02/x/succeeded"], "November 2's 300, shared 10:30")
	require.EqualValues(t, 225, amounts["02/y/succeeded"])
	require.EqualValues(t, 200, amounts["03/x/succeeded"], "40 units at the second tier")
	require.EqualValues(t, 7, amounts["03/x/failed"], "a failure is what it charged")
	require.EqualValues(t, 1300-800+7, total, "the days sum to what the period adds")
}
