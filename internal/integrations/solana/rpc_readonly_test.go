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
