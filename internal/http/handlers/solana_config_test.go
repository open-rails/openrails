package handlers

import (
	"encoding/json"
	"github.com/open-rails/openrails/internal/railresolve"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"

	"context"
	solanago "github.com/gagliardetto/solana-go"
)

const testDevUSDCMint = "5CVTPbcqPuzQd9bMCViire6zQVSr7TUTWTjM21aE4TZ"

// fakeMintReader serves a synthetic, initialized SPL mint account at `decimals`
// for every address — decimals now come from the chain (#817), so a handler test
// must arm a reader or the token is (correctly) dropped as unpriceable.
type fakeMintReader struct{ decimals uint8 }

func (r fakeMintReader) GetAccountData(context.Context, solanago.PublicKey) ([]byte, error) {
	blob := make([]byte, solanaint.MintAccountSize)
	blob[44] = r.decimals
	blob[45] = 1
	return blob, nil
}

func TestGetSolanaConfig(t *testing.T) {
	t.Run("default disables recurring Solana Pay", func(t *testing.T) {
		runtime := &app.Runtime{
			Config:             &config.Config{},
			SolanaMintDecimals: solanamodule.NewMintDecimals(fakeMintReader{decimals: 6}),
			RailConfigs: railresolve.FixedSet{
				"solana": {
					Rail: models.RailSolana,
					Solana: &config.SolanaRailConfig{
						Network: "devnet",
						Tokens: map[string]config.TokenConfig{
							"USDC": {
								Name: "Dev USDC",
								Mint: "5CVTPbcqPuzQd9bMCViire6zQVSr7TUTWTjM21aE4TZ",
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
		require.True(t, response.Features.SolanaPayRecurringSubscriptions)
	})

	runtime := &app.Runtime{
		Config:             &config.Config{},
		SolanaMintDecimals: solanamodule.NewMintDecimals(fakeMintReader{decimals: 6}),
		RailConfigs: railresolve.FixedSet{
			"solana": {
				Rail: models.RailSolana,
				Solana: &config.SolanaRailConfig{
					Network: "devnet",
					Tokens: map[string]config.TokenConfig{
						"USDC": {
							Name: "Dev USDC",
							Mint: "5CVTPbcqPuzQd9bMCViire6zQVSr7TUTWTjM21aE4TZ",
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
	require.Equal(t, testDevUSDCMint, response.Tokens[0].Mint)
	// #817: decimals are the MINT's on-chain value, not a config knob.
	require.Equal(t, 6, response.Tokens[0].Decimals)
	require.True(t, response.Tokens[0].Preferred)
	require.True(t, response.Tokens[0].RecurringEligible)
}
