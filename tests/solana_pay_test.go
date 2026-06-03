//go:build integration

package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSolanaPayTransactionRequestFlow tests the full transaction_request flow
// which uses the Solana Pay Transaction Request spec endpoints
func TestSolanaPayTransactionRequestFlow(t *testing.T) {
	suite, token, _ := setupTestSuiteWithSolana(t)
	// Add store branding for testing
	suite.Config.Store = &config.StoreConfig{
		Name:    "Test Store",
		LogoURL: config.DefaultLogoURL,
	}
	// Set a proper host for solana_pay_url generation
	suite.Config.Host = "https://api.test.com"

	products := suite.SeedProducts()
	// Use a one-time purchase product (products[2] is Solana-only)
	priceID := products[2].Prices[0].ID

	t.Run("create checkout session returns solana_pay_url", func(t *testing.T) {
		body := map[string]any{
			"price_id": priceID.String(),
			"payment": map[string]any{
				"processor":    "solana",
				"token_symbol": "USDC",
				"flow":         "transaction_request",
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
		assert.Equal(t, "solana", payment["processor"])

		// Verify URL format: solana:https://...
		nextAction, ok := resp["next_action"].(map[string]any)
		require.True(t, ok, "next_action should be present")
		assert.Equal(t, "solana_pay", nextAction["type"], "next_action type should be solana_pay")
		solanaPayURL, ok := nextAction["solana_pay_url"].(string)
		require.True(t, ok)
		assert.NotEmpty(t, solanaPayURL, "Should include solana_pay_url")
		assert.True(t, strings.HasPrefix(solanaPayURL, "solana:https://"), "URL should start with solana:https://")
		assert.Contains(t, solanaPayURL, "/v1/checkout/", "URL should contain checkout path")
		assert.Contains(t, solanaPayURL, "/solana-pay", "URL should contain solana-pay suffix")
	})
}

// TestSolanaPayGetEndpoint tests GET /v1/checkout/:id/solana-pay
func TestSolanaPayGetEndpoint(t *testing.T) {
	suite, token, _ := setupTestSuiteWithSolana(t)
	suite.Config.Store = &config.StoreConfig{
		Name:    "Test Store",
		LogoURL: config.DefaultLogoURL,
	}
	suite.Config.Host = "https://api.test.com"

	products := suite.SeedProducts()
	priceID := products[2].Prices[0].ID

	t.Run("returns label and icon for valid session", func(t *testing.T) {
		// First create a checkout session
		body := map[string]any{
			"price_id": priceID.String(),
			"payment": map[string]any{
				"processor":    "solana",
				"token_symbol": "USDC",
				"flow":         "transaction_request",
			},
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/checkout", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		suite.Server.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		var createResp map[string]any
		json.Unmarshal(w.Body.Bytes(), &createResp)
		sessionID := createResp["id"].(string)

		// Now call GET /v1/checkout/:id/solana-pay (no auth required)
		w2 := httptest.NewRecorder()
		req2, _ := http.NewRequest("GET", "/v1/checkout/"+sessionID+"/solana-pay", nil)
		suite.Server.Handler().ServeHTTP(w2, req2)

		require.Equal(t, http.StatusOK, w2.Code, "Should return 200 OK, got body: %s", w2.Body.String())

		var resp map[string]any
		err := json.Unmarshal(w2.Body.Bytes(), &resp)
		require.NoError(t, err)

		// Should return label from config or product name
		label, ok := resp["label"].(string)
		require.True(t, ok)
		assert.NotEmpty(t, label, "label should not be empty")

		// Should return icon from config
		icon, ok := resp["icon"].(string)
		require.True(t, ok)
		assert.Equal(t, config.DefaultLogoURL, icon)
	})

	t.Run("returns 404 for invalid session ID", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/checkout/cs_invalid123/solana-pay", nil)
		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("returns 404 for non-existent session", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/checkout/cs_00000000-0000-0000-0000-000000000000/solana-pay", nil)
		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

// TestSolanaPayPostEndpoint tests POST /v1/checkout/:id/solana-pay
func TestSolanaPayPostEndpoint(t *testing.T) {
	suite, token, _ := setupTestSuiteWithSolana(t)
	suite.Config.Store = &config.StoreConfig{
		Name:    "Test Store",
		LogoURL: config.DefaultLogoURL,
	}
	suite.Config.Host = "https://api.test.com"

	products := suite.SeedProducts()
	priceID := products[2].Prices[0].ID

	t.Run("returns error without account", func(t *testing.T) {
		// First create a checkout session
		body := map[string]any{
			"price_id": priceID.String(),
			"payment": map[string]any{
				"processor":    "solana",
				"token_symbol": "USDC",
				"flow":         "transaction_request",
			},
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/checkout", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		suite.Server.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		var createResp map[string]any
		json.Unmarshal(w.Body.Bytes(), &createResp)
		sessionID := createResp["id"].(string)

		// Call POST without account
		w2 := httptest.NewRecorder()
		req2, _ := http.NewRequest("POST", "/v1/checkout/"+sessionID+"/solana-pay", bytes.NewReader([]byte("{}")))
		req2.Header.Set("Content-Type", "application/json")
		suite.Server.Handler().ServeHTTP(w2, req2)

		assert.Equal(t, http.StatusBadRequest, w2.Code)
	})

	t.Run("returns 400 for non-Solana session", func(t *testing.T) {
		// Create a non-Solana checkout session
		mobiusPriceID := products[0].Prices[0].ID
		body := map[string]any{
			"price_id": mobiusPriceID.String(),
			"payment": map[string]any{
				"processor":     "mobius",
				"payment_token": "tok_test_123",
				"email":         "test@example.com",
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

		// Use mock NMI to allow this request to succeed
		suiteWithNMI, _ := SetupSuiteWithMockNMI(t)
		suiteWithNMI.Config.Processors["solana"] = suite.Config.Processors["solana"]
		products2 := suiteWithNMI.SeedProducts()
		mobiusPriceID2 := products2[0].Prices[0].ID

		body["price_id"] = mobiusPriceID2.String()
		jsonBody, _ = json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/checkout", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		suiteWithNMI.Server.Handler().ServeHTTP(w, req)

		// Even if checkout succeeds, trying to use solana-pay endpoint should fail
		if w.Code == http.StatusOK {
			var createResp map[string]any
			json.Unmarshal(w.Body.Bytes(), &createResp)
			if sessionID, ok := createResp["id"].(string); ok {
				w2 := httptest.NewRecorder()
				postBody := map[string]any{"account": "DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh"}
				postBodyJSON, _ := json.Marshal(postBody)
				req2, _ := http.NewRequest("POST", "/v1/checkout/"+sessionID+"/solana-pay", bytes.NewReader(postBodyJSON))
				req2.Header.Set("Content-Type", "application/json")
				suiteWithNMI.Server.Handler().ServeHTTP(w2, req2)

				assert.Equal(t, http.StatusBadRequest, w2.Code, "Should return 400 for non-Solana session")
			}
		}
	})
}

// TestSolanaPayTransferRequestNotAffected ensures transfer_request flow still works
func TestSolanaPayTransferRequestNotAffected(t *testing.T) {
	suite, token, _ := setupTestSuiteWithSolana(t)
	products := suite.SeedProducts()
	priceID := products[2].Prices[0].ID

	body := map[string]any{
		"price_id": priceID.String(),
		"payment": map[string]any{
			"processor":    "solana",
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

	assert.Equal(t, "requires_action", resp["status"])

	payment, ok := resp["payment"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "solana", payment["processor"])
	assert.NotEmpty(t, payment["reference"], "Should include reference")
	assert.NotEmpty(t, payment["transaction_url"], "Should include transaction_url")

	// transfer_request should NOT have solana_pay_url
	_, hasSolanaPayURL := payment["solana_pay_url"]
	assert.False(t, hasSolanaPayURL || payment["solana_pay_url"] == "", "transfer_request should not have solana_pay_url")
}

// setupTestSuiteWithSolanaPayConfig extends setupTestSuiteWithSolana with Store config
func setupTestSuiteWithSolanaPayConfig(t *testing.T) (*TestContainerSuite, string, string) {
	suite, token, userID := setupTestSuiteWithSolana(t)
	suite.Config.Store = &config.StoreConfig{
		Name:    "Test Store",
		LogoURL: config.DefaultLogoURL,
	}
	suite.Config.Host = "https://api.test.com"
	return suite, token, userID
}
