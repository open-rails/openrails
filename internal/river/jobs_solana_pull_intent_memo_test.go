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

// pullMemoTxResult wraps a legacy tx (with optional #713 memo stamped first)
// into the GetTransactionResult shape the verify leg reads off the chain.
func pullMemoTxResult(t *testing.T, memoLocalID uuid.UUID) *rpc.GetTransactionResult {
	t.Helper()
	payer := solanago.NewWallet().PublicKey()
	recipient := solanago.NewWallet().PublicKey()
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

// TestVerifyPullMemoMatchesIntent pins the recurring verify-leg #713 rule:
// a landed pull whose stamped memo names THIS intent passes; a different
// local-id fails (→ parked, never auto-repaired).
//
// or#893: an ABSENT memo now fails too. OpenRails builds and signs the pull
// transaction and stamps the intent id before submission, and the signature
// being verified is the one recorded pre-submit — so an unstamped transaction
// at that signature is not "a pre-memo pull", it is not the transaction we
// built. Undecodable/absent tx payloads still pass: that is a thin RPC answer,
// not evidence about the memo.
func TestVerifyPullMemoMatchesIntent(t *testing.T) {
	t.Parallel()

	intentID := uuid.MustParse("6b1f3c2d-9a8e-4b7c-8d5f-0e1a2b3c4d5e")
	otherID := uuid.MustParse("0dae1b8f-4c6e-4f6a-9b2d-7e5c3a1f8d42")

	// present-and-matching passes
	require.NoError(t, verifyPullMemoMatchesIntent(pullMemoTxResult(t, intentID), intentID))

	// present-and-wrong fails
	err := verifyPullMemoMatchesIntent(pullMemoTxResult(t, otherID), intentID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "purchase memo mismatch")

	// absent memo fails: we stamp every pull we build
	err = verifyPullMemoMatchesIntent(pullMemoTxResult(t, uuid.Nil), intentID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "purchase memo missing")

	// no result / no tx payload passes (fakes and thin RPC answers)
	require.NoError(t, verifyPullMemoMatchesIntent(nil, intentID))
	require.NoError(t, verifyPullMemoMatchesIntent(&rpc.GetTransactionResult{}, intentID))
}
