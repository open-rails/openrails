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
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckoutSupportsConfiguredSecondaryNMIProvider(t *testing.T) {
	suite, mock := SetupSuiteWithMockNMI(t)
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID
	configureSecondaryNMIProvider(t, suite, mock, "acme", priceID)

	userID := uuid.New().String()
	email := "checkout-acme-" + t.Name() + "@test.example.com"
	token := getTestIssuer().CreateToken(userID, email)

	body := map[string]any{
		"price_id": priceID.String(),
		"payment": map[string]any{
			"processor":     "acme",
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
	assert.Equal(t, models.Processor("acme"), sub.Processor)

	pms := suite.GetPaymentMethodsByUserID(userID)
	require.NotEmpty(t, pms)
	assert.Equal(t, models.Processor("acme"), pms[0].Processor)
	assert.GreaterOrEqual(t, int(mock.RequestCount), 1, "should have used the configured NMI client")
}

func TestRenewMembershipDuplicateTransactionIsNoOp(t *testing.T) {
	suite := setupTestSuite(t)
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := "renewal-acme-" + uuid.New().String()[:8]
	now := suite.GetClock().Now().UTC()
	periodEnd := now.Add(30 * 24 * time.Hour)

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              userID,
		PriceID:             priceID,
		Status:              models.StatusActive,
		Processor:           models.Processor("acme"),
		ProcessorSubID:      "acme-sub-" + uuid.New().String()[:8],
		CurrentPeriodEndsAt: &periodEnd,
	})
	defer suite.CleanupSubscriptionsForUser(userID)

	txnID := "acme-renew-" + uuid.New().String()[:8]
	err := suite.App.Runtime.SubscriptionLifecycleService.RenewMembership(context.Background(), &subscriptions.RenewMembershipParams{
		Processor:               models.Processor("acme"),
		ProcessorSubscriptionID: sub.ProcessorSubscriptionID,
		TransactionID:           txnID,
		Amount:                  sub.Price.Amount,
		Currency:                sub.Price.Currency,
	})
	require.NoError(t, err)

	afterFirst := suite.GetSubscription(sub.ID)
	require.NotNil(t, afterFirst.CurrentPeriodEndsAt)
	firstPeriodEnd := *afterFirst.CurrentPeriodEndsAt

	err = suite.App.Runtime.SubscriptionLifecycleService.RenewMembership(context.Background(), &subscriptions.RenewMembershipParams{
		Processor:               models.Processor("acme"),
		ProcessorSubscriptionID: sub.ProcessorSubscriptionID,
		TransactionID:           txnID,
		Amount:                  sub.Price.Amount,
		Currency:                sub.Price.Currency,
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
			assert.Equal(t, models.Processor("acme"), payment.Processor)
		}
	}
	assert.Equal(t, 1, matched, "expected exactly one payment record for the renewal transaction")
}

func configureSecondaryNMIProvider(t *testing.T, suite *TestContainerSuite, mock *MockNMIServer, provider string, priceID uuid.UUID) {
	t.Helper()

	provider = strings.ToLower(provider)
	suite.Config.Processors[provider] = &config.ProcessorConfig{
		Type:          config.ProcessorTypeNMI,
		SecurityKey:   "test-security-key-" + provider,
		DirectPostURL: mock.URL(),
		QueryURL:      mock.URL(),
	}
	processors.InitNMIBackedProcessors(suite.Config)

	settings := &config.NMIProviderSettings{
		Name:        provider,
		SecurityKey: "test-security-key-" + provider,
		TestMode:    true,
	}
	client, err := nmi.NewClient(provider, settings, true)
	require.NoError(t, err)
	client.DirectPostURL = mock.URL()

	suite.App.Runtime.NMIClients[provider] = client
	if suite.App.Runtime.SubscriptionService != nil {
		suite.App.Runtime.SubscriptionService.NMIClients = suite.App.Runtime.NMIClients
	}
	if suite.App.Runtime.VaultService != nil {
		suite.App.Runtime.VaultService.NMIClients = suite.App.Runtime.NMIClients
	}
	if suite.App.Runtime.CheckoutService != nil {
		suite.App.Runtime.CheckoutService.NMIClients = suite.App.Runtime.NMIClients
		if suite.App.Runtime.CheckoutService.NMISaleService != nil {
			suite.App.Runtime.CheckoutService.NMISaleService.NMIClients = suite.App.Runtime.NMIClients
		}
	}

	price := suite.GetPrice(priceID)
	if price.Processors == nil {
		price.Processors = map[string]map[string]string{}
	}
	price.Processors[provider] = map[string]string{
		models.ProcessorKeyPlanID: provider + "-plan",
	}
	_, err = suite.BunDB.NewUpdate().Model(price).Column("processors").WherePK().Exec(context.Background())
	require.NoError(t, err)
}
