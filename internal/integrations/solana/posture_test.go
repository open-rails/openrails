package solana

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/providerposture"
)

func genesisServer(t *testing.T, genesis string, genesisReads, sends *atomic.Int64) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		switch req.Method {
		case "getGenesisHash":
			genesisReads.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": genesis})
		default:
			sends.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": "fake rejects"}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDevnetChainVerifiedOnceBeforeSubmission(t *testing.T) {
	var reads, sends atomic.Int64
	srv := genesisServer(t, devnetGenesisHash, &reads, &sends)
	c := NewRPCFallbackClient(RPCFallbackConfig{Network: "devnet", CustomEndpoint: srv.URL})
	for i := 0; i < 2; i++ {
		_, err := c.SendTransaction(context.Background(), &solanago.Transaction{})
		require.NotErrorIs(t, err, providerposture.ErrDisarmed)
	}
	require.EqualValues(t, 1, reads.Load())
	require.EqualValues(t, 2, sends.Load())
}

func TestSandboxChainOnMainnetOrUnknownGenesisIsDisarmed(t *testing.T) {
	for _, genesis := range []string{mainnetGenesisHash, "11111111111111111111111111111111"} {
		var reads, sends atomic.Int64
		srv := genesisServer(t, genesis, &reads, &sends)
		c := NewRPCFallbackClient(RPCFallbackConfig{Network: "devnet", CustomEndpoint: srv.URL})
		_, err := c.SendTransactionSkipPreflight(context.Background(), &solanago.Transaction{})
		require.ErrorIs(t, err, providerposture.ErrDisarmed)
		require.EqualValues(t, 0, sends.Load())
	}
}

func TestMainnetPostureAndLoopbackFixtureSkipVerification(t *testing.T) {
	var reads, sends atomic.Int64
	srv := genesisServer(t, mainnetGenesisHash, &reads, &sends)
	live := NewRPCFallbackClient(RPCFallbackConfig{Network: "mainnet", CustomEndpoint: srv.URL})
	_, _ = live.SendTransaction(context.Background(), &solanago.Transaction{})
	fixture := NewRPCFallbackClient(RPCFallbackConfig{Network: "devnet", CustomEndpoint: srv.URL, LoopbackFixture: true})
	_, err := fixture.SendTransaction(context.Background(), &solanago.Transaction{})
	require.NotErrorIs(t, err, providerposture.ErrDisarmed)
	require.EqualValues(t, 0, reads.Load())
	require.EqualValues(t, 2, sends.Load())
}
