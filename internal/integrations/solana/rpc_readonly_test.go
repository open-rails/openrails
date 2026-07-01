package solana

import (
	"context"
	"errors"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
)

// TestReadOnlyBlocksTransactionSubmission pins the #346 Solana wire choke:
// in readonly mode both submission paths fail locally with the sentinel,
// before any RPC endpoint is contacted (the client has no reachable endpoint
// here, so a pass-through would error differently).
func TestReadOnlyBlocksTransactionSubmission(t *testing.T) {
	c := NewRPCFallbackClient(RPCFallbackConfig{ReadOnly: true, CustomEndpoint: "http://127.0.0.1:1"})

	_, err := c.SendTransaction(context.Background(), &solanago.Transaction{})
	if !errors.Is(err, ErrProviderReadOnly) {
		t.Fatalf("SendTransaction: expected ErrProviderReadOnly, got %v", err)
	}
	_, err = c.SendTransactionSkipPreflight(context.Background(), &solanago.Transaction{})
	if !errors.Is(err, ErrProviderReadOnly) {
		t.Fatalf("SendTransactionSkipPreflight: expected ErrProviderReadOnly, got %v", err)
	}
}

func TestRPCProviderKeySelectsHeliusEndpoint(t *testing.T) {
	c := NewRPCFallbackClient(RPCFallbackConfig{Network: "devnet", RPCProvider: "helius", RPCAPIKey: "rpc-key"})
	if len(c.endpoints) < 2 {
		t.Fatalf("expected helius plus public fallback, got %#v", c.endpoints)
	}
	if got := c.endpoints[0].URL; got != "https://devnet.helius-rpc.com/?api-key=rpc-key" {
		t.Fatalf("expected helius devnet endpoint, got %q", got)
	}
}

func TestRPCProviderPublicIgnoresAPIKey(t *testing.T) {
	c := NewRPCFallbackClient(RPCFallbackConfig{Network: "mainnet", RPCProvider: "public", RPCAPIKey: "ignored"})
	if len(c.endpoints) != 1 {
		t.Fatalf("expected only public endpoint, got %#v", c.endpoints)
	}
	if got := c.endpoints[0].URL; got != "https://api.mainnet-beta.solana.com" {
		t.Fatalf("expected public endpoint, got %q", got)
	}
}
