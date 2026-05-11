package solana

import (
	"context"
	"testing"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/programs/system"
	"github.com/doujins-org/solana-go/rpc"
	"github.com/stretchr/testify/require"
)

func TestVerifyTransferRequiresExpectedContentFields(t *testing.T) {
	t.Parallel()

	client := &RPCClient{}

	t.Run("requires amount", func(t *testing.T) {
		t.Parallel()

		err := client.VerifyTransfer(context.Background(), VerifyTransferRequest{
			Signature:         "dummy-signature",
			ExpectedAmount:    0,
			ExpectedRecipient: "DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh",
			ExpectedReference: "11111111111111111111111111111112",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "expected amount must be greater than 0")
	})

	t.Run("requires recipient", func(t *testing.T) {
		t.Parallel()

		err := client.VerifyTransfer(context.Background(), VerifyTransferRequest{
			Signature:         "dummy-signature",
			ExpectedAmount:    123,
			ExpectedRecipient: "",
			ExpectedReference: "11111111111111111111111111111112",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "expected recipient is required")
	})

	t.Run("requires reference", func(t *testing.T) {
		t.Parallel()

		err := client.VerifyTransfer(context.Background(), VerifyTransferRequest{
			Signature:         "dummy-signature",
			ExpectedAmount:    123,
			ExpectedRecipient: "DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh",
			ExpectedReference: "",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "expected reference is required")
	})
}

func TestFindTransferMatchRejectsSystemTransferWhenSPLMintExpected(t *testing.T) {
	t.Parallel()

	payer := mustPublicKey(t, "11111111111111111111111111111112")
	recipient := mustPublicKey(t, "DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh")
	transfer := system.NewTransferInstruction(10_000_000, payer, recipient).Build()
	tx, err := solanago.NewTransaction(
		[]solanago.Instruction{transfer},
		solanago.Hash{},
		solanago.TransactionPayer(payer),
	)
	require.NoError(t, err)

	candidates := map[string]struct{}{recipient.String(): {}}
	txResult := &rpc.GetTransactionResult{Meta: &rpc.TransactionMeta{}}

	match, err := findTransferMatch(tx, txResult, candidates, "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU", 10_000_000, "")
	require.NoError(t, err)
	require.Nil(t, match)

	match, err = findTransferMatch(tx, txResult, candidates, wrappedSOLMint, 10_000_000, "")
	require.NoError(t, err)
	require.NotNil(t, match)
	require.Equal(t, "system", match.program)
}

func mustPublicKey(t *testing.T, value string) solanago.PublicKey {
	t.Helper()
	key, err := solanago.PublicKeyFromBase58(value)
	require.NoError(t, err)
	return key
}
