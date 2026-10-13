package solana

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/rpc"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/providerposture"
)

// rpcStub is a scripted Solana JSON-RPC node. answer returns a result value,
// or an rpcFault to reply with a JSON-RPC error.
type rpcStub struct {
	mu     sync.Mutex
	calls  map[string]int
	order  []string
	answer func(method string, n int, params []json.RawMessage) any
}

type rpcFault struct {
	Code    int
	Message string
}

func newRPCStub(t *testing.T, answer func(method string, n int, params []json.RawMessage) any) (*rpcStub, string) {
	t.Helper()
	s := &rpcStub{calls: map[string]int{}, answer: answer}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	return s, srv.URL
}

func (s *rpcStub) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.calls[req.Method]++
	s.order = append(s.order, req.Method)
	n := s.calls[req.Method]
	s.mu.Unlock()
	out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	switch v := s.answer(req.Method, n, req.Params).(type) {
	case rpcFault:
		out["error"] = map[string]any{"code": v.Code, "message": v.Message}
	default:
		out["result"] = v
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *rpcStub) count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[method]
}

func (s *rpcStub) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.order)
}

func signatureStatus(status string, onChainErr any) map[string]any {
	return map[string]any{"context": map[string]any{"slot": 100}, "value": []any{
		map[string]any{"slot": 100, "confirmations": 5, "confirmationStatus": status, "err": onChainErr},
	}}
}

func unseenStatus() map[string]any {
	return map[string]any{"context": map[string]any{"slot": 100}, "value": []any{nil}}
}

func fastWatch(t *testing.T) {
	t.Helper()
	prev := watchPollInterval
	watchPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { watchPollInterval = prev })
}

// A confirmation watch ends on the chain's terminal (blockhash last valid
// height) or the caller's context, never on a clock of ours.
func TestWatchTransactionEndsOnlyOnChainTerminalOrCaller(t *testing.T) {
	fastWatch(t)
	watch := func(t *testing.T, landAt int, terminal ChainTerminal, ctx context.Context) (*rpcStub, *TransactionOutcome, error) {
		height := uint64(1000)
		stub, url := newRPCStub(t, func(method string, n int, _ []json.RawMessage) any {
			switch method {
			case "getBlockHeight":
				height++
				return height
			case "getSignatureStatuses":
				if landAt > 0 && n >= landAt {
					return signatureStatus("confirmed", nil)
				}
				return unseenStatus()
			}
			return rpcFault{Code: -32601, Message: "unexpected " + method}
		})
		c := NewRPCClientWithConfig(RPCClientConfig{PublicURL: url, Network: "devnet"})
		out, err := c.WatchTransaction(ctx, solanago.Signature{1}, rpc.CommitmentConfirmed, terminal)
		return stub, out, err
	}

	t.Run("late landing within a valid blockhash is success", func(t *testing.T) {
		_, out, err := watch(t, 12, ChainTerminal{LastValidBlockHeight: 1_000_000}, context.Background())
		require.NoError(t, err)
		require.True(t, out.Succeeded())
		require.Equal(t, rpc.ConfirmationStatusConfirmed, out.Status)
	})

	t.Run("block height past the terminal expires it", func(t *testing.T) {
		stub, _, err := watch(t, 0, ChainTerminal{LastValidBlockHeight: 1005}, context.Background())
		require.ErrorIs(t, err, ErrTransactionExpired)
		require.GreaterOrEqual(t, stub.count("getSignatureStatuses"), 5, "status is read before every height verdict")
	})

	t.Run("unknown terminal watches until the caller cancels", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		stub, _, err := watch(t, 0, ChainTerminal{}, ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Zero(t, stub.count("getBlockHeight"), "no terminal: the height is never consulted")
	})
}

// The signature is persisted before any byte is sent, a presubmit failure
// sends nothing, and a landed-but-reverted pull surfaces its Custom code.
func TestBuildSignSubmitPersistsBeforeSendAndSurfacesRevert(t *testing.T) {
	key, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	signer := NewKeypairSigner(key)
	ix := system.NewTransferInstruction(1, key.PublicKey(), solanago.NewWallet().PublicKey()).Build()

	run := func(t *testing.T, onChainErr any, presubmitErr error) (*rpcStub, *solanago.Transaction, []solanago.Signature, solanago.Signature, error) {
		var submitted *solanago.Transaction
		stub, url := newRPCStub(t, func(method string, _ int, params []json.RawMessage) any {
			switch method {
			case "getGenesisHash":
				return devnetGenesisHash
			case "getLatestBlockhash":
				return map[string]any{"context": map[string]any{"slot": 1}, "value": map[string]any{
					"blockhash": solanago.Hash{9}.String(), "lastValidBlockHeight": 5000,
				}}
			case "sendTransaction":
				var b64 string
				if err := json.Unmarshal(params[0], &b64); err != nil {
					return rpcFault{Code: -32602, Message: err.Error()}
				}
				raw, err := base64.StdEncoding.DecodeString(b64)
				if err != nil {
					return rpcFault{Code: -32602, Message: err.Error()}
				}
				tx, err := solanago.TransactionFromBytes(raw)
				if err != nil {
					return rpcFault{Code: -32602, Message: err.Error()}
				}
				submitted = tx
				return tx.Signatures[0].String()
			case "getSignatureStatuses":
				return signatureStatus("confirmed", onChainErr)
			}
			return rpcFault{Code: -32601, Message: "unexpected " + method}
		})
		c := NewRPCClientWithConfig(RPCClientConfig{PublicURL: url, Network: "devnet"})
		var persisted []solanago.Signature
		sig, err := BuildSignSubmitPresubmit(context.Background(), signer, c, []solanago.Instruction{ix}, func(s solanago.Signature) error {
			require.Zero(t, stub.count("sendTransaction"), "persisted before submit")
			persisted = append(persisted, s)
			return presubmitErr
		})
		return stub, submitted, persisted, sig, err
	}

	t.Run("landed", func(t *testing.T) {
		_, submitted, persisted, sig, err := run(t, nil, nil)
		require.NoError(t, err)
		require.Equal(t, []solanago.Signature{sig}, persisted)
		require.NotNil(t, submitted)
		require.True(t, submitted.Message.AccountKeys[0].Equals(key.PublicKey()), "the cranker is fee payer")
		require.Equal(t, solanago.Hash{9}, submitted.Message.RecentBlockhash)
		require.NoError(t, submitted.VerifySignatures())
	})

	t.Run("presubmit failure sends nothing", func(t *testing.T) {
		stub, _, _, _, err := run(t, nil, errors.New("db down"))
		require.ErrorContains(t, err, "NOT sent")
		require.Zero(t, stub.count("sendTransaction"))
	})

	t.Run("reverted on chain", func(t *testing.T) {
		revert := map[string]any{"InstructionError": []any{0, map[string]any{"Custom": 4}}}
		_, _, _, _, err := run(t, revert, nil)
		require.ErrorContains(t, err, `"Custom":4`)
	})
}

// Readonly and provider posture: submission is refused locally until every
// endpoint in the chain is proven on the PSP's network, mainnet included.
func TestSubmissionGatedByReadOnlyAndChainPosture(t *testing.T) {
	genesisStub := func(t *testing.T, genesis string) (*rpcStub, string) {
		return newRPCStub(t, func(method string, _ int, _ []json.RawMessage) any {
			if method == "getGenesisHash" {
				return genesis
			}
			return rpcFault{Code: -32000, Message: "stub rejects " + method}
		})
	}
	send := func(c *RPCFallbackClient) error {
		_, err := c.SendTransaction(context.Background(), &solanago.Transaction{})
		return err
	}
	sendSkip := func(c *RPCFallbackClient) error {
		_, err := c.SendTransactionSkipPreflight(context.Background(), &solanago.Transaction{})
		return err
	}

	t.Run("readonly blocks both submit paths before any request", func(t *testing.T) {
		stub, url := genesisStub(t, devnetGenesisHash)
		c := NewRPCFallbackClient(RPCFallbackConfig{ReadOnly: true, Network: "devnet", PublicURL: url})
		require.ErrorIs(t, send(c), ErrProviderReadOnly)
		require.ErrorIs(t, sendSkip(c), ErrProviderReadOnly)
		require.Zero(t, stub.total())
	})

	for network, genesis := range map[string]string{"devnet": devnetGenesisHash, "testnet": testnetGenesisHash, "mainnet": mainnetGenesisHash} {
		t.Run(network+" genesis verified once then submits", func(t *testing.T) {
			stub, url := genesisStub(t, genesis)
			c := NewRPCFallbackClient(RPCFallbackConfig{Network: network, PublicURL: url})
			require.NotErrorIs(t, send(c), providerposture.ErrDisarmed)
			require.NotErrorIs(t, sendSkip(c), providerposture.ErrDisarmed)
			require.Equal(t, 1, stub.count("getGenesisHash"))
			require.Equal(t, 2, stub.count("sendTransaction"))
		})
	}

	for name, row := range map[string]struct{ network, genesis, want string }{
		"mainnet endpoint on a devnet PSP": {"devnet", mainnetGenesisHash, "is on mainnet; the PSP declares devnet"},
		"devnet endpoint on a mainnet PSP": {"mainnet", devnetGenesisHash, "is on devnet; the PSP declares mainnet"},
		"testnet endpoint on a devnet PSP": {"devnet", testnetGenesisHash, "is on testnet; the PSP declares devnet"},
		"unknown cluster on a devnet PSP":  {"devnet", "11111111111111111111111111111111", "unrecognized genesis hash"},
		"unknown cluster on a mainnet PSP": {"mainnet", "11111111111111111111111111111111", "unrecognized genesis hash"},
	} {
		t.Run(name+" is disarmed", func(t *testing.T) {
			stub, url := genesisStub(t, row.genesis)
			c := NewRPCFallbackClient(RPCFallbackConfig{Network: row.network, PublicURL: url})
			err := sendSkip(c)
			require.ErrorIs(t, err, providerposture.ErrDisarmed)
			require.ErrorContains(t, err, row.want)
			require.Zero(t, stub.count("sendTransaction"))
		})
	}

	t.Run("every endpoint is checked: an rpc_url on another network disarms", func(t *testing.T) {
		primary, primaryURL := genesisStub(t, mainnetGenesisHash)
		public, publicURL := genesisStub(t, devnetGenesisHash)
		c := NewRPCFallbackClient(RPCFallbackConfig{Network: "devnet", URL: primaryURL + "/secret-token", PublicURL: publicURL})
		err := send(c)
		require.ErrorIs(t, err, providerposture.ErrDisarmed)
		require.ErrorContains(t, err, "solana endpoint rpc_url is on mainnet; the PSP declares devnet")
		require.NotContains(t, err.Error(), "secret-token")
		require.Zero(t, primary.count("sendTransaction")+public.count("sendTransaction"))
	})
}

// The rpc_url reaches its provider and nothing else: a key in its query or
// its path is not in the held endpoint, a log line, or an error string.
func TestRPCCredentialReachesOnlyTheProvider(t *testing.T) {
	const secret = "merchant-rpc-secret-key"
	for name, rpcURL := range map[string]string{"query": "/?api-key=" + secret, "path": "/v2/" + secret} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var got []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				got = append(got, r.URL.String())
				mu.Unlock()
				http.Error(w, "upstream is unhappy "+r.URL.String(), http.StatusInternalServerError)
			}))
			defer srv.Close()
			_, public := newRPCStub(t, func(method string, _ int, _ []json.RawMessage) any {
				return rpcFault{Code: -32000, Message: "public rejects " + method}
			})

			var logs bytes.Buffer
			prevOut, prevLevel := log.StandardLogger().Out, log.GetLevel()
			log.SetOutput(&logs)
			log.SetLevel(log.DebugLevel)
			defer func() { log.SetOutput(prevOut); log.SetLevel(prevLevel) }()

			c := NewRPCFallbackClient(RPCFallbackConfig{URL: srv.URL + rpcURL, PublicURL: public, Network: "mainnet"})
			_, err := c.GetBalance(context.Background(), solanago.SystemProgramID)
			require.ErrorIs(t, err, ErrAllRPCEndpointsFailed)

			mu.Lock()
			require.NotEmpty(t, got)
			require.Contains(t, got[0], secret, "the provider receives its key")
			mu.Unlock()
			for name, s := range map[string]string{"endpoint": c.GetEndpoint(), "error": err.Error(), "logs": logs.String(), "posture": c.PostureKey().Endpoint} {
				require.NotContains(t, s, secret, name)
			}
		})
	}

	sentinel := errors.New("boom ?api-key=leak")
	wrapped := &allEndpointsFailedError{operation: "GetTransaction", err: sentinel}
	require.ErrorIs(t, wrapped, sentinel, "error identity survives redaction")
	require.ErrorIs(t, wrapped, ErrAllRPCEndpointsFailed)
	require.NotContains(t, wrapped.Error(), "leak")
}

// rpc_url is the primary endpoint and the network's public one its fallback;
// without one, the public endpoint alone.
func TestRPCEndpointChain(t *testing.T) {
	helius := NewRPCFallbackClient(RPCFallbackConfig{Network: "devnet", URL: "https://devnet.helius-rpc.com/?api-key=rpc-key"})
	require.Len(t, helius.endpoints, 2)
	require.Equal(t, "https://devnet.helius-rpc.com", helius.endpoints[0].URL)
	require.Equal(t, "https://devnet.helius-rpc.com/?api-key=rpc-key", helius.endpoints[0].target.String())
	require.Equal(t, "https://api.devnet.solana.com", helius.endpoints[1].URL)
	require.NotEmpty(t, helius.PrimaryCredentialFingerprint())

	for network, public := range map[string]string{"mainnet": "https://api.mainnet-beta.solana.com", "devnet": "https://api.devnet.solana.com", "testnet": "https://api.testnet.solana.com"} {
		c := NewRPCFallbackClient(RPCFallbackConfig{Network: network})
		require.Len(t, c.endpoints, 1)
		require.Equal(t, public, c.endpoints[0].URL)
		require.Empty(t, c.PrimaryCredentialFingerprint())
	}

	// A failing rpc_url falls back to the public endpoint.
	var primaryCalls int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer primary.Close()
	_, public := newRPCStub(t, func(method string, _ int, _ []json.RawMessage) any {
		if method == "getBalance" {
			return map[string]any{"context": map[string]any{"slot": 1}, "value": 42}
		}
		return rpcFault{Code: -32601, Message: method}
	})
	c := NewRPCFallbackClient(RPCFallbackConfig{Network: "devnet", URL: primary.URL + "/k", PublicURL: public})
	balance, err := c.GetBalance(context.Background(), solanago.SystemProgramID)
	require.NoError(t, err)
	require.Equal(t, uint64(42), balance)
	require.Equal(t, 1, primaryCalls)
}
