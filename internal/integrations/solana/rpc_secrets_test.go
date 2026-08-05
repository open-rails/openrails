package solana

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

// #SEC-17: the merchant's RPC credential must reach the PROVIDER and nothing
// else — not a log line, not an error string, not an HTTP response body. Every
// endpoint failure path (timeout/429/5xx) is exercised here because those are
// routine, not exceptional.
func TestRPCCredentialNeverLeaksToLogsOrErrors(t *testing.T) {
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

	// Capture everything logrus emits during the call.
	var logs bytes.Buffer
	prevOut, prevLevel := log.StandardLogger().Out, log.GetLevel()
	log.SetOutput(&logs)
	log.SetLevel(log.DebugLevel)
	defer func() { log.SetOutput(prevOut); log.SetLevel(prevLevel) }()

	client := NewRPCFallbackClient(RPCFallbackConfig{
		CustomEndpoint: srv.URL + "/?api-key=" + secret,
		Network:        "mainnet",
	})

	// The endpoint we HOLD is credential-free.
	require.NotContains(t, client.GetEndpoint(), secret)
	require.NotContains(t, client.GetEndpoint(), "api-key=")

	_, err := client.GetBalance(context.Background(), solanago.MustPublicKeyFromBase58("11111111111111111111111111111111"))
	require.Error(t, err)

	// The credential still reaches the provider.
	mu.Lock()
	keys := append([]string(nil), gotKeys...)
	mu.Unlock()
	require.NotEmpty(t, keys)
	require.Equal(t, secret, keys[0], "the provider must still receive the api key")

	// ...and nowhere else.
	require.NotContains(t, err.Error(), secret, "error text must not carry the RPC credential")
	require.NotContains(t, err.Error(), "api-key=", "error text must not carry an api-key query param")
	require.NotContains(t, logs.String(), secret, "log output must not carry the RPC credential")
	require.NotContains(t, logs.String(), "api-key=", "log output must not carry an api-key query param")

	// The failure is classifiable so HTTP handlers can map it to a generic body.
	require.True(t, errors.Is(err, ErrAllRPCEndpointsFailed))
}

// A credential embedded in a DEFAULT (Helius) endpoint is handled identically.
func TestDefaultEndpointsHoldNoCredential(t *testing.T) {
	for _, eps := range [][]RPCEndpoint{
		DefaultMainnetEndpoints("helius-secret"),
		DefaultDevnetEndpoints("helius-secret"),
	} {
		for _, ep := range eps {
			require.NotContains(t, ep.URL, "helius-secret")
			require.NotContains(t, ep.URL, "api-key=")
		}
		require.Equal(t, "helius-secret", eps[0].secret.Get("api-key"))
	}
}

// A wrapped upstream error keeps its identity for callers that classify it
// (river's rpc.ErrNotFound check) even though the rendered message is redacted.
func TestAllEndpointsFailedPreservesErrorChain(t *testing.T) {
	sentinel := errors.New("boom ?api-key=leak")
	err := &allEndpointsFailedError{operation: "GetTransaction", err: sentinel}
	require.True(t, errors.Is(err, sentinel))
	require.True(t, errors.Is(err, ErrAllRPCEndpointsFailed))
	require.NotContains(t, err.Error(), "leak")
	require.True(t, strings.Contains(err.Error(), "REDACTED"))
}
