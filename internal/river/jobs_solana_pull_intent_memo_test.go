package riverjobs

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
)

func landedPull(t *testing.T, memoLocalID uuid.UUID) *rpc.GetTransactionResult {
	t.Helper()
	payer, recipient := solanago.NewWallet().PublicKey(), solanago.NewWallet().PublicKey()
	var ixs []solanago.Instruction
	if memoLocalID != uuid.Nil {
		ixs = append(ixs, solanaint.NewMemoInstruction(solanaint.PurchaseMemo(memoLocalID)))
	}
	ixs = append(ixs, system.NewTransferInstruction(1, payer, recipient).Build())
	tx, err := solanago.NewTransaction(ixs, solanago.Hash{}, solanago.TransactionPayer(payer))
	require.NoError(t, err)
	raw, err := tx.MarshalBinary()
	require.NoError(t, err)
	payload, err := json.Marshal([]any{base64.StdEncoding.EncodeToString(raw), "base64"})
	require.NoError(t, err)
	env := new(rpc.TransactionResultEnvelope)
	require.NoError(t, env.UnmarshalJSON(payload))
	return &rpc.GetTransactionResult{Transaction: env, Meta: &rpc.TransactionMeta{}}
}

// #713/or#893: OpenRails builds, stamps and records the pull signature before
// submit, so the landed tx at that signature must carry THIS intent's memo; a
// different or absent memo is not our transaction (parked, never repaired). A
// thin RPC answer with no tx payload is not memo evidence and passes.
func TestVerifyPullMemoMatchesIntent(t *testing.T) {
	intentID := uuid.New()
	require.NoError(t, verifyPullMemoMatchesIntent(landedPull(t, intentID), intentID))
	require.ErrorContains(t, verifyPullMemoMatchesIntent(landedPull(t, uuid.New()), intentID), "purchase memo mismatch")
	require.ErrorContains(t, verifyPullMemoMatchesIntent(landedPull(t, uuid.Nil), intentID), "purchase memo missing")
	require.NoError(t, verifyPullMemoMatchesIntent(nil, intentID))
	require.NoError(t, verifyPullMemoMatchesIntent(&rpc.GetTransactionResult{}, intentID))
}
