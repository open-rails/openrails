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
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/providerposture"
	"github.com/open-rails/openrails/pkg/merchant"
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

// xs-007 row 36: a confirmation watch ends on the chain's terminal (blockhash
// last valid height) or the caller's context — never on a clock of ours.
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
		c := NewRPCClientWithConfig(RPCClientConfig{Endpoint: url, Network: "devnet"})
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

type staticSecret string

func (s staticSecret) GetSecret(context.Context, merchant.ID, string) (string, error) {
	return string(s), nil
}

// #674: the signature is persisted before any byte is sent, a presubmit failure
// sends nothing, and a landed-but-reverted pull surfaces its Custom code.
func TestBuildSignSubmitPersistsBeforeSendAndSurfacesRevert(t *testing.T) {
	key, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	signer := NewKeypairSigner(staticSecret(key.String()), 0)
	mid := merchant.ID(uuid.New())
	ix := system.NewTransferInstruction(1, key.PublicKey(), solanago.NewWallet().PublicKey()).Build()

	run := func(t *testing.T, onChainErr any, presubmitErr error) (*rpcStub, *solanago.Transaction, []solanago.Signature, solanago.Signature, error) {
		var submitted *solanago.Transaction
		stub, url := newRPCStub(t, func(method string, _ int, params []json.RawMessage) any {
			switch method {
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
		c := NewRPCClientWithConfig(RPCClientConfig{Endpoint: url, Network: "devnet", LoopbackFixture: true})
		var persisted []solanago.Signature
		sig, err := BuildSignSubmitPresubmit(context.Background(), mid, signer, c, []solanago.Instruction{ix}, func(s solanago.Signature) error {
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
		require.True(t, submitted.Message.AccountKeys[0].Equals(key.PublicKey()), "merchant is fee payer")
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

// #346 readonly and provider posture: submission is refused locally unless the
// chain is proven to be a sandbox (or the process is live on mainnet).
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
		c := NewRPCFallbackClient(RPCFallbackConfig{ReadOnly: true, Network: "devnet", CustomEndpoint: url})
		require.ErrorIs(t, send(c), ErrProviderReadOnly)
		require.ErrorIs(t, sendSkip(c), ErrProviderReadOnly)
		require.Zero(t, stub.total())
	})

	t.Run("devnet genesis verified once then submits", func(t *testing.T) {
		stub, url := genesisStub(t, devnetGenesisHash)
		c := NewRPCFallbackClient(RPCFallbackConfig{Network: "devnet", CustomEndpoint: url})
		require.NotErrorIs(t, send(c), providerposture.ErrDisarmed)
		require.NotErrorIs(t, sendSkip(c), providerposture.ErrDisarmed)
		require.Equal(t, 1, stub.count("getGenesisHash"))
		require.Equal(t, 2, stub.count("sendTransaction"))
	})

	for _, genesis := range []string{mainnetGenesisHash, "11111111111111111111111111111111"} {
		t.Run("sandbox config on genesis "+genesis[:6]+" is disarmed", func(t *testing.T) {
			stub, url := genesisStub(t, genesis)
			c := NewRPCFallbackClient(RPCFallbackConfig{Network: "devnet", CustomEndpoint: url})
			require.ErrorIs(t, sendSkip(c), providerposture.ErrDisarmed)
			require.Zero(t, stub.count("sendTransaction"))
		})
	}

	t.Run("mainnet and loopback fixture skip genesis", func(t *testing.T) {
		stub, url := genesisStub(t, mainnetGenesisHash)
		require.NotErrorIs(t, send(NewRPCFallbackClient(RPCFallbackConfig{Network: "mainnet", CustomEndpoint: url})), providerposture.ErrDisarmed)
		require.NotErrorIs(t, send(NewRPCFallbackClient(RPCFallbackConfig{Network: "devnet", CustomEndpoint: url, LoopbackFixture: true})), providerposture.ErrDisarmed)
		require.Zero(t, stub.count("getGenesisHash"))
		require.Equal(t, 2, stub.count("sendTransaction"))
	})

	t.Run("loopback fixture on a non-loopback host is disarmed", func(t *testing.T) {
		c := NewRPCFallbackClient(RPCFallbackConfig{Network: "devnet", CustomEndpoint: "http://rpc.example.com", LoopbackFixture: true})
		require.ErrorIs(t, send(c), providerposture.ErrDisarmed)
	})
}

// #SEC-17: the RPC credential reaches the provider and nothing else — not the
// held endpoint URL, a log line, or an error string.
func TestRPCCredentialReachesOnlyTheProvider(t *testing.T) {
	const secret = "merchant-rpc-secret-key"
	var mu sync.Mutex
	var gotKeys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotKeys = append(gotKeys, r.URL.Query().Get("api-key"))
		mu.Unlock()
		http.Error(w, "upstream is unhappy", http.StatusInternalServerError)
	}))
	defer srv.Close()

	var logs bytes.Buffer
	prevOut, prevLevel := log.StandardLogger().Out, log.GetLevel()
	log.SetOutput(&logs)
	log.SetLevel(log.DebugLevel)
	defer func() { log.SetOutput(prevOut); log.SetLevel(prevLevel) }()

	c := NewRPCFallbackClient(RPCFallbackConfig{CustomEndpoint: srv.URL + "/?api-key=" + secret, Network: "mainnet"})
	_, err := c.GetBalance(context.Background(), solanago.SystemProgramID)
	require.ErrorIs(t, err, ErrAllRPCEndpointsFailed)

	mu.Lock()
	require.NotEmpty(t, gotKeys)
	require.Equal(t, secret, gotKeys[0])
	mu.Unlock()
	for name, s := range map[string]string{"endpoint": c.GetEndpoint(), "error": err.Error(), "logs": logs.String()} {
		require.NotContains(t, s, secret, name)
		require.NotContains(t, s, "api-key=", name)
	}

	sentinel := errors.New("boom ?api-key=leak")
	wrapped := &allEndpointsFailedError{operation: "GetTransaction", err: sentinel}
	require.ErrorIs(t, wrapped, sentinel, "error identity survives redaction")
	require.ErrorIs(t, wrapped, ErrAllRPCEndpointsFailed)
	require.NotContains(t, wrapped.Error(), "leak")
}

func TestRPCProviderEndpointSelection(t *testing.T) {
	for _, eps := range [][]RPCEndpoint{DefaultMainnetEndpoints("helius-secret"), DefaultDevnetEndpoints("helius-secret")} {
		for _, ep := range eps {
			require.NotContains(t, ep.URL, "helius-secret")
			require.NotContains(t, ep.URL, "api-key=")
		}
		require.Equal(t, "helius-secret", eps[0].secret.Get("api-key"))
	}

	helius := NewRPCFallbackClient(RPCFallbackConfig{Network: "devnet", RPCProvider: "helius", RPCAPIKey: "rpc-key"})
	require.GreaterOrEqual(t, len(helius.endpoints), 2, "helius plus public fallback")
	require.Equal(t, "https://devnet.helius-rpc.com/", helius.endpoints[0].URL)
	require.Equal(t, "rpc-key", helius.endpoints[0].secret.Get("api-key"))
	require.NotEmpty(t, helius.PrimaryCredentialFingerprint())

	public := NewRPCFallbackClient(RPCFallbackConfig{Network: "mainnet", RPCProvider: "public", RPCAPIKey: "ignored"})
	require.Len(t, public.endpoints, 1)
	require.Equal(t, "https://api.mainnet-beta.solana.com", public.endpoints[0].URL)
	require.Empty(t, public.PrimaryCredentialFingerprint())
}
