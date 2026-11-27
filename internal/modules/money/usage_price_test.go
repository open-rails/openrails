package money

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/catalog"
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

// A posting takes the price less what earlier receipts took, line by line;
// a volume crossing leaves a negative remainder.
func TestPostingDeltaTakesWhatIsLeft(t *testing.T) {
	period := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	p := prints(volume(10, 5, 100))
	target, err := p.priceQuantities([]periodQuantity{{period: period, meter: "work", print: "a", quantity: 90, requests: 3}})
	require.NoError(t, err)
	lines, amount := postingDelta(target[period], nil)
	require.EqualValues(t, 900, amount)
	require.Len(t, lines, 1)
	posted := map[lineKey]*PostingLine{{"work", "", "a"}: &lines[0]}
	again, err := p.priceQuantities([]periodQuantity{{period: period, meter: "work", print: "a", quantity: 90, requests: 3}})
	require.NoError(t, err)
	none, amount := postingDelta(again[period], posted)
	require.Empty(t, none)
	require.Zero(t, amount, "a repeated read posts nothing")
	crossed, err := p.priceQuantities([]periodQuantity{{period: period, meter: "work", print: "a", quantity: 110, requests: 4}})
	require.NoError(t, err)
	reversal, amount := postingDelta(crossed[period], posted)
	require.EqualValues(t, 550-900, amount)
	require.Equal(t, []PostingLine{{MeterKey: "work", Print: "a", Requests: 1, Quantity: 20, Amount: -350}}, reversal)
}

// A zone's days are bounded by its own midnights: Kolkata's fall on the
// half hour of UTC.
func TestLocalDaysAreTheZonesMidnights(t *testing.T) {
	at := func(day, hour, minute int) time.Time { return time.Date(2026, 11, day, hour, minute, 0, 0, time.UTC) }
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	from, to := LocalDays(kolkata, at(1, 20, 0), at(1, 20, 1))
	require.Equal(t, at(1, 18, 30), from, "the Kolkata day 20:00 UTC falls in starts at 18:30 UTC")
	require.Equal(t, at(2, 18, 30), to)
}
