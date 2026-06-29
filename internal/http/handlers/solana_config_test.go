package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
)

func TestGetSolanaConfig(t *testing.T) {
	t.Run("default disables recurring Solana Pay", func(t *testing.T) {
		runtime := &app.Runtime{
			Config: &config.Config{},
			Rails: config.ProviderAccountSet{
				"solana": {
					Rail: models.RailSolana,
					Solana: &config.SolanaRailConfig{
						Network: "devnet",
						Tokens: map[string]config.TokenConfig{
							"USDC": {
								Name:     "Dev USDC",
								Mint:     "5CVTPbcqPuzQd9bMCViire6zQVSr7TUTWTjM21aE4TZ",
								Decimals: 6,
							},
						},
					},
				},
			},
		}
		recorder := httptest.NewRecorder()
		req := httprequest.NewHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/solana/config", nil), runtime)

		GetSolanaConfig(req)

		require.Equal(t, http.StatusOK, recorder.Code)
		var response SolanaRuntimeConfigResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
		require.False(t, response.Features.SolanaPayRecurringSubscriptions)
	})

	runtime := &app.Runtime{
		Config: &config.Config{},
		Rails: config.ProviderAccountSet{
			"solana": {
				Rail: models.RailSolana,
				Solana: &config.SolanaRailConfig{
					Network:                         "devnet",
					SolanaPayRecurringSubscriptions: true,
					Tokens: map[string]config.TokenConfig{
						"USDC": {
							Name:     "Dev USDC",
							Mint:     "5CVTPbcqPuzQd9bMCViire6zQVSr7TUTWTjM21aE4TZ",
							Decimals: 6,
						},
					},
				},
			},
		},
	}
	recorder := httptest.NewRecorder()
	req := httprequest.NewHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/solana/config", nil), runtime)

	GetSolanaConfig(req)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response SolanaRuntimeConfigResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Equal(t, "devnet", response.Network)
	require.Equal(t, "solana:devnet", response.Chain)
	// RPCURL is always empty (#352): no rpc_endpoint knob, and the Helius key must never reach a browser.
	require.Equal(t, "", response.RPCURL)
	require.Equal(t, "devnet", response.ExplorerCluster)
	require.Equal(t, solanatokens.PreferredStablecoin, response.PreferredToken)
	require.True(t, response.Features.SolanaPay)
	require.True(t, response.Features.RecurringSubscriptions)
	require.True(t, response.Features.SolanaPayRecurringSubscriptions)
	require.Len(t, response.Tokens, 1)
	require.Equal(t, "USDC", response.Tokens[0].Symbol)
	require.Equal(t, "5CVTPbcqPuzQd9bMCViire6zQVSr7TUTWTjM21aE4TZ", response.Tokens[0].Mint)
	require.True(t, response.Tokens[0].Preferred)
	require.True(t, response.Tokens[0].RecurringEligible)
}
