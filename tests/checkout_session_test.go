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

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckoutSessionRequiresAuth(t *testing.T) {
	suite := getSharedTestSuite(t)
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	body := map[string]any{
		"price_id": priceID.String(),
		"payment": map[string]any{
			"rail": "nmi",
		},
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/checkout", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")

	suite.Server.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code, "Should return 401 without auth")
}

func TestCheckoutSessionMobiusSubscription(t *testing.T) {
	suite, mock := SetupSuiteWithMockNMI(t)
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()
	email := "checkout-session-mobius-" + t.Name() + "@test.example.com"
	token := getTestIssuer().CreateToken(userID, email)

	mock.Reset()

	body := map[string]any{
		"price_id": priceID.String(),
		"payment": map[string]any{
			"rail":          "nmi",
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
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/checkout", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	suite.Server.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK, got body: %s", w.Body.String())

	var resp map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, "succeeded", resp["status"], "Status should be succeeded")
	assert.NotEmpty(t, resp["subscription_id"], "Should include subscription_id")

	payment, ok := resp["payment"].(map[string]any)
	require.True(t, ok, "payment should be an object")
	assert.Equal(t, "nmi", payment["rail"], "Rail should be nmi")
	assert.NotEmpty(t, payment["transaction_id"], "Should include transaction_id")
}

func TestCheckoutSessionSolanaTransferRequest(t *testing.T) {
	suite, token, _ := setupTestSuiteWithSolana(t)
	recipientWallet := "AKnL4NNf3DGWZJS6cPknBuEGnVsV4A4m5tgebLHaRSZ9"
	suite.seedRailMerchantAccountWithEvidence(
		dbtest.WithTestMerchant(context.Background()),
		"solana",
		config.ExpectedProviderEnvironment(suite.Config.IsTestMode()),
		"DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh",
		`{"source":"test","settings":{"recipient_wallet":"`+recipientWallet+`"}}`,
	)
	products := suite.SeedProducts()
	priceID := products[2].Prices[0].ID

	body := map[string]any{
		"price_id": priceID.String(),
		"payment": map[string]any{
			"rail":         "solana",
			"token_symbol": "USDC",
			"flow":         "transfer_request",
		},
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/checkout", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	suite.Server.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK, got body: %s", w.Body.String())

	var resp map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, "requires_action", resp["status"], "Status should be requires_action")

	payment, ok := resp["payment"].(map[string]any)
	require.True(t, ok, "payment should be an object")
	assert.Equal(t, "solana", payment["rail"], "Rail should be solana")
	assert.NotEmpty(t, payment["reference"], "Should include reference")
	transactionURL, ok := payment["transaction_url"].(string)
	require.True(t, ok, "Should include transaction_url")
	assert.True(t, strings.HasPrefix(transactionURL, "solana:"+recipientWallet+"?"), "transaction_url should use provider-account recipient_wallet")
}

func TestCheckoutSessionSolanaTransferRequestDefaultsRecipientToAccountID(t *testing.T) {
	suite, token, _ := setupTestSuiteWithSolana(t)
	accountID := "DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh"
	suite.seedRailMerchantAccountWithEvidence(
		dbtest.WithTestMerchant(context.Background()),
		"solana",
		config.ExpectedProviderEnvironment(suite.Config.IsTestMode()),
		accountID,
		`{"source":"test"}`,
	)
	products := suite.SeedProducts()
	priceID := products[2].Prices[0].ID

	body := map[string]any{
		"price_id": priceID.String(),
		"payment": map[string]any{
			"rail":         "solana",
			"token_symbol": "USDC",
			"flow":         "transfer_request",
		},
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/checkout", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	suite.Server.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK, got body: %s", w.Body.String())

	var resp map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	payment, ok := resp["payment"].(map[string]any)
	require.True(t, ok, "payment should be an object")
	transactionURL, ok := payment["transaction_url"].(string)
	require.True(t, ok, "Should include transaction_url")
	assert.True(t, strings.HasPrefix(transactionURL, "solana:"+accountID+"?"), "transaction_url should default recipient to provider account_id")
}

func TestCheckoutSessionCCBillRedirect(t *testing.T) {
	suite := getSharedTestSuite(t)
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()
	email := "checkout-session-ccbill-" + t.Name() + "@test.example.com"
	token := getTestIssuer().CreateTokenWithClaims(userID, email, map[string]any{
		"username": "user_" + t.Name(),
	})

	body := map[string]any{
		"price_id": priceID.String(),
		"payment": map[string]any{
			"rail":       "ccbill",
			"email":      email,
			"first_name": "Test",
			"last_name":  "User",
			"address1":   "123 Test St",
			"city":       "Test City",
			"state":      "CA",
			"zip":        "90210",
			"country":    "US",
		},
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/checkout", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	suite.Server.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK, got body: %s", w.Body.String())

	var resp map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, "requires_action", resp["status"], "Status should be requires_action")

	payment, ok := resp["payment"].(map[string]any)
	require.True(t, ok, "payment should be an object")
	assert.Equal(t, "ccbill", payment["rail"], "Rail should be ccbill")
	assert.NotEmpty(t, payment["redirect_url"], "Should include redirect_url")
}

func TestCheckoutSessionMobiusTokenXOR(t *testing.T) {
	suite, _ := SetupSuiteWithMockNMI(t)
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()
	email := "checkout-session-xor-" + t.Name() + "@test.example.com"
	token := getTestIssuer().CreateToken(userID, email)

	body := map[string]any{
		"price_id": priceID.String(),
		"payment": map[string]any{
			"rail":              "nmi",
			"payment_token":     "tok_test_123",
			"payment_method_id": "pm_123",
		},
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/checkout", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	suite.Server.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "Should return 400 for invalid payment fields")
}
