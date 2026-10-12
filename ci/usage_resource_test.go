//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
)

// resourceSpend is the customer's USD usage report grouped by resource, as
// the customer reads it: each resource's total amount.
func resourceSpend(t *testing.T, bill *openrails.Engine, customer billing.CustomerID) map[string]int64 {
	t.Helper()
	auth := &authtest.Fake{}
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, bill, openrails.Routes{Auth: auth}))
	req := httptest.NewRequest(http.MethodGet, "/v1/me/usage?currency=USD&group_by=resource", nil)
	req.Header.Set("Authorization", "Bearer "+auth.Person(customer.String()))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var report billing.Usage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &report))
	out := map[string]int64{}
	for _, row := range report.Rows {
		out[row.Key] = row.Amount
	}
	return out
}

// A recorded usage event keeps its resource in its resource column, which
// the usage report and the metrics read; the caller's metadata stays its own.
func TestRecordedUsageKeepsItsResource(t *testing.T) {
	f := newFixture(t)
	bill, client := f.runtimeWithStripe(t, "resource-"+uuid.NewString()[:8], nil)
	ctx := t.Context()
	customer := billing.CustomerID(uuid.New())
	_, err := createCreditGrant(ctx, client, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: 1_000_000, Source: "support", SourceID: "seed"})
	require.NoError(t, err)
	usage := func(sourceID, resource string, amount int64, metadata map[string]any) billing.RecordUsageParams {
		return billing.RecordUsageParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "gpu", Amount: amount,
			Resource: resource, Metadata: metadata, Source: "worker", SourceID: sourceID}
	}
	results, err := client.RecordUsage(ctx, []billing.RecordUsageParams{
		usage("u-1", "gpu:a100", 300, nil),
		usage("u-2", "gpu:a100", 200, nil),
		usage("u-3", "  cpu  ", 100, nil),
		usage("u-4", "", 50, nil),
		usage("u-5", "gpu:h100", 400, map[string]any{"resource": "the caller's own"}),
	})
	require.NoError(t, err)
	for _, r := range results {
		require.Equal(t, http.StatusCreated, r.Status, "%+v", r.Error)
	}
	resource := func(i int) *string { return results[i].Event.Resource }
	require.Equal(t, "gpu:a100", *resource(0))
	require.NotContains(t, results[0].Event.Metadata, "resource", "the resource is not copied into metadata")
	require.Equal(t, "cpu", *resource(2), "trimmed")
	require.Nil(t, resource(3), "no resource is NULL")
	require.Equal(t, "gpu:h100", *resource(4))
	require.Equal(t, "the caller's own", results[4].Event.Metadata["resource"])

	replay, err := recordUsage(ctx, client, usage("u-1", "gpu:a100", 300, nil))
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Equal(t, "gpu:a100", *replay.Resource, "a replay answers the stored resource")

	require.Equal(t, map[string]int64{"gpu:h100": 400, "gpu:a100": 500, "cpu": 100, "": 50}, resourceSpend(t, bill, customer))

	from, to := results[0].Event.OccurredAt.Add(-time.Hour).Format(time.RFC3339), results[0].Event.OccurredAt.Add(time.Hour).Format(time.RFC3339)
	metrics, err := client.QueryMetrics(ctx, billing.MetricsQuery{Measures: []string{"usage_revenue"}, By: []string{"sku"},
		Range: &billing.MetricsRange{From: from, To: to}, Filters: map[string][]string{"customer": {customer.String()}}})
	require.NoError(t, err)
	bySKU := map[string]string{}
	for _, row := range metricsRows(metrics) {
		bySKU[row["sku"]] = row["usage_revenue"]
	}
	require.Equal(t, map[string]string{"gpu:h100": "400", "gpu:a100": "500", "cpu": "100", "": "50"}, bySKU)
}

// Resource used to be copied into metadata before rating. Recording it in its
// own column must keep resource-based rate cards working without changing the
// caller's metadata, including an explicit metadata override.
func TestRecordedUsageResourceKeepsItsRating(t *testing.T) {
	p := newPricing(t, newFixture(t), "resource-rating")
	price := perUnit(100, 0)
	price.Currency = "USD"
	_, err := p.client.SetMeter(p.ctx, "gpu-requests", billing.SetMeterParams{
		EventType: "gpu.request", Aggregation: catalog.AggregationCount,
		GroupBy: map[string]string{"resource": "metadata.resource"},
		RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: p.product,
			Filter: map[string][]string{"resource": {"gpu:a100"}}, Price: price}),
	})
	require.NoError(t, err)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 10_000)

	metadata := map[string]any{"note": "caller's data"}
	params := usage(customer, "gpu.request", nil, metadata)
	params.Resource = "  gpu:a100  "
	first := p.record(params)
	require.EqualValues(t, 100, first.Amount, "resource-based cards still price the event")
	require.Equal(t, "gpu:a100", *first.Resource)
	require.Equal(t, map[string]int64{"gpu-requests": 100}, charged(first))
	require.Equal(t, map[string]any{"note": "caller's data"}, metadata, "input metadata is untouched")
	require.Equal(t, metadata, first.Metadata, "stored metadata contains no synthesized resource")

	params = usage(customer, "gpu.request", nil, map[string]any{"resource": "gpu:a100"})
	params.Resource = "gpu:h100"
	second := p.record(params)
	require.EqualValues(t, 100, second.Amount, "explicit metadata retains its original pricing precedence")
	require.Equal(t, "gpu:h100", *second.Resource)
	require.Equal(t, "gpu:a100", second.Metadata["resource"])

	params = usage(customer, "gpu.request", nil, map[string]any{"resource": nil})
	params.Resource = "gpu:a100"
	third := p.record(params)
	require.Zero(t, third.Amount, "an explicitly empty metadata value is not replaced")
	require.Contains(t, third.Metadata, "resource")
	require.Nil(t, third.Metadata["resource"])
	params = usage(customer, "gpu.request", nil, nil)
	params.Resource = "gpu:a100"
	fourth := p.record(params)
	require.EqualValues(t, 100, fourth.Amount, "nil metadata also receives a rating-only resource")
	require.Empty(t, fourth.Metadata, "rating does not create stored metadata")

	p.meter("numeric-resource", "resource.quantity", catalog.AggregationSum, "metadata.resource", nil, perUnit(100, 0), nil)
	params = usage(customer, "resource.quantity", nil, nil)
	params.Resource = "2"
	fifth := p.record(params)
	require.EqualValues(t, 200, fifth.Amount, "sum meters retain the same resource value as filters")
	require.Empty(t, fifth.Metadata)
	params = usage(customer, "resource.quantity", nil, map[string]any{"resource": int64(3)})
	params.Resource = "2"
	sixth := p.record(params)
	require.EqualValues(t, 300, sixth.Amount, "explicit numeric metadata retains precedence")
	encodedMetadata, err := json.Marshal(sixth.Metadata)
	require.NoError(t, err)
	require.JSONEq(t, `{"resource":3}`, string(encodedMetadata))
	require.Equal(t, int64(3), params.Metadata["resource"], "input numeric metadata is untouched")
	require.EqualValues(t, 9_200, p.balance(customer).BalanceAmount)
}
