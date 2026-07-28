package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"

	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
)

// #SEC-17: the response body an authenticated customer sees on an RPC failure
// must carry no provider credential and no upstream URL. Drives the REAL error
// the fallback client produces, not a hand-built string.
func TestSolanaClientErrorDoesNotEchoRPCDetail(t *testing.T) {
	const secret = "merchant-rpc-secret-key"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := solanarpc.NewRPCClientWithConfig(solanarpc.RPCClientConfig{
		Endpoint: srv.URL + "/?api-key=" + secret,
		Network:  "mainnet",
	})
	_, err := client.GetBalance(context.Background(), solanago.MustPublicKeyFromBase58("11111111111111111111111111111111"))
	require.Error(t, err)

	status, msg := solanaClientError(err, http.StatusBadRequest)
	require.Equal(t, http.StatusBadGateway, status)
	require.NotContains(t, msg, secret)
	require.NotContains(t, msg, "api-key=")
	require.NotContains(t, msg, srv.URL)
	require.Equal(t, "Solana RPC is temporarily unavailable; please retry", msg)
}

// A domain (non-transport) error keeps its message, minus any credential.
func TestSolanaClientErrorKeepsDomainMessage(t *testing.T) {
	status, msg := solanaClientError(errors.New("subscriber already enrolled"), http.StatusBadRequest)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "subscriber already enrolled", msg)
}
