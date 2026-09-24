package money

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/pkg/pricing"
)

func TestInvoicePeriods(t *testing.T) {
	d := func(y int, m time.Month, day, h, mi int) time.Time {
		return time.Date(y, m, day, h, mi, 0, 0, time.UTC)
	}
	anchor := d(2026, 1, 31, 9, 30)
	for _, tt := range []struct {
		name, boundary string
		now, anchor    time.Time
		from, to       time.Time
	}{
		{"calendar month", " calendar_month ", d(2026, 6, 29, 12, 0), time.Time{}, d(2026, 5, 1, 0, 0), d(2026, 6, 1, 0, 0)},
		{"calendar month crosses year", InvoiceBoundaryCalendarMonth, d(2026, 1, 15, 0, 0), time.Time{}, d(2025, 12, 1, 0, 0), d(2026, 1, 1, 0, 0)},
		{"anniversary clamps to short month", InvoiceBoundaryAnniversary, d(2026, 3, 30, 10, 0), anchor, anchor, d(2026, 2, 28, 9, 30)},
		{"anniversary after this month's date", InvoiceBoundaryAnniversary, d(2026, 3, 31, 10, 0), anchor, d(2026, 2, 28, 9, 30), d(2026, 3, 31, 9, 30)},
	} {
		from, to, err := PreviousInvoicePeriod(tt.now, tt.anchor, tt.boundary)
		require.NoError(t, err, tt.name)
		require.Equal(t, tt.from, from, tt.name)
		require.Equal(t, tt.to, to, tt.name)
	}

	// Fixed interval (also the blank default): epoch-aligned, exactly one interval, ending at or before now.
	now := d(2026, 6, 29, 12, 0)
	from, to, err := PreviousInvoicePeriod(now, time.Time{}, "")
	require.NoError(t, err)
	require.Equal(t, fixedInvoiceInterval, to.Sub(from))
	require.Zero(t, to.Unix()%int64(fixedInvoiceInterval/time.Second))
	require.False(t, to.After(now))
	require.True(t, to.Add(fixedInvoiceInterval).After(now))

	_, _, err = PreviousInvoicePeriod(now, time.Time{}, "weekly")
	require.Error(t, err)
	for in, want := range map[string]string{
		"": InvoiceBoundaryFixedInterval, " Calendar_Month ": InvoiceBoundaryCalendarMonth,
		"anniversary": InvoiceBoundaryAnniversary, "weekly": "",
	} {
		require.Equal(t, want, NormalizeInvoiceBoundary(in), in)
	}
}

// Validation runs before the (nil) query handle is touched.
func TestPendingInvoiceItemGuards(t *testing.T) {
	ctx, merchantID, customerID := context.Background(), uuid.New(), uuid.New()
	at := time.Now()
	require.NoError(t, insertPendingInvoiceItemTx(ctx, nil, merchantID, customerID, "USD", "usage", "r-1", 0, at, nil), "zero amount is a no-op")
	require.EqualError(t, insertPendingInvoiceItemTx(ctx, nil, merchantID, customerID, "USD", " ", "r-1", 100, at, nil),
		"invoice item source_type and source_id required")
	require.EqualError(t, insertPendingInvoiceItemTx(ctx, nil, merchantID, customerID, "USD", "usage", "r-1", 100, time.Time{}, nil),
		"invoice item timestamp required")
}

func TestPayerRateCardOverrideKeepsDefaultMeterContract(t *testing.T) {
	perUnit := func(amount int64) pricing.RatePrice {
		return pricing.RatePrice{Model: pricing.ModelPerUnit, Currency: "USD", PerUnit: &pricing.PerUnitPrice{UnitAmount: amount}}
	}
	def := catalogRateCardRow{
		ID: uuid.New(), MeterKey: "requests", EventType: "request.completed", ValueKey: "$.units",
		Aggregation: pricing.AggregationSum, GroupBy: map[string]string{"region": "$.region"},
		Filter: map[string][]string{"region": {"eu"}}, Allowance: &pricing.Allowance{Included: 5}, Price: perUnit(10_000),
	}
	other := catalogRateCardRow{ID: uuid.New(), MeterKey: "storage", Price: perUnit(1)}
	override := catalogRateCardRow{
		ID: uuid.New(), MeterKey: "requests", PayerScoped: true, EventType: "poisoned.event",
		GroupBy: map[string]string{"poisoned": "$.poisoned"}, Filter: map[string][]string{"poisoned": {"x"}},
		Allowance: &pricing.Allowance{Included: 20}, Price: perUnit(4_000),
	}

	resolved, err := resolvePayerRateCardOverrides([]catalogRateCardRow{override, def, other})
	require.NoError(t, err)
	want := def
	want.ID, want.PayerScoped, want.Price, want.Allowance = override.ID, true, override.Price, override.Allowance
	require.Equal(t, []catalogRateCardRow{want, other}, resolved)

	_, err = resolvePayerRateCardOverrides([]catalogRateCardRow{override, other})
	require.ErrorIs(t, err, ErrDefaultRateCardRequired)
}

func TestRateCardFilterRules(t *testing.T) {
	rules, err := rateCardFilterRules(catalogRateCardRow{
		ID:      uuid.New(),
		GroupBy: map[string]string{"region": "metadata.region", "plan": "$.plan", "tier": "dimensions.tier"},
		Filter:  map[string][]string{"region": {"eu"}, "plan": {"pro", "team"}, "tier": {"gold"}},
	})
	require.NoError(t, err)
	require.ElementsMatch(t, []usageFilterRule{
		{PropertyKey: "region", AllowedValues: []string{"eu"}},
		{PropertyKey: "plan", AllowedValues: []string{"pro", "team"}},
		{PropertyKey: "tier", AllowedValues: []string{"gold"}},
	}, rules)

	_, err = rateCardFilterRules(catalogRateCardRow{ID: uuid.New(), Filter: map[string][]string{"region": {"eu"}}})
	require.ErrorContains(t, err, `filter dimension "region" has no meter property`)
}

func TestMeteringPageBounds(t *testing.T) {
	for _, tt := range []struct{ limit, offset, wantLimit, wantOffset int }{
		{0, -1, defaultMeteringPageSize, 0},
		{maxMeteringPageSize + 1, 1, maxMeteringPageSize, 1},
		{1, math.MaxInt, 1, math.MaxInt32},
	} {
		limit, offset := normalizeMeteringPage(tt.limit, tt.offset)
		require.Equal(t, tt.wantLimit, limit)
		require.Equal(t, tt.wantOffset, offset)
		require.Equal(t, int32(tt.wantOffset), meteringPageInt32(offset))
	}
	require.Equal(t, int32(0), meteringPageInt32(-5))
	require.Equal(t, int32(math.MaxInt32), meteringPageInt32(math.MaxInt))
}

func TestAllowanceSourcePrice(t *testing.T) {
	meter := pricing.Meter{
		Key: "runtime", Aggregation: pricing.AggregationSum,
		GroupBy: map[string]string{"size": "metadata.size", "resource_id": "metadata.resource_id"},
	}
	matrix := func(cells map[string]pricing.MatrixCell) pricing.RatePrice {
		return pricing.RatePrice{Model: pricing.ModelPerUnit, Currency: "USD", PerUnit: &pricing.PerUnitPrice{
			Matrix: &pricing.Matrix{Dimension: "size", Cells: cells},
		}}
	}
	valid := matrix(map[string]pricing.MatrixCell{"small": {UnitAmount: 10_000, Included: 100}, "large": {UnitAmount: 1}})
	require.NoError(t, validateAllowanceSourcePrice(meter, valid, "USD"))

	withMeter := func(edit func(*pricing.Meter)) pricing.Meter {
		m := meter
		m.GroupBy = map[string]string{"size": "metadata.size", "resource_id": "metadata.resource_id"}
		edit(&m)
		return m
	}
	for name, tt := range map[string]struct {
		meter    pricing.Meter
		price    pricing.RatePrice
		currency string
	}{
		"unsupported aggregation": {withMeter(func(m *pricing.Meter) { m.Aggregation = pricing.AggregationMax }), valid, "USD"},
		"currency mismatch":       {meter, valid, "EUR"},
		"non-matrix price": {meter, pricing.RatePrice{
			Model: pricing.ModelPerUnit, Currency: "USD", PerUnit: &pricing.PerUnitPrice{UnitAmount: 10_000},
		}, "USD"},
		"missing resource dimension": {withMeter(func(m *pricing.Meter) { delete(m.GroupBy, "resource_id") }), valid, "USD"},
		"missing matrix dimension":   {withMeter(func(m *pricing.Meter) { delete(m.GroupBy, "size") }), valid, "USD"},
		"no included cells":          {meter, matrix(map[string]pricing.MatrixCell{"small": {UnitAmount: 10_000}}), "USD"},
	} {
		require.ErrorIs(t, validateAllowanceSourcePrice(tt.meter, tt.price, tt.currency), ErrAllowanceSourceInvalid, name)
	}
}
