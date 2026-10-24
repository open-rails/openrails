//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func usageMeter(t *testing.T, client *openrails.Client, amount int64) (*billing.Product, billing.SetMeterParams) {
	t.Helper()
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "usage-compute", DisplayName: "Usage compute"})
	require.NoError(t, err)
	params := billing.SetMeterParams{EventType: "compute", ValueProperty: "units", Aggregation: catalog.AggregationSum, RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: product.ID, Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "USD", PerUnit: &catalog.PerUnitPrice{UnitAmount: amount, DivideBy: 1}}})}
	_, err = client.SetMeter(t.Context(), "compute", params)
	require.NoError(t, err)
	return product, params
}
func usageInput(customer billing.CustomerID, id string, units int64) billing.RecordUsageParams {
	return billing.RecordUsageParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "compute", Dimensions: map[string]int64{"units": units}, Source: "critical", SourceID: id, Metadata: map[string]any{"source": "host work receipt"}}
}
func oneUsage(t *testing.T, client *openrails.Client, input billing.RecordUsageParams) *billing.UsageEvent {
	t.Helper()
	rows, err := client.RecordUsage(t.Context(), []billing.RecordUsageParams{input})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NoError(t, rows[0].Err())
	require.Equal(t, http.StatusCreated, rows[0].Status)
	require.NotNil(t, rows[0].Event)
	return rows[0].Event
}
func TestUsageContracts(t *testing.T) {
	t.Run("OR-J24-S01/replay-retains-one-immutable-rated-event", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		input := usageInput(customer, "stable-event", 2)
		input.Amount = ptr(int64(200))
		first := oneUsage(t, w.alpha, input)
		require.EqualValues(t, 200, first.Amount)
		require.Len(t, first.Charges, 1)
		require.EqualValues(t, 2, first.Charges[0].Quantity)
		input.Dimensions = map[string]int64{"units": 99}
		input.Metadata = map[string]any{"source": "different replay metadata"}
		replay, err := w.alpha.RecordUsage(t.Context(), []billing.RecordUsageParams{input})
		require.NoError(t, err)
		require.NoError(t, replay[0].Err())
		require.Equal(t, http.StatusOK, replay[0].Status)
		require.True(t, replay[0].Event.Replayed)
		require.Equal(t, first.ID, replay[0].Event.ID)
		require.Equal(t, first.Dimensions, replay[0].Event.Dimensions)
		require.Equal(t, first.Metadata, replay[0].Event.Metadata)
		require.Equal(t, first.Charges, replay[0].Event.Charges)
		for _, changed := range []billing.RecordUsageParams{func() billing.RecordUsageParams { v := input; v.Amount = ptr(int64(201)); return v }(), func() billing.RecordUsageParams { v := input; v.Outcome = billing.UsageFailed; return v }()} {
			refused, err := w.alpha.RecordUsage(t.Context(), []billing.RecordUsageParams{changed})
			require.NoError(t, err)
			require.ErrorIs(t, refused[0].Err(), billing.ErrIdempotencyKeyReused)
		}
		read, err := w.alpha.GetUsageEvent(t.Context(), first.ID)
		require.NoError(t, err)
		require.False(t, read.Replayed)
		require.Equal(t, first.Charges, read.Charges)
		events, err := w.alpha.ListUsageEvents(t.Context(), billing.UsageEventListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, events.Items, 1)
		balance(t, w.alpha, customer, 800, 0, 800)
		ledger, err := w.alpha.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		require.Len(t, ledger.Items, 2)
	})
	t.Run("OR-J24-S02/late-event-uses-ingest-price-and-prior-event-stays-pinned", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		_, params := usageMeter(t, w.alpha, 100)
		original := usageInput(customer, "before-price-edit", 2)
		first := oneUsage(t, w.alpha, original)
		require.EqualValues(t, 200, first.Amount)
		params.RateCard.Value.Price.PerUnit.UnitAmount = 300
		_, err := w.alpha.SetMeter(t.Context(), "compute", params)
		require.NoError(t, err)
		late := usageInput(customer, "late-after-price-edit", 2)
		late.OccurredAt = ptr(w.clock.Now().Add(-time.Hour))
		recorded := oneUsage(t, w.alpha, late)
		require.EqualValues(t, 600, recorded.Amount, "new events are rated at ingestion, including late delivery")
		require.WithinDuration(t, *late.OccurredAt, recorded.OccurredAt, time.Microsecond)
		replay, err := w.alpha.RecordUsage(t.Context(), []billing.RecordUsageParams{original})
		require.NoError(t, err)
		require.NoError(t, replay[0].Err())
		require.EqualValues(t, 200, replay[0].Event.Amount)
		require.Equal(t, first.ID, replay[0].Event.ID)
		changed := params
		changed.ValueProperty = "different-units"
		_, err = w.alpha.SetMeter(t.Context(), "compute", changed)
		require.ErrorIs(t, err, billing.ErrConflict)
		for i, at := range []time.Time{w.clock.Now().Add(-36 * 24 * time.Hour), w.clock.Now().Add(time.Hour)} {
			invalid := usageInput(customer, fmt.Sprintf("outside-%d", i), 2)
			invalid.OccurredAt = &at
			rows, err := w.alpha.RecordUsage(t.Context(), []billing.RecordUsageParams{invalid})
			require.NoError(t, err)
			require.Error(t, rows[0].Err())
		}
		balance(t, w.alpha, customer, 200, 0, 200)
	})
	t.Run("OR-J24-S03/batch-item-errors-preserve-neighbors-and-merchant-boundary", func(t *testing.T) {
		a := newAuthorityWorld(t)
		usageMeter(t, a.alphaRemote, 100)
		alice, bob := billing.CustomerID(uuid.MustParse(a.alice.ID)), billing.CustomerID(uuid.MustParse(a.bob.ID))
		input := []billing.RecordUsageParams{usageInput(alice, "alice-good", 2), usageInput(billing.CustomerID{}, "invalid-customer", 5), usageInput(bob, "bob-good", 3)}
		rows, err := a.alphaRemote.RecordUsage(t.Context(), input)
		require.NoError(t, err)
		require.Len(t, rows, 3)
		require.NoError(t, rows[0].Err())
		require.EqualValues(t, 200, rows[0].Event.Amount)
		require.Error(t, rows[1].Err())
		require.Nil(t, rows[1].Event)
		require.NoError(t, rows[2].Err())
		require.Equal(t, bob, rows[2].Event.CustomerID)
		require.EqualValues(t, 300, rows[2].Event.Amount)
		_, err = a.betaRemote.GetUsageEvent(t.Context(), rows[0].Event.ID)
		require.ErrorIs(t, err, billing.ErrNotFound)
		foreign, err := a.betaRemote.ListUsageEvents(t.Context(), billing.UsageEventListParams{CustomerID: alice})
		require.NoError(t, err)
		require.Empty(t, foreign.Items)
		status, raw := a.request(t, http.MethodPost, "/v1/app/usage-events", a.aliceToken, billing.RecordUsageBatchParams{Items: input})
		require.Equal(t, http.StatusForbidden, status, string(raw))
		own, err := a.alphaRemote.ListUsageEvents(t.Context(), billing.UsageEventListParams{CustomerID: alice})
		require.NoError(t, err)
		require.Len(t, own.Items, 1)
	})
	t.Run("OR-J24-S04/grouped-pagination-retains-exact-customer-totals", func(t *testing.T) {
		a := newAuthorityWorld(t)
		usageMeter(t, a.alphaRemote, 10)
		alice, bob := billing.CustomerID(uuid.MustParse(a.alice.ID)), billing.CustomerID(uuid.MustParse(a.bob.ID))
		for i, row := range []struct {
			resource string
			units    int64
		}{{"alpha", 10}, {"alpha", 20}, {"beta", 40}, {"gamma", 5}} {
			input := usageInput(alice, fmt.Sprintf("report-%d", i), row.units)
			input.Resource = row.resource
			oneUsage(t, a.alphaRemote, input)
		}
		foreign := usageInput(bob, "foreign-report", 90)
		foreign.Resource = "foreign"
		oneUsage(t, a.alphaRemote, foreign)
		q := url.Values{"currency": {"USD"}, "group_by": {"resource"}, "from": {a.clock.Now().Add(-time.Hour).Format(time.RFC3339Nano)}, "to": {a.clock.Now().Add(time.Hour).Format(time.RFC3339Nano)}, "limit": {"1"}}
		var all []billing.UsageRow
		var firstCursor string
		for i := 0; i < 3; i++ {
			status, raw := a.request(t, http.MethodGet, "/v1/me/usage?"+q.Encode(), a.aliceToken, nil)
			require.Equal(t, http.StatusOK, status, string(raw))
			var page billing.Usage
			require.NoError(t, json.Unmarshal(raw, &page))
			require.Equal(t, alice, page.CustomerID)
			require.Len(t, page.Rows, 1)
			all = append(all, page.Rows...)
			if i < 2 {
				require.NotNil(t, page.NextCursor)
				q.Set("cursor", *page.NextCursor)
				if i == 0 {
					firstCursor = *page.NextCursor
				}
			} else {
				require.Nil(t, page.NextCursor)
			}
		}
		require.Equal(t, []string{"beta", "alpha", "gamma"}, []string{all[0].Key, all[1].Key, all[2].Key})
		require.Equal(t, []int64{400, 300, 50}, []int64{all[0].Amount, all[1].Amount, all[2].Amount})
		require.Equal(t, []int64{1, 2, 1}, []int64{all[0].EventCount, all[1].EventCount, all[2].EventCount})
		q.Set("cursor", firstCursor)
		status, raw := a.request(t, http.MethodGet, "/v1/me/usage?"+q.Encode(), a.bobToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var scoped billing.Usage
		require.NoError(t, json.Unmarshal(raw, &scoped))
		require.Equal(t, bob, scoped.CustomerID)
		require.Empty(t, scoped.Rows, "a pagination position cannot select another customer's groups")
		q.Del("cursor")
		status, raw = a.request(t, http.MethodGet, "/v1/me/usage?"+q.Encode(), a.bobToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &scoped))
		require.Len(t, scoped.Rows, 1)
		require.Equal(t, "foreign", scoped.Rows[0].Key)
		require.EqualValues(t, 900, scoped.Rows[0].Amount)
	})
}
