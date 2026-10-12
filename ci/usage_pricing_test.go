//go:build e2e && integration

package ci_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/money"
)

// pricing is a merchant whose meters each have a rate card under one product,
// and the usage it records.
type pricing struct {
	t       *testing.T
	ctx     context.Context
	engine  *openrails.Engine
	client  *openrails.Client
	product billing.ProductID
	started time.Time
}

func newPricing(t *testing.T, f *fixture, slug string) *pricing {
	t.Helper()
	engine, client := f.runtimeWithStripe(t, slug+"-"+uuid.NewString()[:8], nil)
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "usage", DisplayName: "Usage"})
	require.NoError(t, err)
	return &pricing{t: t, ctx: t.Context(), engine: engine, client: client, product: product.ID, started: time.Now().UTC().Truncate(time.Second)}
}

// meter declares a meter of eventType and its rate card.
func (p *pricing) meter(key, eventType string, aggregation catalog.Aggregation, valueProperty string, groupBy map[string]string, price catalog.RatePrice, allowance *catalog.Allowance) {
	p.t.Helper()
	price.Currency = "USD"
	_, err := p.client.SetMeter(p.ctx, key, billing.SetMeterParams{
		EventType: eventType, Aggregation: aggregation, ValueProperty: valueProperty, GroupBy: groupBy,
		RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: p.product, Price: price, Allowance: allowance}),
	})
	require.NoError(p.t, err)
}

func perUnit(unitAmount, divideBy int64) catalog.RatePrice {
	return catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{UnitAmount: unitAmount, DivideBy: divideBy}}
}

func (p *pricing) grant(customer billing.CustomerID, amount int64) {
	p.t.Helper()
	_, err := createCreditGrant(p.ctx, p.client, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: amount, Source: "test", SourceID: uuid.NewString()})
	require.NoError(p.t, err)
}

func (p *pricing) balance(customer billing.CustomerID) *billing.Balance {
	p.t.Helper()
	balance, err := p.client.GetBalance(p.ctx, customer, "USD")
	require.NoError(p.t, err)
	return balance
}

// usage is one event of eventType with dimensions and metadata.
func usage(customer billing.CustomerID, eventType string, dimensions map[string]int64, metadata map[string]any) billing.RecordUsageParams {
	return billing.RecordUsageParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: eventType,
		Dimensions: dimensions, Metadata: metadata, Source: "test", SourceID: uuid.NewString()}
}

func (p *pricing) record(params billing.RecordUsageParams) *billing.UsageEvent {
	p.t.Helper()
	event, err := recordUsage(p.ctx, p.client, params)
	require.NoError(p.t, err)
	return event
}

// charged is an event's charges as meter -> amount.
func charged(event *billing.UsageEvent) map[string]int64 {
	out := map[string]int64{}
	for _, c := range event.Charges {
		out[c.MeterKey] += c.Amount
	}
	return out
}

// A per-million-token card prices each event as it is recorded: the prepaid
// balance drops by its charge at once, and the event says what each meter
// charged. An event no priced meter consumes is metered only.
func TestUsageIsPricedWhenRecorded(t *testing.T) {
	p := newPricing(t, newFixture(t), "priced")
	p.meter("input-tokens", "llm.request", catalog.AggregationSum, "input_tokens", nil, perUnit(150_000, 1_000_000), nil)
	p.meter("output-tokens", "llm.request", catalog.AggregationSum, "output_tokens", nil, perUnit(600_000, 1_000_000), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 10_000_000)

	first := p.record(usage(customer, "llm.request", map[string]int64{"input_tokens": 10_000, "output_tokens": 2_000}, nil))
	require.Equal(t, map[string]int64{"input-tokens": 1_500, "output-tokens": 1_200}, charged(first))
	require.EqualValues(t, 2_700, first.Amount)
	require.Len(t, first.Charges, 2)
	for _, c := range first.Charges {
		require.Nil(t, c.Cell)
		require.Equal(t, map[string]int64{"input-tokens": 10_000, "output-tokens": 2_000}[c.MeterKey], c.Quantity)
	}
	require.NotNil(t, first.BalanceTransactionID)
	require.EqualValues(t, 9_997_300, p.balance(customer).BalanceAmount)

	// 13,333 input tokens cost round(1,999.95) = 2,000; 2,001 output 1,201.
	second := p.record(usage(customer, "llm.request", map[string]int64{"input_tokens": 3_333, "output_tokens": 1}, nil))
	require.Equal(t, map[string]int64{"input-tokens": 500, "output-tokens": 1}, charged(second))
	require.EqualValues(t, 9_996_799, p.balance(customer).BalanceAmount)

	free := p.record(usage(customer, "unmetered", map[string]int64{"input_tokens": 1_000_000}, nil))
	require.Empty(t, free.Charges)
	require.Zero(t, free.Amount)
	require.Nil(t, free.BalanceTransactionID)
	require.EqualValues(t, 9_996_799, p.balance(customer).BalanceAmount)
}

// Charges telescope: 1,000 events of 1,234 tokens at $0.15 per million each
// cost 185.1 micros, rounded where they fall, and together cost exactly
// 185,100: the price of 1,234,000 tokens.
func TestChargesTelescopeToThePeriodPrice(t *testing.T) {
	p := newPricing(t, newFixture(t), "telescope")
	p.meter("tokens", "llm.request", catalog.AggregationSum, "tokens", nil, perUnit(150_000, 1_000_000), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1_000_000)

	items := make([]billing.RecordUsageParams, 1_000)
	for i := range items {
		items[i] = usage(customer, "llm.request", map[string]int64{"tokens": 1_234}, nil)
	}
	results, err := p.client.RecordUsage(p.ctx, items)
	require.NoError(t, err)
	var total int64
	for _, r := range results {
		require.Equal(t, http.StatusCreated, r.Status, "%+v", r.Error)
		require.Contains(t, []int64{185, 186}, r.Event.Amount)
		total += r.Event.Amount
	}
	require.EqualValues(t, 185_100, total)
	require.EqualValues(t, 1_000_000-185_100, p.balance(customer).BalanceAmount)
}

// Graduated tiers, packages with free units, a per-period maximum and an
// included allowance each charge an event what it adds to the period's price.
// An event of an earlier period is priced from that period's quantity, which
// starts at zero.
func TestTieredPackageAndMaximumPriceMarginally(t *testing.T) {
	p := newPricing(t, newFixture(t), "marginal")
	two := int64(2)
	p.meter("calls", "calls", catalog.AggregationCount, "", nil, catalog.RatePrice{Model: catalog.ModelTiered, Tiered: &catalog.TieredPrice{
		Mode: catalog.TierModeGraduated, Tiers: []catalog.RateTier{{UpTo: &two, UnitAmount: 1_000}, {UnitAmount: 500}},
	}}, nil)
	p.meter("storage", "storage", catalog.AggregationSum, "bytes", nil, catalog.RatePrice{Model: catalog.ModelPackage, Package: &catalog.PackagePrice{
		Amount: 7_000, PackageSize: 1_000, FreeUnits: 500,
	}}, nil)
	capped := perUnit(10, 0)
	capped.PerUnit.MaximumAmount = 25_000
	p.meter("compute", "compute", catalog.AggregationSum, "seconds", nil, capped, nil)
	p.meter("requests", "requests", catalog.AggregationCount, "", nil, perUnit(2_000, 0), &catalog.Allowance{Included: 2})
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 100_000_000)

	charges := func(eventType string, dimensions []map[string]int64) []int64 {
		t.Helper()
		var out []int64
		for _, d := range dimensions {
			out = append(out, p.record(usage(customer, eventType, d, nil)).Amount)
		}
		return out
	}
	require.Equal(t, []int64{1_000, 1_000, 500, 500}, charges("calls", make([]map[string]int64, 4)))
	bytes := func(n int64) map[string]int64 { return map[string]int64{"bytes": n} }
	// 400 and 800 bytes: 0 and 300 past the free 500, one package; 1,100 is
	// still one; 1,600 is two.
	require.Equal(t, []int64{0, 7_000, 0, 7_000}, charges("storage", []map[string]int64{bytes(400), bytes(400), bytes(300), bytes(500)}))
	seconds := map[string]int64{"seconds": 1_000}
	require.Equal(t, []int64{10_000, 10_000, 5_000, 0}, charges("compute", []map[string]int64{seconds, seconds, seconds, seconds}))
	require.Equal(t, []int64{0, 0, 2_000}, charges("requests", make([]map[string]int64, 3)))
	require.EqualValues(t, 100_000_000-3_000-14_000-25_000-2_000, p.balance(customer).BalanceAmount)

	// The previous period has no quantity yet: its first call is a tier-one call.
	start, err := money.CurrentBillingPeriodStart(time.Now(), time.Time{}, money.BillingBoundaryFixedInterval)
	require.NoError(t, err)
	earlier := usage(customer, "calls", nil, nil)
	at := start.Add(-time.Minute)
	earlier.OccurredAt = &at
	require.EqualValues(t, 1_000, p.record(earlier).Amount)
	require.EqualValues(t, 500, p.record(usage(customer, "calls", nil, nil)).Amount, "the current period's quantity is its own")
}

// Usage is never refused for money: past the prepaid balance the rest is
// owed, and the owed refuses the next admission instead.
func TestPrepaidUsagePastTheBalanceIsOwed(t *testing.T) {
	p := newPricing(t, newFixture(t), "past")
	p.meter("gpu-seconds", "gpu", catalog.AggregationSum, "seconds", nil, perUnit(1_000, 0), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1_000_000)

	results, err := p.client.RecordUsage(p.ctx, []billing.RecordUsageParams{usage(customer, "gpu", map[string]int64{"seconds": 1_500}, nil)})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, results[0].Status, "%+v", results[0].Error)
	require.EqualValues(t, 1_500_000, results[0].Event.Amount)
	balance := p.balance(customer)
	require.EqualValues(t, 0, balance.BalanceAmount)
	require.EqualValues(t, 500_000, balance.OwedAmount)
	require.Equal(t, billing.BillingModePrepaid, balance.BillingMode)

	p.meter("inference", "inference", catalog.AggregationSum, "units", nil, dynamicPrice(), nil)
	hostPriced := usage(customer, "inference", map[string]int64{"units": 1}, nil)
	hostPriced.Amount = new(int64(100_000))
	require.EqualValues(t, 100_000, p.record(hostPriced).Amount, "a host-priced event past the balance is owed too")
	require.EqualValues(t, 600_000, p.balance(customer).OwedAmount)

	deadline := time.Now().Add(time.Hour)
	verdicts, err := p.client.Admit(p.ctx, []billing.AdmitParams{{RequestID: uuid.NewString(), CustomerID: customer, Invoker: customer.String(),
		InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: 1, ExpiresAt: &deadline}})
	require.NoError(t, err)
	require.False(t, verdicts[0].Allowed())
	require.NotNil(t, verdicts[0].Admission, "%+v", verdicts[0])
	require.Equal(t, billing.AdmissionBlockedByMoney, *verdicts[0].Admission.BlockedBy)
}

// A rate change applies from the next event: what was charged stays, and the
// next event pays the new card's price for what it adds.
func TestPriceChangeAppliesFromTheNextEvent(t *testing.T) {
	p := newPricing(t, newFixture(t), "change")
	p.meter("tokens", "llm.request", catalog.AggregationSum, "tokens", nil, perUnit(150_000, 1_000_000), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 10_000_000)

	first := p.record(usage(customer, "llm.request", map[string]int64{"tokens": 1_000_000}, nil))
	require.EqualValues(t, 150_000, first.Amount)
	p.meter("tokens", "llm.request", catalog.AggregationSum, "tokens", nil, perUnit(300_000, 1_000_000), nil)
	second := p.record(usage(customer, "llm.request", map[string]int64{"tokens": 1_000_000}, nil))
	require.EqualValues(t, 300_000, second.Amount, "R'(2M) - R'(1M) on the new card")
	require.EqualValues(t, 10_000_000-450_000, p.balance(customer).BalanceAmount)
}

// A replay answers the first charges, even after the price changed, and
// moves no money.
func TestUsageReplayAnswersTheFirstCharge(t *testing.T) {
	p := newPricing(t, newFixture(t), "replay")
	p.meter("tokens", "llm.request", catalog.AggregationSum, "tokens", nil, perUnit(150_000, 1_000_000), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 10_000_000)

	params := usage(customer, "llm.request", map[string]int64{"tokens": 2_000_000}, nil)
	first := p.record(params)
	require.EqualValues(t, 300_000, first.Amount)
	p.meter("tokens", "llm.request", catalog.AggregationSum, "tokens", nil, perUnit(900_000, 1_000_000), nil)
	again := p.record(params)
	require.True(t, again.Replayed)
	require.Equal(t, first.ID, again.ID)
	require.Equal(t, first.Charges, again.Charges)
	require.Equal(t, first.Amount, again.Amount)
	require.EqualValues(t, 10_000_000-300_000, p.balance(customer).BalanceAmount)
}

// A matrix with no cell for the event's value refuses the event: a catalog
// gap is never free, and nothing is recorded or charged.
func TestMissingMatrixCellIsUsageNotPriced(t *testing.T) {
	p := newPricing(t, newFixture(t), "cell")
	price := catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{DivideBy: 1_000_000,
		Matrix: &catalog.Matrix{Dimension: "model", Cells: map[string]catalog.MatrixCell{"small": {UnitAmount: 100_000}}}}}
	p.meter("tokens", "llm.request", catalog.AggregationSum, "tokens", map[string]string{"model": "metadata.model"}, price, nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1_000_000)

	params := usage(customer, "llm.request", map[string]int64{"tokens": 1_000_000}, map[string]any{"model": "huge"})
	results, err := p.client.RecordUsage(p.ctx, []billing.RecordUsageParams{params, usage(customer, "llm.request", map[string]int64{"tokens": 1_000_000}, map[string]any{"model": "small"})})
	require.NoError(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, results[0].Status)
	require.ErrorIs(t, results[0].Err(), billing.ErrUsageNotPriced)
	require.Equal(t, billing.CodeUsageNotPriced, results[0].Error.Code)
	require.Equal(t, "metadata.model", *results[0].Error.Param)
	require.Equal(t, http.StatusCreated, results[1].Status, "a refused item refuses only itself")
	require.EqualValues(t, 100_000, results[1].Event.Amount)
	require.Equal(t, "small", *results[1].Event.Charges[0].Cell)
	require.EqualValues(t, 900_000, p.balance(customer).BalanceAmount)

	// The refused event recorded nothing: once the cell exists it records.
	price.PerUnit.Matrix.Cells["huge"] = catalog.MatrixCell{UnitAmount: 200_000}
	p.meter("tokens", "llm.request", catalog.AggregationSum, "tokens", map[string]string{"model": "metadata.model"}, price, nil)
	event := p.record(params)
	require.False(t, event.Replayed)
	require.EqualValues(t, 200_000, event.Amount)
}

// The LLM recipe: one event per request with disjoint token dimensions and
// the model in metadata; a meter per token type, each priced per million by
// a model matrix. Cached input is its own meter, never a share of input.
func TestLLMTokensPricePerMillionByModel(t *testing.T) {
	p := newPricing(t, newFixture(t), "llm")
	byModel := func(small, large int64) catalog.RatePrice {
		return catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{DivideBy: 1_000_000,
			Matrix: &catalog.Matrix{Dimension: "model", Cells: map[string]catalog.MatrixCell{"small": {UnitAmount: small}, "large": {UnitAmount: large}}}}}
	}
	model := map[string]string{"model": "metadata.model"}
	p.meter("input-tokens", "chat", catalog.AggregationSum, "input_tokens", model, byModel(150_000, 2_500_000), nil)
	p.meter("cached-input-tokens", "chat", catalog.AggregationSum, "cached_input_tokens", model, byModel(75_000, 1_250_000), nil)
	p.meter("output-tokens", "chat", catalog.AggregationSum, "output_tokens", model, byModel(600_000, 10_000_000), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 100_000_000)

	request := func(name string, input, cached, output int64) *billing.UsageEvent {
		return p.record(usage(customer, "chat", map[string]int64{"input_tokens": input, "cached_input_tokens": cached, "output_tokens": output}, map[string]any{"model": name}))
	}
	// small: 12,000 x 0.15 + 4,000 x 0.075 + 1,500 x 0.6 = 1,800 + 300 + 900.
	small := request("small", 12_000, 4_000, 1_500)
	require.Equal(t, map[string]int64{"input-tokens": 1_800, "cached-input-tokens": 300, "output-tokens": 900}, charged(small))
	for _, c := range small.Charges {
		require.Equal(t, "small", *c.Cell)
	}
	// large: 12,000 x 2.5 + 4,000 x 1.25 + 1,500 x 10 = 30,000 + 5,000 + 15,000.
	large := request("large", 12_000, 4_000, 1_500)
	require.Equal(t, map[string]int64{"input-tokens": 30_000, "cached-input-tokens": 5_000, "output-tokens": 15_000}, charged(large))
	// Each model's quantity is its own: a second small request is priced from
	// small's 12,000, not from both models' 24,000.
	again := request("small", 1_000_000, 0, 0)
	require.Equal(t, map[string]int64{"input-tokens": 150_000}, charged(again), "a zero dimension takes nothing")
	require.EqualValues(t, 100_000_000-3_000-50_000-150_000, p.balance(customer).BalanceAmount)

	// A request's hold is its prompt and max tokens at its model's price, and
	// its capture what it used, priced by the same cards.
	held := p.admitUsage(customer, "chat-large", billing.UsageEstimate{EventType: "chat",
		Dimensions: map[string]int64{"input_tokens": 2_000, "output_tokens": 4_000}, Metadata: map[string]any{"model": "large"}})
	require.EqualValues(t, 5_000+40_000, held.Admission.EstimatedAmount)
	receipt := p.capture(billing.CaptureAdmissionParams{RequestID: "chat-large", Usage: billing.CaptureUsage{EventType: "chat",
		Dimensions: map[string]int64{"input_tokens": 2_000, "cached_input_tokens": 1_000, "output_tokens": 300}, Metadata: map[string]any{"model": "large"}}})[0]
	require.Equal(t, http.StatusOK, receipt.Status, "%+v", receipt.Error)
	require.Equal(t, map[string]int64{"input-tokens": 5_000, "cached-input-tokens": 1_250, "output-tokens": 3_000}, receiptCharges(receipt.Receipt))
	require.EqualValues(t, 0, p.balance(customer).HeldAmount)
}

// Whatever the order and sizes of a period's events, each meter's charges sum
// to its card's price for the period's total quantity.
func TestPeriodChargesEqualThePeriodPrice(t *testing.T) {
	p := newPricing(t, newFixture(t), "period")
	thousand := int64(1_000)
	tiered := catalog.RatePrice{Model: catalog.ModelTiered, Tiered: &catalog.TieredPrice{Mode: catalog.TierModeGraduated,
		Tiers: []catalog.RateTier{{UpTo: &thousand, UnitAmount: 7, FlatAmount: 1_000}, {UnitAmount: 3}}}}
	p.meter("tiered", "work", catalog.AggregationSum, "tiered", nil, tiered, &catalog.Allowance{Included: 150})
	divided := perUnit(1_000_001, 999)
	divided.PerUnit.Round = catalog.RoundUp
	divided.PerUnit.MaximumAmount = 9_000_000
	p.meter("divided", "work", catalog.AggregationSum, "divided", nil, divided, &catalog.Allowance{Included: 2})
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1_000_000_000)

	var totals = map[string]int64{}
	var charges = map[string]int64{}
	for i := range 120 {
		q := int64(i*37%101 + 1)
		dims := map[string]int64{"tiered": q, "divided": q * 13}
		event := p.record(usage(customer, "work", dims, nil))
		for _, c := range event.Charges {
			charges[c.MeterKey] += c.Amount
			totals[c.MeterKey] += c.Quantity
		}
	}
	// R(Q) computed here from the cards' definitions.
	tieredQ := max(totals["tiered"]-150, 0)
	wantTiered := 1_000 + min(tieredQ, 1_000)*7 + max(tieredQ-1_000, 0)*3
	dividedQ := max(totals["divided"]-2*999, 0)
	wantDivided := min((dividedQ*1_000_001+998)/999, 9_000_000)
	require.Equal(t, map[string]int64{"tiered": wantTiered, "divided": wantDivided}, charges, "quantities %v", totals)
	require.EqualValues(t, 1_000_000_000-wantTiered-wantDivided, p.balance(customer).BalanceAmount)
}

// The usage report groups what each meter, and matrix cell, charged: its
// events, the quantity it priced and the amount, largest first, paged.
func TestUsageReportGroupsByMeter(t *testing.T) {
	f := newFixture(t)
	bill, client := f.runtimeWithStripe(t, "report-"+uuid.NewString()[:8], nil)
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "usage", DisplayName: "Usage"})
	require.NoError(t, err)
	p := &pricing{t: t, ctx: t.Context(), engine: bill, client: client, product: product.ID}
	byModel := catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{DivideBy: 1_000_000,
		Matrix: &catalog.Matrix{Dimension: "model", Cells: map[string]catalog.MatrixCell{"small": {UnitAmount: 1_000_000}, "large": {UnitAmount: 10_000_000}}}}}
	p.meter("input-tokens", "chat", catalog.AggregationSum, "input_tokens", map[string]string{"model": "metadata.model"}, byModel, nil)
	p.meter("output-tokens", "chat", catalog.AggregationSum, "output_tokens", nil, perUnit(2, 0), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1_000_000)
	for _, e := range []struct {
		model         string
		input, output int64
	}{{"small", 1_000, 500}, {"large", 2_000, 100}, {"small", 3_000, 0}} {
		p.record(usage(customer, "chat", map[string]int64{"input_tokens": e.input, "output_tokens": e.output}, map[string]any{"model": e.model}))
	}

	type row struct {
		meter, cell    string
		events, amount int64
		quantity       int64
	}
	read := func(r billing.UsageRow) row {
		out := row{meter: r.Key, events: r.EventCount, amount: r.Amount}
		if r.Cell != nil {
			out.cell = *r.Cell
		}
		if r.Quantity != nil {
			out.quantity = *r.Quantity
		}
		return out
	}
	want := []row{
		{"input-tokens", "large", 1, 20_000, 2_000},
		{"input-tokens", "small", 2, 4_000, 4_000},
		{"output-tokens", "", 2, 1_200, 600},
	}
	reader := readUsageAs(t, bill, customer)
	rows, pages := reader.all(url.Values{"group_by": {"meter"}}, 1)
	require.Equal(t, 3, pages)
	got := make([]row, len(rows))
	for i, r := range rows {
		got[i] = read(r)
	}
	require.Equal(t, want, got)

	byType := reader.report(url.Values{"group_by": {"event_type"}})
	require.Len(t, byType.Rows, 1)
	require.EqualValues(t, 25_200, byType.Rows[0].Amount, "an event's amount is its charges")
	require.Nil(t, byType.Rows[0].Quantity)
}

// A metered card's charges are posted when usage is recorded, so it states
// no payment term; only a flat fee says when it is collected.
func TestMeteredCardRefusesPaymentTerm(t *testing.T) {
	client := newFixture(t).runtime(t, "term-"+uuid.NewString()[:8])
	document := func(term string) *catalog.Application {
		t.Helper()
		params, err := catalog.ParseApplicationYAML([]byte(`schema_version: 1
meters:
  calls:
    aggregation: count
products:
  api:
    display_name: API
    rate_cards:
    - meter: calls
` + term + `      price: {model: per_unit, currency: USD, per_unit: {unit_amount: "1000"}}
    - payment_term: in_advance
      price: {model: flat, currency: USD, flat: {amount: "5000000"}}
`))
		require.NoError(t, err)
		return params
	}
	_, err := client.ApplyCatalog(t.Context(), document("      payment_term: in_arrears\n"), billing.ApplyCatalogParams{})
	require.ErrorIs(t, err, billing.ErrInvalid)
	require.ErrorContains(t, err, "states no payment_term")
	_, err = client.ApplyCatalog(t.Context(), document(""), billing.ApplyCatalogParams{})
	require.NoError(t, err, "a flat fee keeps its payment term")
	meter, err := client.GetMeter(t.Context(), "calls")
	require.NoError(t, err)
	require.NotNil(t, meter.RateCard)
}

// Volume tiers would lower the price of earlier units when a later event
// reaches a cheaper band: a metered card takes graduated tiers only.
func TestMeteredCardRefusesVolumeTiers(t *testing.T) {
	p := newPricing(t, newFixture(t), "volume")
	ten := int64(10)
	tiers := func(mode catalog.TierMode) catalog.RatePrice {
		return catalog.RatePrice{Model: catalog.ModelTiered, Currency: "USD", Tiered: &catalog.TieredPrice{
			Mode: mode, Tiers: []catalog.RateTier{{UpTo: &ten, UnitAmount: 1_000}, {UnitAmount: 500}}}}
	}
	set := func(mode catalog.TierMode) error {
		_, err := p.client.SetMeter(p.ctx, "calls", billing.SetMeterParams{Aggregation: catalog.AggregationCount,
			RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: p.product, Price: tiers(mode)})})
		return err
	}
	err := set(catalog.TierModeVolume)
	require.ErrorIs(t, err, billing.ErrInvalid)
	var refusal *billing.StatusError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, "usage_rate_card_invalid", refusal.Code)
	require.ErrorContains(t, err, "volume tiers")
	require.NoError(t, set(catalog.TierModeGraduated))

	params, err := catalog.ParseApplicationYAML([]byte(`schema_version: 1
meters:
  seats:
    aggregation: count
products:
  seats:
    display_name: Seats
    rate_cards:
    - meter: seats
      price:
        model: tiered
        currency: USD
        tiered: {mode: volume, tiers: [{up_to: 10, unit_amount: "1000"}, {unit_amount: "500"}]}
`))
	require.NoError(t, err)
	_, err = p.client.ApplyCatalog(p.ctx, params, billing.ApplyCatalogParams{})
	require.ErrorIs(t, err, billing.ErrInvalid)
	require.ErrorContains(t, err, "volume tiers")
}
