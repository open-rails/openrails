//go:build integration

package tests

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/doujins-org/doujins-billing/config"
)

// TestSolanaTokensNoAuth tests that /v1/solana/tokens doesn't require auth
func TestSolanaTokensNoAuth(t *testing.T) {
	suite := setupTestSuite(t)

	t.Run("returns supported tokens without auth", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/solana/tokens", nil)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK, got: %s", w.Body.String())

		var response struct {
			Tokens []struct {
				Symbol   string  `json:"symbol"`
				Name     string  `json:"name"`
				Mint     string  `json:"mint"`
				Decimals int     `json:"decimals"`
				Price    float64 `json:"price"`
			} `json:"tokens"`
		}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		// Should have at least SOL and USDC
		assert.GreaterOrEqual(t, len(response.Tokens), 2, "Should have at least 2 tokens")

		// Find SOL token
		var foundSOL bool
		for _, token := range response.Tokens {
			if token.Symbol == "SOL" {
				foundSOL = true
				assert.Equal(t, "Solana", token.Name)
				assert.Equal(t, 9, token.Decimals)
				assert.NotEmpty(t, token.Mint)
			}
		}
		assert.True(t, foundSOL, "Should have SOL token")
	})
}

// setupTestSuiteWithSolana configures the test suite with Solana config
func setupTestSuiteWithSolana(t *testing.T) (*TestContainerSuite, string, string) {
	suite, token, userID := setupTestSuiteWithAuth(t)

	// Add Solana configuration to the Processors map
	if suite.Config.Processors == nil {
		suite.Config.Processors = make(map[string]*config.ProcessorConfig)
	}
	suite.Config.Processors["solana"] = &config.ProcessorConfig{
		Type:            config.ProcessorTypeSolana,
		RecipientWallet: "DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh",
		SupportedTokens: config.DefaultDevnetTokens(),
		// RPCEndpoint and Network are derived from test_mode
	}

	return suite, token, userID
}
