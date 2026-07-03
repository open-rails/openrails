package solana

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var (
	memoTestLocalID = uuid.MustParse("0dae1b8f-4c6e-4f6a-9b2d-7e5c3a1f8d42")
	memoTestOtherID = uuid.MustParse("f0e1d2c3-b4a5-4697-8879-6a5b4c3d2e1f")
)

// TestPurchaseMemoWirePin pins the exact on-chain memo bytes for a known
// local-id: the #714 recovery lane and external tooling parse these verbatim.
func TestPurchaseMemoWirePin(t *testing.T) {
	t.Parallel()

	got := PurchaseMemo(memoTestLocalID)
	require.Equal(t, "openrails:1:0dae1b8f-4c6e-4f6a-9b2d-7e5c3a1f8d42", got)
	require.Equal(t, []byte{
		'o', 'p', 'e', 'n', 'r', 'a', 'i', 'l', 's', ':', '1', ':',
		'0', 'd', 'a', 'e', '1', 'b', '8', 'f', '-',
		'4', 'c', '6', 'e', '-',
		'4', 'f', '6', 'a', '-',
		'9', 'b', '2', 'd', '-',
		'7', 'e', '5', 'c', '3', 'a', '1', 'f', '8', 'd', '4', '2',
	}, []byte(got))
}

func TestParsePurchaseMemo(t *testing.T) {
	t.Parallel()

	id, ok := ParsePurchaseMemo(PurchaseMemo(memoTestLocalID))
	require.True(t, ok)
	require.Equal(t, memoTestLocalID, id)

	// Surrounding whitespace tolerated.
	id, ok = ParsePurchaseMemo("  " + PurchaseMemo(memoTestLocalID) + "\n")
	require.True(t, ok)
	require.Equal(t, memoTestLocalID, id)

	for name, memo := range map[string]string{
		"foreign memo":        "thanks for the coffee",
		"empty":               "",
		"prefix only":         "openrails:1:",
		"other version":       "openrails:2:0dae1b8f-4c6e-4f6a-9b2d-7e5c3a1f8d42",
		"missing version":     "openrails:0dae1b8f-4c6e-4f6a-9b2d-7e5c3a1f8d42",
		"malformed uuid":      "openrails:1:not-a-uuid-at-all-not-a-uuid-at-all!",
		"non-canonical uuid":  "openrails:1:0dae1b8f4c6e4f6a9b2d7e5c3a1f8d42",
		"nil uuid":            "openrails:1:00000000-0000-0000-0000-000000000000",
		"trailing garbage":    "openrails:1:0dae1b8f-4c6e-4f6a-9b2d-7e5c3a1f8d42x",
		"case-changed prefix": "OpenRails:1:0dae1b8f-4c6e-4f6a-9b2d-7e5c3a1f8d42",
	} {
		id, ok := ParsePurchaseMemo(memo)
		require.False(t, ok, "expected reject: %s", name)
		require.Equal(t, uuid.Nil, id, name)
	}
}

// TestNewMemoInstructionWire pins the SPL Memo instruction encoding: program
// MemoSq4..., data = the raw memo string (NO length prefix), zero accounts.
func TestNewMemoInstructionWire(t *testing.T) {
	t.Parallel()

	memoStr := PurchaseMemo(memoTestLocalID)
	ix := NewMemoInstruction(memoStr)

	require.Equal(t, "MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr", ix.ProgramID().String())
	data, err := ix.Data()
	require.NoError(t, err)
	require.Equal(t, []byte(memoStr), data)
	require.Empty(t, ix.Accounts())
}

// TestBuildTransferInstructionsMemoOrdering pins the #713 instruction order:
// SPL Memo BEFORE the transfer (Solana Pay spec), for both the SOL and SPL
// token branches; no memo requested ⇒ no memo instruction.
func TestBuildTransferInstructionsMemoOrdering(t *testing.T) {
	t.Parallel()

	from := solanago.NewWallet().PublicKey()
	to := solanago.NewWallet().PublicKey()
	mint := solanago.NewWallet().PublicKey()
	memoStr := PurchaseMemo(memoTestLocalID)

	t.Run("native SOL", func(t *testing.T) {
		t.Parallel()
		ixs, err := buildTransferInstructions(TransferRequest{
			FromWallet:  from.String(),
			ToWallet:    to.String(),
			TokenSymbol: "SOL",
			Amount:      10_000_000,
			Memo:        memoStr,
		}, from, to)
		require.NoError(t, err)
		require.Len(t, ixs, 2)
		require.Equal(t, solanago.MemoProgramID, ixs[0].ProgramID())
		data, err := ixs[0].Data()
		require.NoError(t, err)
		require.Equal(t, []byte(memoStr), data)
		require.Equal(t, system.ProgramID, ixs[1].ProgramID())
	})

	t.Run("SPL token", func(t *testing.T) {
		t.Parallel()
		ixs, err := buildTransferInstructions(TransferRequest{
			FromWallet:  from.String(),
			ToWallet:    to.String(),
			TokenSymbol: "USDC",
			TokenMint:   mint.String(),
			Amount:      5_000_000,
			Memo:        memoStr,
		}, from, to)
		require.NoError(t, err)
		require.Len(t, ixs, 2)
		require.Equal(t, solanago.MemoProgramID, ixs[0].ProgramID())
		require.Equal(t, solanago.TokenProgramID, ixs[1].ProgramID())
	})

	t.Run("no memo", func(t *testing.T) {
		t.Parallel()
		ixs, err := buildTransferInstructions(TransferRequest{
			FromWallet:  from.String(),
			ToWallet:    to.String(),
			TokenSymbol: "SOL",
			Amount:      10_000_000,
		}, from, to)
		require.NoError(t, err)
		require.Len(t, ixs, 1)
		require.Equal(t, system.ProgramID, ixs[0].ProgramID())
	})
}

// memoTestTx builds a legacy SOL transfer tx (payer→recipient) with the given
// extra leading instructions.
func memoTestTx(t *testing.T, payer, recipient solanago.PublicKey, amount uint64, leading ...solanago.Instruction) *solanago.Transaction {
	t.Helper()
	ixs := append(append([]solanago.Instruction{}, leading...),
		system.NewTransferInstruction(amount, payer, recipient).Build())
	tx, err := solanago.NewTransaction(ixs, solanago.Hash{}, solanago.TransactionPayer(payer))
	require.NoError(t, err)
	return tx
}

func TestPurchaseMemoLocalIDsAndVerify(t *testing.T) {
	t.Parallel()

	payer := solanago.NewWallet().PublicKey()
	recipient := solanago.NewWallet().PublicKey()

	stamped := memoTestTx(t, payer, recipient, 10, NewMemoInstruction(PurchaseMemo(memoTestLocalID)))
	unstamped := memoTestTx(t, payer, recipient, 10)
	foreign := memoTestTx(t, payer, recipient, 10, NewMemoInstruction("gm"))

	require.Equal(t, []uuid.UUID{memoTestLocalID}, PurchaseMemoLocalIDs(stamped))
	require.Empty(t, PurchaseMemoLocalIDs(unstamped))
	require.Empty(t, PurchaseMemoLocalIDs(foreign))
	require.Empty(t, PurchaseMemoLocalIDs(nil))

	// present-and-matching passes
	require.NoError(t, VerifyPurchaseMemo(stamped, memoTestLocalID))
	// present-and-wrong fails
	err := VerifyPurchaseMemo(stamped, memoTestOtherID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "purchase memo mismatch")
	// absent passes (backward compatible)
	require.NoError(t, VerifyPurchaseMemo(unstamped, memoTestLocalID))
	// foreign memos are not ours: ignored
	require.NoError(t, VerifyPurchaseMemo(foreign, memoTestLocalID))
	// Nil want skips
	require.NoError(t, VerifyPurchaseMemo(stamped, uuid.Nil))
}

// memoTestTxResult wraps a tx into a GetTransactionResult whose envelope
// decodes via GetTransaction() (base64 lane), with SOL balance deltas that
// satisfy verifyBalanceChanges for the recipient.
func memoTestTxResult(t *testing.T, tx *solanago.Transaction, amount uint64) *rpc.GetTransactionResult {
	t.Helper()
	raw, err := tx.MarshalBinary()
	require.NoError(t, err)
	payload, err := json.Marshal([]any{base64.StdEncoding.EncodeToString(raw), "base64"})
	require.NoError(t, err)
	env := new(rpc.TransactionResultEnvelope)
	require.NoError(t, env.UnmarshalJSON(payload))

	n := len(tx.Message.AccountKeys)
	pre := make([]uint64, n)
	post := make([]uint64, n)
	pre[0] = 1_000_000_000
	post[0] = 1_000_000_000 - amount
	post[1] = pre[1] + amount // recipient is the first writable non-signer
	return &rpc.GetTransactionResult{
		Transaction: env,
		Meta:        &rpc.TransactionMeta{PreBalances: pre, PostBalances: post},
	}
}

// TestValidateTransactionContentMemoVerifyLeg exercises the verify leg
// end-to-end at validateTransactionContent altitude: present-and-matching
// passes, present-and-wrong fails, absent passes.
func TestValidateTransactionContentMemoVerifyLeg(t *testing.T) {
	t.Parallel()

	payer := solanago.NewWallet().PublicKey()
	recipient := solanago.NewWallet().PublicKey()
	const amount = uint64(10_000_000)

	stamped := memoTestTx(t, payer, recipient, amount, NewMemoInstruction(PurchaseMemo(memoTestLocalID)))
	unstamped := memoTestTx(t, payer, recipient, amount)

	require.NoError(t, validateTransactionContent(
		memoTestTxResult(t, stamped, amount), amount, recipient.String(), "", payer.String(), nil, memoTestLocalID))

	err := validateTransactionContent(
		memoTestTxResult(t, stamped, amount), amount, recipient.String(), "", payer.String(), nil, memoTestOtherID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "purchase memo mismatch")

	require.NoError(t, validateTransactionContent(
		memoTestTxResult(t, unstamped, amount), amount, recipient.String(), "", payer.String(), nil, memoTestLocalID))
}
