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
		out[string(rune('a'+i))] = printCard{card: ratingCard{MeterKey: "work", Aggregation: catalog.AggregationSum, Price: price}, invoice: base.Add(time.Duration(i) * time.Hour)}
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
	require.Equal(t, []int64{100*10 + 20*5, 30 * 2}, amounts, "print order, not the invoice given")

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

// A bucket's amount is the price through the bucket less the price through
// the bucket before, so the buckets of a period sum to what it posts, and so
// do the days of a zone they fold into; a bucket's rows share it by quantity.
func TestHistoryHoursFoldIntoZonesDays(t *testing.T) {
	period := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	at := func(day, hour, minute int) time.Time { return time.Date(2026, 11, day, hour, minute, 0, 0, time.UTC) }
	customer := billing.CustomerID{1}
	row := func(bucket time.Time, invoker string, quantity int64) eventstore.UsageBucket {
		return eventstore.UsageBucket{CustomerID: customer, BucketAt: bucket, Currency: "USD", EventType: "work", Meter: "work", Print: "a",
			PeriodStartsAt: period, Invoker: invoker, Outcome: eventstore.UsageSucceeded, Requests: 1, Quantity: quantity}
	}
	p := prints(graduated(10, 5, 100))
	prior := []eventstore.UsageBucket{row(at(1, 6, 0), "x", 80)}
	buckets := []eventstore.UsageBucket{row(at(1, 18, 0), "x", 10), row(at(1, 19, 0), "x", 10), row(at(1, 19, 0), "y", 20), row(at(2, 3, 0), "x", 40),
		{CustomerID: customer, BucketAt: at(2, 3, 0), Currency: "USD", EventType: "work", PeriodStartsAt: period, Invoker: "x", Outcome: eventstore.UsageFailed, Requests: 1, Charged: 7}}
	priced, err := p.priceHistory(buckets, prior)
	require.NoError(t, err)
	byBucket := map[string]int64{}
	for _, r := range priced {
		byBucket[r.BucketAt.Format("02T15:04")+"/"+r.Invoker+"/"+string(r.Outcome)] += r.Amount
	}
	// Before the first bucket: 80 units, 800. 18:00 takes the period to 90
	// units (900), 19:00 to 120 (1100), 03:00 to 160 (1300).
	require.EqualValues(t, 100, byBucket["01T18:00/x/succeeded"])
	require.EqualValues(t, 66, byBucket["01T19:00/x/succeeded"], "19:00's 200, shared 10:20")
	require.EqualValues(t, 134, byBucket["01T19:00/y/succeeded"])
	require.EqualValues(t, 200, byBucket["02T03:00/x/succeeded"], "40 units at the second tier")
	require.EqualValues(t, 7, byBucket["02T03:00/x/failed"], "a failure is what it charged")

	kolkata, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	for _, c := range []struct {
		zone *time.Location
		days map[string]int64
	}{
		// The 18:00 UTC hour starts at 23:30 in Kolkata, 19:00 UTC at 00:30
		// the next day: Kolkata's days are cut at the hour.
		{kolkata, map[string]int64{"01": 100, "02": 407}},
		{time.UTC, map[string]int64{"01": 300, "02": 207}},
	} {
		days := map[string]int64{}
		var total int64
		for _, r := range priced {
			days[r.BucketAt.In(c.zone).Format("02")] += r.Amount
			total += r.Amount
		}
		require.Equal(t, c.days, days, c.zone.String())
		require.EqualValues(t, 1300-800+7, total, "the days sum to what the period adds")
	}
	from, to := LocalDays(kolkata, at(1, 20, 0), at(1, 20, 1))
	require.Equal(t, at(1, 18, 30), from, "the Kolkata day 20:00 UTC falls in starts at 18:30 UTC")
	require.Equal(t, at(2, 18, 30), to)
}
