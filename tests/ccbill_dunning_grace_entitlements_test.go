//go:build integration

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
)

func TestCCBillDunningGraceEntitlements(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	// Ensure catalog + profile mapping exists for webhook resolution.
	products := suite.SeedProducts()
	require.NotEmpty(t, products)

	userID := uuid.New().String()
	username := "ccbill_dunning_" + uuid.New().String()
	suite.CreateProfileUser(userID, username)

	subID := "ccbill_sub_" + uuid.New().String()
	initialTxnID := "ccbill_txn_init_" + uuid.New().String()
	renewalFailTxnID := "ccbill_txn_fail_" + uuid.New().String()
	renewalSuccessTxnID := "ccbill_txn_renew_" + uuid.New().String()

	startTS := time.Now().UTC().Add(2 * time.Minute).Truncate(time.Second)
	paidTermEnd := startTS.Add(30 * 24 * time.Hour)
	paidTermEndTS := time.Date(paidTermEnd.Year(), paidTermEnd.Month(), paidTermEnd.Day(), 23, 59, 59, 0, time.UTC)

	// 1) NewSaleSuccess creates subscription + finite entitlement window ending at nextRenewalDate.
	newSale := mustLoadJSONMap(t, "testdata/webhooks/ccbill/newsalesuccess.json")
	newSale["username"] = username
	newSale["subscriptionId"] = subID
	newSale["transactionId"] = initialTxnID
	newSale["timestamp"] = startTS.Format("2006-01-02 15:04:05")
	newSale["nextRenewalDate"] = paidTermEnd.Format("2006-01-02")
	postCCBillWebhook(t, suite.ServerURL, "NewSaleSuccess", newSale)

	require.Eventually(t, func() bool {
		sub := suite.GetSubscriptionByProcessorID(subID)
		return sub != nil && sub.Status == models.StatusActive && sub.CurrentPeriodEndsAt != nil
	}, 10*time.Second, 200*time.Millisecond)

	sub := suite.GetSubscriptionByProcessorID(subID)
	require.NotNil(t, sub)
	require.Equal(t, userID, sub.TenantSubjectID.String())
	require.Equal(t, models.ProcessorCCBill, sub.Processor)

	require.NotNil(t, sub.CurrentPeriodEndsAt)
	require.True(t, sub.CurrentPeriodEndsAt.Equal(paidTermEndTS), "current_period_ends_at should match nextRenewalDate + timestamp time-of-day")

	ent := mustGetSubscriptionEntitlement(t, suite, ctx, userID, sub.ID, "premium")
	require.NotNil(t, ent.EndAt)
	require.True(t, ent.EndAt.Equal(paidTermEndTS), "entitlement end_at should match paid term end")

	// 2) RenewalFailure marks past_due and sets next_retry_at.
	renewalFailure := mustLoadJSONMap(t, "testdata/webhooks/ccbill/renewalfailure.json")
	renewalFailure["subscriptionId"] = subID
	renewalFailure["transactionId"] = renewalFailTxnID
	renewalFailure["timestamp"] = paidTermEndTS.Format("2006-01-02 15:04:05")
	nextRetryAt := paidTermEndTS.Add(3 * 24 * time.Hour)
	renewalFailure["nextRetryDate"] = nextRetryAt.Format("2006-01-02")
	postCCBillWebhook(t, suite.ServerURL, "RenewalFailure", renewalFailure)

	require.Eventually(t, func() bool {
		s := suite.GetSubscriptionByProcessorID(subID)
		return s != nil && s.Status == models.StatusPastDue && s.NextRetryAt != nil && s.NextRetryAt.Equal(nextRetryAt)
	}, 10*time.Second, 200*time.Millisecond)

	sub = suite.GetSubscriptionByProcessorID(subID)
	require.NotNil(t, sub)
	require.Equal(t, models.StatusPastDue, sub.Status)
	require.NotNil(t, sub.NextRetryAt)
	require.True(t, sub.NextRetryAt.Equal(nextRetryAt))
	require.Nil(t, sub.RetryAttempts, "CCBill should not use NMI-style retry_attempts")
	require.Nil(t, sub.LastRetryAt, "CCBill should not use NMI-style last_retry_at")
	require.NotNil(t, sub.GraceEndsAt)
	require.True(t, sub.GraceEndsAt.Equal(nextRetryAt))

	ent = mustGetSubscriptionEntitlement(t, suite, ctx, userID, sub.ID, "premium")
	require.NotNil(t, ent.EndAt)
	require.True(t, ent.EndAt.Equal(paidTermEndTS), "paid entitlement end_at should remain at paid term end")

	// 3) RenewalSuccess clears dunning/grace and extends the paid term + entitlement window.
	renewalSuccess := mustLoadJSONMap(t, "testdata/webhooks/ccbill/renewalsuccess.json")
	renewalSuccess["subscriptionId"] = subID
	renewalSuccess["transactionId"] = renewalSuccessTxnID
	renewalSuccess["timestamp"] = nextRetryAt.Format("2006-01-02 15:04:05")
	renewalSuccess["renewalDate"] = nextRetryAt.Format("2006-01-02")
	newPaidTermEnd := nextRetryAt.Add(30 * 24 * time.Hour)
	newPaidTermEndTS := time.Date(newPaidTermEnd.Year(), newPaidTermEnd.Month(), newPaidTermEnd.Day(), nextRetryAt.Hour(), nextRetryAt.Minute(), nextRetryAt.Second(), 0, time.UTC)
	renewalSuccess["nextRenewalDate"] = newPaidTermEnd.Format("2006-01-02")
	postCCBillWebhook(t, suite.ServerURL, "RenewalSuccess", renewalSuccess)

	require.Eventually(t, func() bool {
		s := suite.GetSubscriptionByProcessorID(subID)
		return s != nil && s.Status == models.StatusActive && s.CurrentPeriodEndsAt != nil && s.CurrentPeriodEndsAt.Equal(newPaidTermEndTS)
	}, 10*time.Second, 200*time.Millisecond)

	sub = suite.GetSubscriptionByProcessorID(subID)
	require.Equal(t, models.StatusActive, sub.Status)
	require.NotNil(t, sub.CurrentPeriodEndsAt)
	require.True(t, sub.CurrentPeriodEndsAt.Equal(newPaidTermEndTS))
	require.Nil(t, sub.NextRetryAt)
	require.Nil(t, sub.GraceEndsAt)

	ent = mustGetSubscriptionEntitlement(t, suite, ctx, userID, sub.ID, "premium")
	require.NotNil(t, ent.EndAt)
	require.True(t, ent.EndAt.Equal(newPaidTermEndTS), "entitlement end_at should extend to new paid term end")
}

func TestCCBillCancellationKeepsAccessUntilPaidTermEnd(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	_ = suite.SeedProducts()

	userID := uuid.New().String()
	username := "ccbill_cancel_" + uuid.New().String()
	suite.CreateProfileUser(userID, username)

	subID := "ccbill_sub_cancel_" + uuid.New().String()
	initialTxnID := "ccbill_txn_init_" + uuid.New().String()

	startTS := time.Now().UTC().Add(2 * time.Minute).Truncate(time.Second)
	paidTermEnd := startTS.Add(30 * 24 * time.Hour)
	paidTermEndTS := time.Date(paidTermEnd.Year(), paidTermEnd.Month(), paidTermEnd.Day(), 23, 59, 59, 0, time.UTC)

	newSale := mustLoadJSONMap(t, "testdata/webhooks/ccbill/newsalesuccess.json")
	newSale["username"] = username
	newSale["subscriptionId"] = subID
	newSale["transactionId"] = initialTxnID
	newSale["timestamp"] = startTS.Format("2006-01-02 15:04:05")
	newSale["nextRenewalDate"] = paidTermEnd.Format("2006-01-02")
	postCCBillWebhook(t, suite.ServerURL, "NewSaleSuccess", newSale)

	require.Eventually(t, func() bool {
		sub := suite.GetSubscriptionByProcessorID(subID)
		return sub != nil && sub.Status == models.StatusActive && sub.CurrentPeriodEndsAt != nil
	}, 10*time.Second, 200*time.Millisecond)

	sub := suite.GetSubscriptionByProcessorID(subID)
	require.NotNil(t, sub)
	require.NotNil(t, sub.CurrentPeriodEndsAt)
	require.True(t, sub.CurrentPeriodEndsAt.Equal(paidTermEndTS))

	// Cancel mid-term; do not revoke immediately.
	cancel := mustLoadJSONMap(t, "testdata/webhooks/ccbill/cancellation.json")
	cancel["subscriptionId"] = subID
	cancel["timestamp"] = startTS.Add(10 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	cancel["source"] = "merchant"
	cancel["reason"] = "Customer requested cancellation"
	postCCBillWebhook(t, suite.ServerURL, "Cancellation", cancel)

	require.Eventually(t, func() bool {
		s := suite.GetSubscriptionByProcessorID(subID)
		return s != nil && s.Status == models.StatusCancelled && s.EndedAt != nil && s.EndedAt.Equal(paidTermEndTS)
	}, 10*time.Second, 200*time.Millisecond)

	sub = suite.GetSubscriptionByProcessorID(subID)
	require.Equal(t, models.StatusCancelled, sub.Status)
	require.NotNil(t, sub.EndedAt)
	require.True(t, sub.EndedAt.Equal(paidTermEndTS), "ended_at should be set to paid term end for period-end cancellation")

	ent := mustGetSubscriptionEntitlement(t, suite, ctx, userID, sub.ID, "premium")
	require.NotNil(t, ent.EndAt)
	require.True(t, ent.EndAt.Equal(paidTermEndTS), "entitlement end_at should remain at paid term end (no immediate revocation)")
	require.Nil(t, ent.RevokedAt, "entitlement should not be revoked on CCBill cancellation webhook")
}

func postCCBillWebhook(t *testing.T, baseURL, eventType string, payload map[string]any) {
	t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/webhooks/ccbill?eventType="+eventType, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	require.True(t, resp.StatusCode >= 200 && resp.StatusCode < 300, "expected 2xx response, got %d", resp.StatusCode)
}

func mustLoadJSONMap(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		b, err = os.ReadFile(filepath.Join("..", path))
	}
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

func mustGetSubscriptionEntitlement(t *testing.T, suite *TestContainerSuite, ctx context.Context, userID string, subscriptionID uuid.UUID, entitlement string) *models.Entitlement {
	t.Helper()
	ents := suite.QueryEntitlements(ctx, `
		WHERE tenant_subject_id = $1
		  AND entitlement = $2
		  AND source_type = $3
		  AND source_id = $4
		  AND deleted_at IS NULL
		ORDER BY start_at DESC
		LIMIT 1`,
		suite.ensureTenantSubject(ctx, userID), entitlement,
		string(models.EntitlementSourceSubscription), subscriptionID)
	require.Len(t, ents, 1, "expected a subscription entitlement window")
	return &ents[0]
}
