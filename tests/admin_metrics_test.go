//go:build integration

package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	chgo "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/tenant"
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
        snapshot_date, currency, tenant_id,
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
		time.Now().UTC(), "usd", tenant.DefaultID.String(),
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

func TestAdminMetricsSummary(t *testing.T) {
	t.Parallel()
	suite, adminToken := setupAdminTestSuite(t)
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID
	seedMetricsData(t, suite, priceID)

	query := "/v1/admin/metrics/summary"
	req, _ := http.NewRequest("GET", query, nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	w := httptest.NewRecorder()
	suite.Server.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "summary response")

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp, "mrr")
	assert.Contains(t, resp, "total_revenue")
	assert.Contains(t, resp, "active_subscriptions")
}

func TestAdminMetricsRevenue(t *testing.T) {
	t.Parallel()
	suite, adminToken := setupAdminTestSuite(t)
	products := suite.SeedProducts()
	seedMetricsData(t, suite, products[0].Prices[0].ID)
	now := time.Now().UTC()
	start := now.AddDate(0, 0, -7).Format("2006-01-02")
	end := now.Format("2006-01-02")

	req, _ := http.NewRequest("GET", fmt.Sprintf("/v1/admin/metrics/revenue?start=%s&end=%s&granularity=day", start, end), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	w := httptest.NewRecorder()
	suite.Server.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp, "buckets")
}

func TestAdminMetricsSubscriptions(t *testing.T) {
	t.Parallel()
	suite, adminToken := setupAdminTestSuite(t)
	products := suite.SeedProducts()
	seedMetricsData(t, suite, products[0].Prices[0].ID)

	req, _ := http.NewRequest("GET", "/v1/admin/metrics/subscriptions?granularity=day", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	w := httptest.NewRecorder()
	suite.Server.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp, "buckets")
}

func TestAdminMetricsProcessors(t *testing.T) {
	t.Parallel()
	suite, adminToken := setupAdminTestSuite(t)
	products := suite.SeedProducts()
	seedMetricsData(t, suite, products[0].Prices[0].ID)

	req, _ := http.NewRequest("GET", "/v1/admin/metrics/processors", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	w := httptest.NewRecorder()
	suite.Server.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp, "processors")
}

func TestAdminMetricsChurn(t *testing.T) {
	t.Parallel()
	suite, adminToken := setupAdminTestSuite(t)
	products := suite.SeedProducts()
	seedMetricsData(t, suite, products[0].Prices[0].ID)

	req, _ := http.NewRequest("GET", "/v1/admin/metrics/churn", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	w := httptest.NewRecorder()
	suite.Server.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp, "monthly_churn")
}
