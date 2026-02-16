//go:build integration

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestCCBillUserReactivation_RestoresEntitlementsAfterExpiration(t *testing.T) {
	suite, userID, processorSubID, subscriptionID, now := seedCCBillActiveSubscriptionWithEntitlement(t)
	ctx := context.Background()

	postCCBillTerminalEvent(t, suite, "Expiration", "testdata/webhooks/ccbill/expiration.json", processorSubID, now)

	require.Eventually(t, func() bool {
		sub := suite.GetSubscriptionByProcessorID(processorSubID)
		return sub != nil && sub.Status == models.StatusCancelled
	}, 10*time.Second, 200*time.Millisecond)

	entitled, err := suite.App.Runtime.EntitlementService.IsEntitled(ctx, userID, "premium", time.Now().UTC().Add(time.Second))
	require.NoError(t, err)
	require.False(t, entitled)

	reactivationPayload := mustLoadJSONMap(t, "testdata/webhooks/ccbill/userreactivation.json")
	nextRenewalDate := now.Add(14 * 24 * time.Hour)
	transactionID := "ccbill_reactivate_txn_" + uuid.New().String()
	reactivationPayload["subscriptionId"] = processorSubID
	reactivationPayload["transactionId"] = transactionID
	reactivationPayload["clientAccnum"] = "945280"
	reactivationPayload["clientSubacc"] = "0000"
	reactivationPayload["price"] = "24.99 for 90 days"
	reactivationPayload["email"] = "reactivated@example.com"
	reactivationPayload["nextRenewalDate"] = nextRenewalDate.Format("2006-01-02")

	postCCBillWebhook(t, suite.ServerURL, "UserReactivation", reactivationPayload)

	expectedPeriodEnd := time.Date(nextRenewalDate.Year(), nextRenewalDate.Month(), nextRenewalDate.Day(), 23, 59, 59, 0, time.UTC)
	require.Eventually(t, func() bool {
		sub := suite.GetSubscriptionByProcessorID(processorSubID)
		if sub == nil || sub.Status != models.StatusActive || sub.CurrentPeriodEndsAt == nil {
			return false
		}
		return sub.CurrentPeriodEndsAt.Equal(expectedPeriodEnd)
	}, 10*time.Second, 200*time.Millisecond)

	entitled, err = suite.App.Runtime.EntitlementService.IsEntitled(ctx, userID, "premium", time.Now().UTC().Add(time.Second))
	require.NoError(t, err)
	require.True(t, entitled)

	latestEnt := mustGetSubscriptionEntitlement(t, suite, ctx, userID, subscriptionID, "premium")
	require.NotNil(t, latestEnt.EndAt)
	require.True(t, latestEnt.EndAt.Equal(expectedPeriodEnd))
	require.Nil(t, latestEnt.RevokedAt)
}

func TestCCBillUserReactivation_BlocksTerminalChargebackTransition(t *testing.T) {
	suite, userID, processorSubID, _, now := seedCCBillActiveSubscriptionWithEntitlement(t)
	ctx := context.Background()

	postCCBillTerminalEvent(t, suite, "Chargeback", "testdata/webhooks/ccbill/chargeback.json", processorSubID, now)

	require.Eventually(t, func() bool {
		sub := suite.GetSubscriptionByProcessorID(processorSubID)
		return sub != nil && sub.Status == models.StatusCancelled
	}, 10*time.Second, 200*time.Millisecond)

	cancelled := suite.GetSubscriptionByProcessorID(processorSubID)
	require.NotNil(t, cancelled)
	require.NotNil(t, cancelled.CancelType)
	require.Equal(t, models.CancelTypeChargeback, *cancelled.CancelType)

	entitled, err := suite.App.Runtime.EntitlementService.IsEntitled(ctx, userID, "premium", time.Now().UTC().Add(time.Second))
	require.NoError(t, err)
	require.False(t, entitled)

	reactivationPayload := mustLoadJSONMap(t, "testdata/webhooks/ccbill/userreactivation.json")
	transactionID := "ccbill_reactivate_txn_" + uuid.New().String()
	reactivationPayload["subscriptionId"] = processorSubID
	reactivationPayload["transactionId"] = transactionID
	reactivationPayload["clientAccnum"] = "945280"
	reactivationPayload["clientSubacc"] = "0000"
	reactivationPayload["price"] = "24.99 for 90 days"
	reactivationPayload["email"] = "reactivated@example.com"
	reactivationPayload["nextRenewalDate"] = now.Add(14 * 24 * time.Hour).Format("2006-01-02")

	postCCBillWebhook(t, suite.ServerURL, "UserReactivation", reactivationPayload)

	require.Eventually(t, func() bool {
		sub := suite.GetSubscriptionByProcessorID(processorSubID)
		return sub != nil && sub.Status == models.StatusCancelled && sub.CancelType != nil && *sub.CancelType == models.CancelTypeChargeback
	}, 10*time.Second, 200*time.Millisecond)

	entitled, err = suite.App.Runtime.EntitlementService.IsEntitled(ctx, userID, "premium", time.Now().UTC().Add(time.Second))
	require.NoError(t, err)
	require.False(t, entitled)
}

func TestCCBillRenewalSuccess_BlocksTerminalChargebackTransition(t *testing.T) {
	suite, userID, processorSubID, _, now := seedCCBillActiveSubscriptionWithEntitlement(t)
	ctx := context.Background()

	postCCBillTerminalEvent(t, suite, "Chargeback", "testdata/webhooks/ccbill/chargeback.json", processorSubID, now)

	require.Eventually(t, func() bool {
		sub := suite.GetSubscriptionByProcessorID(processorSubID)
		return sub != nil && sub.Status == models.StatusCancelled
	}, 10*time.Second, 200*time.Millisecond)

	initial := suite.GetSubscriptionByProcessorID(processorSubID)
	require.NotNil(t, initial)
	require.NotNil(t, initial.CancelType)
	require.Equal(t, models.CancelTypeChargeback, *initial.CancelType)

	renewalPayload := mustLoadJSONMap(t, "testdata/webhooks/ccbill/renewalsuccess.json")
	renewalTxnID := "ccbill_renewal_after_chargeback_" + uuid.New().String()
	renewalPayload["subscriptionId"] = processorSubID
	renewalPayload["transactionId"] = renewalTxnID
	renewalPayload["timestamp"] = now.Add(2 * time.Hour).Format("2006-01-02 15:04:05")
	renewalPayload["nextRenewalDate"] = now.Add(30 * 24 * time.Hour).Format("2006-01-02")

	postCCBillWebhook(t, suite.ServerURL, "RenewalSuccess", renewalPayload)

	require.Eventually(t, func() bool {
		sub := suite.GetSubscriptionByProcessorID(processorSubID)
		return sub != nil && sub.Status == models.StatusCancelled && sub.CancelType != nil && *sub.CancelType == models.CancelTypeChargeback
	}, 10*time.Second, 200*time.Millisecond)

	entitled, err := suite.App.Runtime.EntitlementService.IsEntitled(ctx, userID, "premium", time.Now().UTC().Add(time.Second))
	require.NoError(t, err)
	require.False(t, entitled)

	count, err := suite.BunDB.NewSelect().
		Model((*models.Payment)(nil)).
		Where("purch.processor = ?", models.ProcessorCCBill).
		Where("purch.transaction_id = ?", renewalTxnID).
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, count)
}

func seedCCBillActiveSubscriptionWithEntitlement(t *testing.T) (*TestContainerSuite, string, string, uuid.UUID, time.Time) {
	t.Helper()

	suite := setupTestSuite(t)
	ctx := context.Background()

	products := suite.SeedProducts()
	require.NotEmpty(t, products)
	require.NotEmpty(t, products[0].Prices)

	userID := uuid.New().String()
	processorSubID := "ccbill_reactivate_" + uuid.New().String()
	now := time.Now().UTC().Truncate(time.Second)

	created := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:         userID,
		PriceID:        products[0].Prices[0].ID,
		Status:         models.StatusActive,
		Processor:      models.ProcessorCCBill,
		ProcessorSubID: processorSubID,
		PeriodStart:    now.Add(-5 * 24 * time.Hour),
		PeriodEnd:      now.Add(25 * 24 * time.Hour),
	})
	require.NotNil(t, created)

	startAt := now.Add(-5 * 24 * time.Hour)
	endAt := now.Add(25 * 24 * time.Hour)
	subID := created.ID
	_, err := suite.BunDB.NewInsert().Model(&models.Entitlement{
		ID:          uuid.New(),
		UserID:      userID,
		Entitlement: "premium",
		StartAt:     startAt,
		EndAt:       &endAt,
		SourceType:  models.EntitlementSourceSubscription,
		SourceID:    &subID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}).Exec(ctx)
	require.NoError(t, err)

	return suite, userID, processorSubID, created.ID, now
}

func postCCBillTerminalEvent(t *testing.T, suite *TestContainerSuite, eventType, fixturePath, processorSubID string, at time.Time) {
	t.Helper()

	terminalPayload := mustLoadJSONMap(t, fixturePath)
	terminalPayload["subscriptionId"] = processorSubID
	terminalPayload["timestamp"] = at.Format("2006-01-02 15:04:05")
	terminalPayload["transactionId"] = "ccbill_term_" + uuid.New().String()
	postCCBillWebhook(t, suite.ServerURL, eventType, terminalPayload)
}
