//go:build integration

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckoutSupportsConfiguredSecondaryNMIProvider(t *testing.T) {
	suite, mock := SetupSuiteWithMockNMI(t)
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID
	const provider = "secondary-nmi"
	configureSecondaryNMIProvider(t, suite, mock, provider, priceID)

	userID := uuid.New().String()
	email := "checkout-nmi-" + uuid.NewString() + "@test.example.com"
	token := suite.MintUserToken(userID, email)

	body := map[string]any{
		"price_id": priceID.String(),
		"payment": map[string]any{
			"rail":          provider,
			"payment_token": "tok_test_123",
			"email":         email,
			"first_name":    "Test",
			"last_name":     "User",
			"address1":      "123 Test St",
			"city":          "Test City",
			"state":         "CA",
			"zip":           "90210",
			"country":       "US",
		},
	}
	jsonBody, err := json.Marshal(body)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/checkout", bytes.NewReader(jsonBody))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	suite.Server.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK, got body: %s", w.Body.String())

	subs := suite.GetAllSubscriptionsByUserID(userID)
	require.NotEmpty(t, subs, "expected subscription records")
	sub := subs[0]
	assert.Equal(t, models.Rail("nmi"), sub.Rail)
	assert.Equal(t, models.StatusActive, sub.Status)
	require.NotNil(t, sub.CurrentPeriodStartsAt)
	require.NotNil(t, sub.CurrentPeriodEndsAt)

	pms := suite.GetPaymentMethodsByUserID(userID)
	require.NotEmpty(t, pms)
	assert.Equal(t, models.Rail("nmi"), pms[0].Rail)

	payments := suite.GetPaymentsByUserID(userID)
	require.NotEmpty(t, payments)
	var completedPayment *models.Payment
	for _, payment := range payments {
		if payment.SubscriptionID != nil && *payment.SubscriptionID == sub.ID && payment.Status == "completed" {
			completedPayment = payment
			break
		}
	}
	require.NotNil(t, completedPayment, "expected a completed payment linked to the activated subscription")

	// MerchantCtx, not a bare merchant-in-context: entitlements FORCEs RLS, and
	// the runtime's EntitlementService runs on the server's BASE pool, so an
	// unpinned read matches zero rows and reports "not entitled" for a grant
	// that exists.
	entitled, err := suite.App.Runtime.EntitlementService.IsEntitled(suite.MerchantCtx(), userID, "premium", suite.GetClock().Now().UTC())
	require.NoError(t, err)
	assert.True(t, entitled, "NMI checkout should grant premium access synchronously after approval")
	assert.GreaterOrEqual(t, int(mock.RequestCount), 1, "should have used the configured NMI client")
}

func TestRenewMembershipDuplicateTransactionIsNoOp(t *testing.T) {
	suite := getSharedTestSuite(t)
	products := suite.SeedProducts()
	price := products[0].Prices[0]
	priceID := price.ID

	userID := uuid.New().String()
	now := suite.GetClock().Now().UTC()
	periodEnd := now.Add(30 * 24 * time.Hour)

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              userID,
		PriceID:             priceID,
		Status:              models.StatusActive,
		Rail:                models.Rail("nmi"),
		RailSubID:           "nmi-sub-" + uuid.New().String()[:8],
		CurrentPeriodEndsAt: &periodEnd,
	})
	defer suite.CleanupSubscriptionsForUser(userID)

	txnID := "nmi-renew-" + uuid.New().String()[:8]
	ctx := suite.MerchantCtx()
	err := suite.App.Runtime.SubscriptionLifecycleService.RenewMembership(ctx, &subscriptions.RenewMembershipParams{
		Rail:               models.Rail("nmi"),
		RailSubscriptionID: sub.RailSubscriptionID,
		TransactionID:      txnID,
		Amount:             price.Amount,
		Currency:           price.Currency,
	})
	require.NoError(t, err)

	afterFirst := suite.GetSubscription(sub.ID)
	require.NotNil(t, afterFirst.CurrentPeriodEndsAt)
	firstPeriodEnd := *afterFirst.CurrentPeriodEndsAt

	err = suite.App.Runtime.SubscriptionLifecycleService.RenewMembership(ctx, &subscriptions.RenewMembershipParams{
		Rail:               models.Rail("nmi"),
		RailSubscriptionID: sub.RailSubscriptionID,
		TransactionID:      txnID,
		Amount:             price.Amount,
		Currency:           price.Currency,
	})
	require.NoError(t, err)

	afterSecond := suite.GetSubscription(sub.ID)
	require.NotNil(t, afterSecond.CurrentPeriodEndsAt)
	assert.True(t, afterSecond.CurrentPeriodEndsAt.Equal(firstPeriodEnd), "duplicate renewal should not advance the billing window again")

	payments := suite.GetPaymentsByUserID(userID)
	matched := 0
	for _, payment := range payments {
		if payment.TransactionID == txnID {
			matched++
			assert.Equal(t, models.Rail("nmi"), payment.Rail)
		}
	}
	assert.Equal(t, 1, matched, "expected exactly one payment record for the renewal transaction")
}

func configureSecondaryNMIProvider(t *testing.T, suite *TestContainerSuite, mock *MockNMIServer, provider string, priceID uuid.UUID) {
	t.Helper()

	provider = strings.ToLower(provider)
	// #788: the secondary account arms as psps state; every
	// consumer resolves it through the ONE seam. The mock gateway serves it.
	suite.SeedNMIPSP(t, provider, "test-security-key-"+provider)
	suite.SetNMIGateway(mock.URL())

	price := suite.GetPrice(priceID)
	if price.PSPLinks == nil {
		price.PSPLinks = map[string]map[string]string{}
	}
	price.PSPLinks[provider] = map[string]string{
		models.RailKeyRail:   string(models.RailNMI),
		models.RailKeyPlanID: provider + "-plan",
	}
	railsJSON, err := json.Marshal(price.PSPLinks)
	require.NoError(t, err)
	_, err = suite.Pool.Exec(context.Background(),
		"UPDATE openrails.prices SET psp_links = $1 WHERE id = $2", railsJSON, price.ID)
	require.NoError(t, err)
}
