//go:build integration

package tests

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestCCBillUpgradeSuccess_ParsesBilledInitialPrice(t *testing.T) {
	suite := setupTestSuite(t)
	products := suite.SeedProducts()
	require.NotEmpty(t, products)
	require.GreaterOrEqual(t, len(products[0].Prices), 2)

	oldPriceID := products[0].Prices[0].ID
	newPrice := products[0].Prices[1]

	userID := uuid.New().String()
	originalProcessorSubID := "ccbill_sub_upgrade_old_" + uuid.New().String()
	newProcessorSubID := "ccbill_sub_upgrade_new_" + uuid.New().String()

	created := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:         userID,
		PriceID:        oldPriceID,
		Status:         models.StatusActive,
		Processor:      models.ProcessorCCBill,
		ProcessorSubID: originalProcessorSubID,
	})
	require.NotNil(t, created)
	require.Equal(t, oldPriceID, created.PriceID)

	payload := mustLoadJSONMap(t, "testdata/webhooks/ccbill/upgradesuccess.json")
	payload["subscriptionId"] = newProcessorSubID
	payload["originalSubscriptionId"] = originalProcessorSubID
	payload["originalClientAccnum"] = 945280
	payload["originalClientSubacc"] = "0000"
	payload["source"] = "FORM"
	payload["scaResponseStatus"] = "Y"
	payload["transactionId"] = "ccbill_txn_upgrade_" + uuid.New().String()
	payload["timestamp"] = time.Now().UTC().Format("2006-01-02 15:04:05")
	payload["flexId"] = "ccbill_quarterly_usd_2499"
	payload["formName"] = "FormQuarterlyUSD"
	payload["billedInitialPrice"] = "24.99"
	payload["billedRecurringPrice"] = "24.99"
	payload["subscriptionInitialPrice"] = "24.99"
	payload["subscriptionRecurringPrice"] = "24.99"
	delete(payload, "amount")

	postCCBillWebhook(t, suite.ServerURL, "UpgradeSuccess", payload)

	require.Eventually(t, func() bool {
		sub := suite.GetSubscriptionByProcessorID(newProcessorSubID)
		return sub != nil && sub.Status == models.StatusActive && sub.PriceID == newPrice.ID
	}, 10*time.Second, 200*time.Millisecond)

	updated := suite.GetSubscriptionByProcessorID(newProcessorSubID)
	require.NotNil(t, updated)
	require.Equal(t, models.StatusActive, updated.Status)
	require.Equal(t, newPrice.ID, updated.PriceID)

	oldLookup := suite.GetSubscriptionByProcessorID(originalProcessorSubID)
	require.Nil(t, oldLookup)
}
