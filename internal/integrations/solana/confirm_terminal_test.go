package solana

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/stretchr/testify/require"
)

// xs-007 row 36: the confirmation watch ends on the chain's terminal — the
// blockhash's last valid block height — or the caller's context. Never a
// clock of this package.

// stubChain is a JSON-RPC server that answers getSignatureStatuses and
// getBlockHeight from scripted state.
type stubChain struct {
	mu          sync.Mutex
	height      uint64
	landAtPoll  int // the poll number on which the signature appears (0 = never)
	statusPolls int
}

func (c *stubChain) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     any    `json:"id"`
		Method string `json:"method"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	c.mu.Lock()
	defer c.mu.Unlock()
	var result any
	switch req.Method {
	case "getBlockHeight":
		c.height++
		result = c.height
	case "getSignatureStatuses":
		c.statusPolls++
		var value []any
		if c.landAtPoll > 0 && c.statusPolls >= c.landAtPoll {
			value = []any{map[string]any{"slot": 100, "confirmations": 5, "confirmationStatus": "confirmed", "err": nil}}
		} else {
			value = []any{nil}
		}
		result = map[string]any{"context": map[string]any{"slot": 100}, "value": value}
	default:
		http.Error(w, "unexpected method "+req.Method, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func newStubChainClient(t *testing.T, chain *stubChain) *RPCClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(chain.serve))
	t.Cleanup(srv.Close)
	prev := watchPollInterval
	watchPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { watchPollInterval = prev })
	return NewRPCClientWithConfig(RPCClientConfig{Endpoint: srv.URL, Network: "devnet"})
}

func TestWatchTransaction_LateLandingIsNotAFailure(t *testing.T) {
	// The signature stays unseen for many polls — far past what a 90 s clock
	// would have tolerated at this cadence — and then lands. The chain still
	// says the blockhash is valid (height stays below the terminal).
	chain := &stubChain{height: 1000, landAtPoll: 12}
	client := newStubChainClient(t, chain)
	sig := solanago.Signature{1}

	outcome, err := client.WatchTransaction(context.Background(), sig, rpc.CommitmentConfirmed, ChainTerminal{LastValidBlockHeight: 1_000_000})
	require.NoError(t, err)
	require.True(t, outcome.Succeeded())
	require.Equal(t, rpc.ConfirmationStatusConfirmed, outcome.Status)
}

func TestWatchTransaction_ChainTerminalEndsTheWatch(t *testing.T) {
	// Never lands; the block height marches past the blockhash's last valid
	// height. That — the chain's own word — is the only "failed to confirm".
	chain := &stubChain{height: 1000}
	client := newStubChainClient(t, chain)
	sig := solanago.Signature{2}

	_, err := client.WatchTransaction(context.Background(), sig, rpc.CommitmentConfirmed, ChainTerminal{LastValidBlockHeight: 1005})
	require.ErrorIs(t, err, ErrTransactionExpired)
	chain.mu.Lock()
	defer chain.mu.Unlock()
	require.GreaterOrEqual(t, chain.statusPolls, 5, "the signature was looked for on every tick before the verdict")
}

func TestWatchTransaction_UnknownTerminalWatchesUntilTheCaller(t *testing.T) {
	// A wallet-built signature carries no known blockhash: nothing but the
	// caller's own context ends the watch.
	chain := &stubChain{height: 1000}
	client := newStubChainClient(t, chain)
	sig := solanago.Signature{3}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	_, err := client.WatchTransaction(ctx, sig, rpc.CommitmentConfirmed, ChainTerminal{})
	require.ErrorIs(t, err, context.Canceled)
	chain.mu.Lock()
	defer chain.mu.Unlock()
	require.GreaterOrEqual(t, chain.statusPolls, 5)
	require.EqualValues(t, 1000, chain.height, "no terminal known: the block height was never consulted")
}
