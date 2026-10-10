package riverjobs

import (
	"context"
	"errors"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/stretchr/testify/require"

	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
)

type pullChain struct {
	result   *rpc.GetTransactionResult
	err      error
	expired  bool
	terminal solanaint.ChainTerminal
}

func (c *pullChain) GetTransaction(context.Context, solanago.Signature) (*rpc.GetTransactionResult, error) {
	return c.result, c.err
}

func (c *pullChain) TransactionExpiredUnseen(_ context.Context, _ solanago.Signature, terminal solanaint.ChainTerminal) (bool, error) {
	c.terminal = terminal
	return c.expired, nil
}

func TestPullRetryNeedsNonexecutionEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		chain pullChain
		want  sigVerdict
	}{
		{"not found is not nonexecution", pullChain{err: rpc.ErrNotFound}, sigVerdictUnknown},
		{"null is not nonexecution", pullChain{}, sigVerdictUnknown},
		{"expired and unseen permits retry", pullChain{err: rpc.ErrNotFound, expired: true}, sigVerdictNotLanded},
		{"read failure stays unresolved", pullChain{err: errors.New("RPC unavailable"), expired: true}, sigVerdictUnknown},
		{"landed payment needs repair", pullChain{result: &rpc.GetTransactionResult{Meta: &rpc.TransactionMeta{}}}, sigVerdictLanded},
		{"reverted payment permits retry", pullChain{result: &rpc.GetTransactionResult{Meta: &rpc.TransactionMeta{Err: "reverted"}}}, sigVerdictNotLanded},
		{"missing execution metadata stays unresolved", pullChain{result: &rpc.GetTransactionResult{}}, sigVerdictUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &SolanaPullIntentHandler{Chain: &tc.chain}
			_, verdict := h.checkSignature(t.Context(), solanaPullEvidence{TransactionID: solanago.Signature{1}.String(), LastValidBlockHeight: 100, BlockhashSlot: 50})
			require.Equal(t, tc.want, verdict)
			if errors.Is(tc.chain.err, rpc.ErrNotFound) {
				require.Equal(t, uint64(100), tc.chain.terminal.LastValidBlockHeight)
				require.Equal(t, uint64(50), tc.chain.terminal.BlockhashSlot)
			}
		})
	}
}
