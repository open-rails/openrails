//go:build e2e && integration

package ci_test

import (
	"math"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

func dynamicPrice() catalog.RatePrice { return catalog.RatePrice{Model: catalog.ModelDynamic} }

func TestDynamicCardChargesTheEventAmount(t *testing.T) {
	f := newFixture(t)
	bill, client := f.runtimeWithStripe(t, "dynamic-"+uuid.NewString()[:8], nil)
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "usage", DisplayName: "Usage"})
	require.NoError(t, err)
	p := &pricing{t: t, ctx: t.Context(), client: client, product: product.ID}
	p.meter("gpu-seconds", "gpu", catalog.AggregationSum, "seconds", nil, dynamicPrice(), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1_000_000)
	u := usage(customer, "gpu", map[string]int64{"seconds": 10}, nil)
	u.Amount = new(int64(250_000))
	event := p.record(u)
	require.EqualValues(t, 250_000, event.Amount)
	require.Equal(t, []billing.UsageCharge{{MeterKey: "gpu-seconds", Model: catalog.ModelDynamic, Quantity: 10, Amount: 250_000}}, event.Charges)
	require.EqualValues(t, 750_000, p.balance(customer).BalanceAmount)
	rows := readUsageAs(t, bill, customer).report(url.Values{"group_by": {"meter"}}).Rows
	require.Len(t, rows, 1)
	require.EqualValues(t, 10, *rows[0].Quantity)
	require.EqualValues(t, 25_000, rows[0].Amount / *rows[0].Quantity, "the report preserves the implied native-unit price")
}

func TestDynamicEventWithoutAmountChargesZero(t *testing.T) {
	p := newPricing(t, newFixture(t), "dynamic-missing")
	p.meter("units", "job", catalog.AggregationSum, "units", nil, dynamicPrice(), nil)
	u := usage(billing.CustomerID(uuid.New()), "job", map[string]int64{"units": 3}, nil)
	event := p.record(u)
	require.Zero(t, event.Amount)
	require.Equal(t, []billing.UsageCharge{{MeterKey: "units", Model: catalog.ModelDynamic, Quantity: 3}}, event.Charges)
}

func TestDynamicEventWithZeroUnitsChargesZero(t *testing.T) {
	p := newPricing(t, newFixture(t), "dynamic-zero")
	p.meter("units", "job", catalog.AggregationSum, "units", nil, dynamicPrice(), nil)
	u := usage(billing.CustomerID(uuid.New()), "job", map[string]int64{"units": 0}, nil)
	u.Amount = new(int64(500_000))
	require.Zero(t, p.record(u).Amount)
	u.SourceID = uuid.NewString()
	u.Dimensions = nil
	require.Zero(t, p.record(u).Amount, "missing units are zero units")
}

func TestCatalogCardIgnoresAStatedAmount(t *testing.T) {
	p := newPricing(t, newFixture(t), "catalog-stated")
	p.meter("units", "job", catalog.AggregationSum, "units", nil, perUnit(10, 1), nil)
	u := usage(billing.CustomerID(uuid.New()), "job", map[string]int64{"units": 3}, nil)
	u.Amount = new(int64(999))
	event := p.record(u)
	require.EqualValues(t, 30, event.Amount)
	require.Equal(t, catalog.ModelPerUnit, event.Charges[0].Model)
}

func TestDynamicAndCatalogCardsOnOneEventTypeEachCharge(t *testing.T) {
	p := newPricing(t, newFixture(t), "dynamic-mixed")
	p.meter("host", "job", catalog.AggregationSum, "units", nil, dynamicPrice(), nil)
	p.meter("catalog", "job", catalog.AggregationSum, "units", nil, perUnit(10, 1), nil)
	u := usage(billing.CustomerID(uuid.New()), "job", map[string]int64{"units": 3}, nil)
	u.Amount = new(int64(999))
	event := p.record(u)
	require.EqualValues(t, 1029, event.Amount)
	require.Equal(t, map[string]int64{"host": 999, "catalog": 30}, charged(event))
	require.EqualValues(t, 1029, p.balance(u.CustomerID).OwedAmount)
}

func TestDynamicCardRequiresASumMeter(t *testing.T) {
	p := newPricing(t, newFixture(t), "dynamic-sum")
	card := billing.MeterRateCardParams{ProductID: p.product, Price: catalog.RatePrice{Model: catalog.ModelDynamic, Currency: "USD"}}
	_, err := p.client.SetMeter(p.ctx, "count", billing.SetMeterParams{Aggregation: catalog.AggregationCount, RateCard: catalog.Value(card)})
	require.ErrorIs(t, err, billing.ErrInvalid)
	require.ErrorContains(t, err, "sum meter")
	p.meter("sum", "job", catalog.AggregationSum, "units", nil, dynamicPrice(), nil)
	_, err = p.client.SetMeter(p.ctx, "sum", billing.SetMeterParams{Aggregation: catalog.AggregationCount, RateCard: catalog.Value(card)})
	require.Error(t, err, "a meter edit cannot turn its dynamic card into a count price")
}

func TestDynamicCardRefusesAllowance(t *testing.T) {
	p := newPricing(t, newFixture(t), "dynamic-allowance")
	_, err := p.client.SetMeter(p.ctx, "units", billing.SetMeterParams{
		Aggregation: catalog.AggregationSum, ValueProperty: "units",
		RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: p.product,
			Price: catalog.RatePrice{Model: catalog.ModelDynamic, Currency: "USD"}, Allowance: &catalog.Allowance{Included: 1}}),
	})
	require.ErrorIs(t, err, billing.ErrInvalid)
	require.ErrorContains(t, err, "cannot have an allowance")
	document, err := catalog.ParseApplicationYAML([]byte(`schema_version: 1
meters:
  units:
    aggregation: sum
    value_property: units
products:
  dynamic:
    display_name: Dynamic
    rate_cards:
    - meter: units
      allowance: {included: 1}
      price: {model: dynamic, currency: USD}
`))
	require.NoError(t, err)
	_, err = p.client.ApplyCatalog(p.ctx, document, billing.ApplyCatalogParams{})
	require.ErrorIs(t, err, billing.ErrInvalid)
	require.ErrorContains(t, err, "cannot have an allowance")
}

func TestDynamicCaptureSettlesTheStatedAmount(t *testing.T) {
	p := newPricing(t, newFixture(t), "dynamic-capture")
	p.meter("dynamic", "job", catalog.AggregationSum, "units", nil, dynamicPrice(), nil)
	p.meter("catalog", "job", catalog.AggregationSum, "units", nil, perUnit(10, 1), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1000)
	admit := p.admitUsage(customer, "request", billing.UsageEstimate{EventType: "job", Dimensions: map[string]int64{"units": 5}, Amount: new(int64(500))})
	require.True(t, admit.Allowed(), "%+v", admit)
	require.EqualValues(t, 550, admit.Admission.EstimatedAmount)
	changed := p.admitUsage(customer, "request", billing.UsageEstimate{EventType: "job", Dimensions: map[string]int64{"units": 5}, Amount: new(int64(501))})
	require.NotNil(t, changed.Error)
	require.Equal(t, billing.CodeIdempotencyKeyReused, changed.Error.Code)
	p.meter("dynamic", "job", catalog.AggregationSum, "units", nil, perUnit(999, 1), nil)
	capture := billing.CaptureAdmissionParams{RequestID: "request", Amount: new(int64(200)), Usage: billing.CaptureUsage{EventType: "job", Dimensions: map[string]int64{"units": 2}}}
	result := p.capture(capture)[0]
	require.Equal(t, http.StatusOK, result.Status, "%+v", result.Error)
	require.Equal(t, map[string]int64{"dynamic": 200, "catalog": 20}, receiptCharges(result.Receipt), "the admitted dynamic model is pinned")
	require.EqualValues(t, 220, result.Receipt.Amount)
	require.EqualValues(t, 780, p.balance(customer).BalanceAmount)
	replay := p.capture(capture)[0]
	require.NoError(t, replay.Err())
	require.True(t, replay.Receipt.Replayed)
	require.Equal(t, result.Receipt.Charges, replay.Receipt.Charges)
	capture.Amount = new(int64(201))
	require.ErrorIs(t, p.capture(capture)[0].Err(), billing.ErrIdempotencyKeyReused)
	require.ErrorIs(t, p.capture(billing.CaptureAdmissionParams{RequestID: "request", Amount: new(int64(220))})[0].Err(), billing.ErrInvalid, "a bare amount is no longer a capture")
}

func TestUsageRetryWithAnotherAmountIsRefused(t *testing.T) {
	p := newPricing(t, newFixture(t), "dynamic-replay")
	p.meter("units", "job", catalog.AggregationSum, "units", nil, dynamicPrice(), nil)
	u := usage(billing.CustomerID(uuid.New()), "job", map[string]int64{"units": 1}, nil)
	u.Amount = new(int64(9007199254740993))
	first := p.record(u)
	require.EqualValues(t, 9007199254740993, first.Amount)
	require.True(t, p.record(u).Replayed)
	u.Amount = new(int64(9007199254740992))
	_, err := recordUsage(p.ctx, p.client, u)
	require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
	u.SourceID, u.Amount = uuid.NewString(), nil
	p.record(u)
	u.Amount = new(int64(0))
	_, err = recordUsage(p.ctx, p.client, u)
	require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused, "missing and explicit zero are distinct stated terms")
}

func TestFailedUsageKeepsItsStatedCost(t *testing.T) {
	p := newPricing(t, newFixture(t), "dynamic-failed")
	p.meter("catalog", "job", catalog.AggregationSum, "units", nil, perUnit(100, 1), nil)
	u := usage(billing.CustomerID(uuid.New()), "job", map[string]int64{"units": 20}, nil)
	u.Outcome, u.Amount = billing.UsageFailed, new(int64(300))
	event := p.record(u)
	require.Zero(t, event.Amount, "no grace windows forgives the failure")
	require.EqualValues(t, 300, event.ForgivenAmount, "the catalog never reprices failed work")
	require.Empty(t, event.Charges)
	u.SourceID, u.Amount = uuid.NewString(), nil
	_, err := recordUsage(p.ctx, p.client, u)
	require.ErrorIs(t, err, billing.ErrInvalid, "failed usage must explicitly state its cost")
}

func TestDynamicChargesCannotOverflowTheEventTotal(t *testing.T) {
	p := newPricing(t, newFixture(t), "dynamic-overflow")
	p.meter("host", "job", catalog.AggregationSum, "units", nil, dynamicPrice(), nil)
	p.meter("tiered", "job", catalog.AggregationSum, "units", nil, catalog.RatePrice{
		Model: catalog.ModelTiered, Tiered: &catalog.TieredPrice{Mode: catalog.TierModeGraduated,
			Tiers: []catalog.RateTier{{UpTo: new(int64(1)), UnitAmount: 2}, {UnitAmount: 1}}},
	}, nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1000)
	u := usage(customer, "job", map[string]int64{"units": 1}, nil)
	u.Amount = new(int64(math.MaxInt64))
	_, err := recordUsage(p.ctx, p.client, u)
	require.Error(t, err)
	require.EqualValues(t, 1000, p.balance(customer).BalanceAmount)
	u.Amount = new(int64(0))
	event := p.record(u)
	require.EqualValues(t, 2, event.Amount, "the catalog still charges its first tier: the refused event did not advance its quantity")
	require.Equal(t, map[string]int64{"host": 0, "tiered": 2}, charged(event))
	require.EqualValues(t, 998, p.balance(customer).BalanceAmount)
}
