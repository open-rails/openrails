//go:build e2e && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/money"
)

// meteredWorld is a world whose clock starts an hour into a billing period,
// with no scheduled passes: each test runs its own.
type meteredWorld struct {
	*world
	t       *testing.T
	client  *openrails.Client
	product billing.ProductID
	start   time.Time // the period the clock starts in
}

const billingInterval = 30 * 24 * time.Hour

func newMeteredWorld(t *testing.T) *meteredWorld {
	w := newWorld(t)
	w.settleRefreshes()
	w.jobs.PeriodicJobs().Clear()
	current, err := money.CurrentBillingPeriodStart(w.clock.Now(), time.Time{}, money.BillingBoundaryFixedInterval)
	require.NoError(t, err)
	start := current.Add(billingInterval)
	w.advance(start.Add(time.Hour).Sub(w.clock.Now()))
	client := w.client[remote]
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "metered-" + uuid.NewString()[:8], DisplayName: "Metered"})
	require.NoError(t, err)
	return &meteredWorld{world: w, t: t, client: client, product: product.ID, start: start}
}

func (m *meteredWorld) meter(key string, aggregation catalog.Aggregation, valueProperty string, price catalog.RatePrice) {
	m.t.Helper()
	price.Currency = "USD"
	_, err := m.client.SetMeter(m.t.Context(), key, billing.SetMeterParams{Aggregation: aggregation, ValueProperty: valueProperty,
		RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: m.product, Price: price})})
	require.NoError(m.t, err)
}

// arrears is a customer invoiced for what it owes, with a credit line.
func (m *meteredWorld) arrears() billing.CustomerID {
	m.t.Helper()
	c := m.newCustomer().cid()
	_, err := m.client.UpdateCustomer(m.t.Context(), c, billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 200_000_000}}})
	require.NoError(m.t, err)
	return c
}

func (m *meteredWorld) use(c billing.CustomerID, eventType string, dimensions map[string]int64, occurredAt *time.Time) *billing.UsageEvent {
	m.t.Helper()
	event, err := recordUsage(m.t.Context(), m.client, billing.RecordUsageParams{CustomerID: c, Invoker: c.String(), Currency: "USD",
		EventType: eventType, Dimensions: dimensions, Source: "test", SourceID: uuid.NewString(), OccurredAt: occurredAt})
	require.NoError(m.t, err)
	return event
}

func (m *meteredWorld) pass(args river.JobArgs) {
	m.t.Helper()
	m.advance(time.Minute)
	job, err := m.jobs.Insert(m.t.Context(), args, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(m.t, err)
	m.waitJob(job.Job.ID)
}

// orders are the customer's usage orders, oldest first.
func (m *meteredWorld) orders(c billing.CustomerID) []*billing.Order {
	m.t.Helper()
	page, err := m.client.ListOrders(m.t.Context(), billing.OrderListParams{Origin: billing.OrderOriginUsage, CustomerID: c})
	require.NoError(m.t, err)
	var out []*billing.Order
	for i := len(page.Items) - 1; i >= 0; i-- {
		o, err := m.client.GetOrder(m.t.Context(), page.Items[i].ID)
		require.NoError(m.t, err)
		out = append(out, o)
	}
	return out
}

// lines is an order's lines as source -> amount.
func lines(o *billing.Order) map[string]int64 {
	out := map[string]int64{}
	for _, l := range o.Lines {
		if l.Source != nil {
			out[*l.Source] += l.Amount
		}
	}
	return out
}

// Rated usage is owed the moment it is recorded, so an arrears customer's
// threshold pass sees it at once and bills it on a line per meter.
func TestArrearsUsageReachesTheThresholdAtOnce(t *testing.T) {
	t.Parallel()
	m := newMeteredWorld(t)
	m.meter("gpu-seconds", catalog.AggregationSum, "seconds", catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{UnitAmount: 10_000}})
	c := m.arrears()

	event := m.use(c, "gpu-seconds", map[string]int64{"seconds": 5_000}, nil)
	require.EqualValues(t, money.DefaultCollectionThresholdAmount, event.Amount)
	balance, err := m.client.GetBalance(t.Context(), c, "USD")
	require.NoError(t, err)
	require.EqualValues(t, money.DefaultCollectionThresholdAmount, balance.OwedAmount)

	m.pass(invoicePass{})
	billed := m.orders(c)
	require.Len(t, billed, 1, "the threshold pass bills the period so far")
	require.EqualValues(t, money.DefaultCollectionThresholdAmount, billed[0].Total)
	require.Equal(t, map[string]int64{"metered:gpu-seconds": money.DefaultCollectionThresholdAmount}, lines(billed[0]))
}

// The period close bills what the period's usage was charged, a line per
// meter, once.
func TestPeriodCloseBillsPricedUsageOnce(t *testing.T) {
	t.Parallel()
	m := newMeteredWorld(t)
	m.meter("calls", catalog.AggregationCount, "", catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{UnitAmount: 1_000}})
	m.meter("bytes", catalog.AggregationSum, "bytes", catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{UnitAmount: 3, DivideBy: 10}})
	c := m.arrears()
	for range 3 {
		m.use(c, "calls", nil, nil)
	}
	m.use(c, "bytes", map[string]int64{"bytes": 1_005}, nil) // round(301.5) = 302
	m.use(c, "bytes", map[string]int64{"bytes": 5}, nil)     // R(1,010) - R(1,005) = 1

	m.advance(m.start.Add(billingInterval + time.Hour).Sub(m.clock.Now()))
	m.pass(openrails.UsageSweepArgs{FinalizePreviousMonth: true})
	m.pass(openrails.UsageSweepArgs{FinalizePreviousMonth: true})
	billed := m.orders(c)
	require.Len(t, billed, 1, "a second close bills nothing more")
	require.True(t, billed[0].Usage.PeriodStartsAt.Equal(m.start))
	require.EqualValues(t, 3_303, billed[0].Total)
	require.Equal(t, map[string]int64{"metered:calls": 3_000, "metered:bytes": 303}, lines(billed[0]))
}

// An event dated in a period whose order is already billed is priced at that
// period's quantity and billed by the open period's order, never dropped and
// never stranded in the closed one.
func TestUsageAfterItsPeriodWasBilledIsBilledInTheOpenPeriod(t *testing.T) {
	t.Parallel()
	m := newMeteredWorld(t)
	one := int64(1)
	m.meter("calls", catalog.AggregationCount, "", catalog.RatePrice{Model: catalog.ModelTiered, Tiered: &catalog.TieredPrice{
		Mode: catalog.TierModeGraduated, Tiers: []catalog.RateTier{{UpTo: &one, UnitAmount: 1_000}, {UnitAmount: 500}}}})
	c := m.arrears()
	require.EqualValues(t, 1_000, m.use(c, "calls", nil, nil).Amount)

	next := m.start.Add(billingInterval)
	m.advance(next.Add(time.Hour).Sub(m.clock.Now()))
	m.pass(openrails.UsageSweepArgs{FinalizePreviousMonth: true})
	first := m.orders(c)
	require.Len(t, first, 1)
	require.EqualValues(t, 1_000, first[0].Total)

	late := next.Add(-time.Minute)
	require.EqualValues(t, 500, m.use(c, "calls", nil, &late).Amount, "the closed period's second call")
	require.EqualValues(t, 1_000, m.use(c, "calls", nil, nil).Amount, "the open period's first call")

	m.advance(next.Add(billingInterval + time.Hour).Sub(m.clock.Now()))
	m.pass(openrails.UsageSweepArgs{FinalizePreviousMonth: true})
	billed := m.orders(c)
	require.Len(t, billed, 2)
	require.EqualValues(t, 1_000, billed[0].Total, "the closed period's order is unchanged")
	require.True(t, billed[1].Usage.PeriodStartsAt.Equal(next))
	require.EqualValues(t, 1_500, billed[1].Total)
}

// A hosted product's shape: payment volume, reported in micros, priced at
// 30 basis points for arrears customers and billed at the period close.
func TestPaymentVolumeAtBasisPointsBillsInArrears(t *testing.T) {
	t.Parallel()
	m := newMeteredWorld(t)
	m.meter("payment-volume", catalog.AggregationSum, "currency_micros", catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{UnitAmount: 30, DivideBy: 10_000}})
	c := m.arrears()
	// 13,345,778 micros of volume at 0.3% is 40,037.334: 40,037.
	var charged []int64
	for _, volume := range []int64{12_345_678, 1_000_001, 99} {
		charged = append(charged, m.use(c, "payment-volume", map[string]int64{"currency_micros": volume}, nil).Amount)
	}
	require.Equal(t, []int64{37_037, 3_000, 0}, charged)
	balance, err := m.client.GetBalance(t.Context(), c, "USD")
	require.NoError(t, err)
	require.EqualValues(t, 40_037, balance.OwedAmount)

	m.advance(m.start.Add(billingInterval + time.Hour).Sub(m.clock.Now()))
	m.pass(openrails.UsageSweepArgs{FinalizePreviousMonth: true})
	billed := m.orders(c)
	require.Len(t, billed, 1)
	require.EqualValues(t, 40_037, billed[0].Total)
	require.Equal(t, map[string]int64{"metered:payment-volume": 40_037}, lines(billed[0]))
}
