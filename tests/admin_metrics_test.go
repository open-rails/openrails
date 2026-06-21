//go:build integration

package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	chgo "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
)

func seedMetricsData(t *testing.T, suite *TestContainerSuite, priceID uuid.UUID) {
	t.Helper()
	userID := uuid.New().String()
	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:         userID,
		PriceID:        priceID,
		Status:         models.StatusActive,
		Processor:      models.ProcessorMobius,
		ProcessorSubID: "metrics-" + uuid.NewString()[:8],
	})

	suite.CreateTestPaymentWithOptions(PaymentOptions{
		UserID:         userID,
		PriceID:        priceID,
		SubscriptionID: &sub.ID,
		Processor:      models.ProcessorMobius,
		Amount:         999,
		TransactionID:  "txn-" + uuid.NewString()[:8],
	})

	seedClickHouseDailyMetrics(t, suite, 999)
}

func seedClickHouseDailyMetrics(t *testing.T, suite *TestContainerSuite, amount int64) {
	t.Helper()
	cfg := suite.Config.ClickHouse
	require.NotNil(t, cfg)

	conn, err := chgo.Open(&chgo.Options{
		Addr: []string{cfg.ClientAddr},
		Auth: chgo.Auth{
			Database: cfg.Database,
			Username: cfg.Username,
			Password: cfg.Password,
		},
	})
	require.NoError(t, err)
	defer conn.Close()

	ctx, cancel := context.WithTimeout(suite.ctx, 5*time.Second)
	defer cancel()
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO daily_metrics (
        snapshot_date, currency, merchant_id,
        subscription_revenue_cents, one_time_revenue_cents, refunds_cents, chargebacks_cents,
        total_revenue_cents, total_revenue_net_cents,
        payments_successful, payments_failed, avg_payment_amount_cents,
        new_subscriptions, scheduled_starts,
        cancellations_user, cancellations_merchant, cancellations_expired, cancellations_chargeback,
        reactivations, active_count_end, past_due_count_end, pending_count_end, mrr_cents, entitlements_granted,
        processor.name, processor.active_subscriptions, processor.new_subscriptions, processor.cancellations,
        processor.revenue_total_cents, processor.revenue_subscription_cents, processor.revenue_one_time_cents,
        processor.revenue_refunds_cents, processor.revenue_chargebacks_cents,
        processor.payments_successful, processor.payments_failed
    ) VALUES`)
	require.NoError(t, err)

	scheduled := int64(0)
	entitlements := int64(1)
	require.NoError(t, batch.Append(
		time.Now().UTC(), "usd", dbtest.TestMerchantID.String(),
		amount, int64(0), int64(0), int64(0),
		amount, amount,
		int64(1), int64(0), amount,
		int64(1), &scheduled,
		int64(0), int64(0), int64(0), int64(0),
		int64(0), int64(1), int64(0), int64(0), amount, &entitlements,
		[]string{string(models.ProcessorMobius)}, []int64{1}, []int64{1}, []int64{0},
		[]int64{amount}, []int64{amount}, []int64{0},
		[]int64{0}, []int64{0},
		[]int64{1}, []int64{0},
	))
	require.NoError(t, batch.Send())
}

// TestAdminMetricsFolded exercises the #528 folded GET /v1/admin/metrics endpoint
// (was /metrics/{summary,revenue,subscriptions,processors,churn}). With a single
// currency present each section is a bare object; the response carries all five
// sections in one document. Authorized by merchant:usage:read.
func TestAdminMetricsFolded(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := newHostSeamAdminRouter(t, suite, "bd000000-0000-4000-8000-000000000003",
		[]string{controlplane.PermMerchantUsageRead})

	products := suite.SeedProducts()
	seedMetricsData(t, suite, products[0].Prices[0].ID)

	req, _ := http.NewRequest("GET", "/v1/admin/metrics?granularity=day", nil)
	req.Header.Set("Authorization", "Bearer host-credential")
	w := httptest.NewRecorder()
	admin.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	// summary section
	summary, ok := resp["summary"].(map[string]any)
	require.True(t, ok, "response must carry a summary object: %s", w.Body.String())
	assert.Contains(t, summary, "mrr")
	assert.Contains(t, summary, "total_revenue")
	assert.Contains(t, summary, "active_subscriptions")

	// revenue series section
	revenue, ok := resp["revenue"].(map[string]any)
	require.True(t, ok, "response must carry a revenue object")
	revBuckets, ok := revenue["buckets"].([]any)
	require.True(t, ok, "revenue should include buckets array: %s", w.Body.String())
	assert.NotEmpty(t, revBuckets)

	// subscription series section
	subscriptions, ok := resp["subscriptions"].(map[string]any)
	require.True(t, ok, "response must carry a subscriptions object")
	subBuckets, ok := subscriptions["buckets"].([]any)
	require.True(t, ok, "subscriptions should include buckets array: %s", w.Body.String())
	assert.NotEmpty(t, subBuckets)

	// processors section
	processors, ok := resp["processors"].(map[string]any)
	require.True(t, ok, "response must carry a processors object")
	procList, ok := processors["processors"].([]any)
	require.True(t, ok, "processors should include processors array: %s", w.Body.String())
	assert.NotEmpty(t, procList)

	// churn section
	churn, ok := resp["churn"].(map[string]any)
	require.True(t, ok, "response must carry a churn object")
	assert.Contains(t, churn, "monthly_churn")
}

// TestAdminMetrics_RequiresMetricsRead proves the delegated gate fails closed:
// a principal without metrics:read cannot read the dashboard.
func TestAdminMetrics_RequiresMetricsRead(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := newHostSeamAdminRouter(t, suite, "bd000000-0000-4000-8000-000000000004",
		[]string{controlplane.PermMerchantCustomersRead})

	req, _ := http.NewRequest("GET", "/v1/admin/metrics?granularity=day", nil)
	req.Header.Set("Authorization", "Bearer host-credential")
	w := httptest.NewRecorder()
	admin.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}
