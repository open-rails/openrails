package solana

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// transferChain serves one landed transaction through the real RPC decode path.
func transferChain(t *testing.T, tx *solanago.Transaction, meta map[string]any) *RPCClient {
	t.Helper()
	raw, err := tx.MarshalBinary()
	require.NoError(t, err)
	_, url := newRPCStub(t, func(method string, _ int, _ []json.RawMessage) any {
		switch method {
		case "getSignatureStatuses":
			return signatureStatus("finalized", nil)
		case "getTransaction":
			return map[string]any{
				"slot": 7, "blockTime": 1_700_000_000,
				"transaction": []string{base64.StdEncoding.EncodeToString(raw), "base64"},
				"meta":        meta,
			}
		}
		return rpcFault{Code: -32601, Message: "unexpected " + method}
	})
	return NewRPCClientWithConfig(RPCClientConfig{Endpoint: url, Network: "devnet"})
}

func keyIndex(t *testing.T, tx *solanago.Transaction, key solanago.PublicKey) int {
	t.Helper()
	for i, k := range tx.Message.AccountKeys {
		if k.Equals(key) {
			return i
		}
	}
	t.Fatalf("key %s not in transaction", key)
	return -1
}

func tokenBalance(idx int, mint, owner, program solanago.PublicKey, amount uint64) map[string]any {
	return map[string]any{"accountIndex": idx, "mint": mint.String(), "owner": owner.String(), "programId": program.String(),
		"uiTokenAmount": map[string]any{"amount": strconv.FormatUint(amount, 10), "decimals": 6}}
}

// An observation is the recipient's balance change in the expected asset,
// however it was moved: one instruction, several, inside another program, via
// Token-2022, or into any token account the recipient owns. Value in another
// asset is reported as Other. The reference and memo decide whether the
// transaction belongs to the reference at all.
func TestObserveTransferReportsWhatTheRecipientReceived(t *testing.T) {
	payer := solanago.NewWallet().PublicKey()
	recipient := solanago.NewWallet().PublicKey()
	reference := solanago.NewWallet().PublicKey()
	mint := solanago.NewWallet().PublicKey()
	otherMint := solanago.NewWallet().PublicKey()
	stray := solanago.NewWallet().PublicKey() // a token account the recipient owns, not its ATA
	localID := uuid.New()
	stamp := PurchaseMemo(localID)
	const want = uint64(10_000_000)

	type credit struct {
		account solanago.PublicKey // token account credited, or the recipient wallet for lamports
		mint    solanago.PublicKey // zero = lamports
		program solanago.PublicKey
		owner   solanago.PublicKey
		amount  uint64
	}
	ata := func(m, program solanago.PublicKey) solanago.PublicKey {
		a, err := AssociatedTokenAddress(recipient, m, program)
		require.NoError(t, err)
		return a
	}
	lamports := func(n uint64) credit { return credit{account: recipient, amount: n} }
	tokens := func(account, m, program solanago.PublicKey, n uint64) credit {
		return credit{account: account, mint: m, program: program, owner: recipient, amount: n}
	}
	for _, c := range []struct {
		name     string
		credits  []credit
		mint     string
		memo     string
		optional bool
		noRef    bool
		metaErr  any
		got      uint64
		other    bool
		wantErr  error
	}{
		{name: "SOL exact", credits: []credit{lamports(want)}, memo: stamp, got: want},
		{name: "SOL over", credits: []credit{lamports(want + 1)}, memo: stamp, got: want + 1},
		{name: "SOL under", credits: []credit{lamports(want - 1)}, memo: stamp, got: want - 1},
		{name: "wrapped SOL mint is native SOL", credits: []credit{lamports(want)}, mint: wrappedSOLMint, memo: stamp, got: want},
		{name: "nothing to the recipient", memo: stamp},
		{name: "SOL when a token is expected", credits: []credit{lamports(want)}, mint: mint.String(), memo: stamp, other: true},
		{name: "legacy SPL", credits: []credit{tokens(ata(mint, solanago.TokenProgramID), mint, solanago.TokenProgramID, want)}, mint: mint.String(), memo: stamp, got: want},
		{name: "Token-2022", credits: []credit{tokens(ata(mint, Token2022ProgramID), mint, Token2022ProgramID, want)}, mint: mint.String(), memo: stamp, got: want},
		{name: "split across two accounts", credits: []credit{tokens(ata(mint, solanago.TokenProgramID), mint, solanago.TokenProgramID, want/2), tokens(stray, mint, solanago.TokenProgramID, want/2)}, mint: mint.String(), memo: stamp, got: want},
		{name: "into a non-ATA account the recipient owns", credits: []credit{tokens(stray, mint, solanago.TokenProgramID, want)}, mint: mint.String(), memo: stamp, got: want},
		{name: "wrong mint", credits: []credit{tokens(ata(otherMint, solanago.TokenProgramID), otherMint, solanago.TokenProgramID, want)}, mint: mint.String(), memo: stamp, other: true},
		{name: "token to someone else", credits: []credit{{account: stray, mint: mint, program: solanago.TokenProgramID, owner: payer, amount: want}}, mint: mint.String(), memo: stamp},
		{name: "reference absent", credits: []credit{lamports(want)}, memo: stamp, noRef: true, wantErr: ErrForeignTransfer},
		{name: "memo for another record", credits: []credit{lamports(want)}, memo: PurchaseMemo(uuid.New()), wantErr: ErrForeignTransfer},
		{name: "we built it, memo missing", credits: []credit{lamports(want)}, wantErr: ErrForeignTransfer},
		{name: "wallet dropped memo", credits: []credit{lamports(want)}, optional: true, got: want},
		{name: "failed on chain", credits: []credit{lamports(want)}, memo: stamp, metaErr: map[string]any{"InstructionError": []any{0, "Custom"}}, wantErr: ErrFailedOnChain},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The only instruction names the reference and moves nothing
			// itself: the credit is whatever the balances say, as for a
			// payment made inside another program.
			ixs := []solanago.Instruction{}
			if c.memo != "" {
				ixs = append(ixs, NewMemoInstruction(c.memo))
			}
			accounts := []*solanago.AccountMeta{solanago.Meta(payer).SIGNER().WRITE(), solanago.Meta(recipient)}
			if !c.noRef {
				accounts = append(accounts, solanago.Meta(reference))
			}
			for _, cr := range c.credits {
				accounts = append(accounts, solanago.Meta(cr.account).WRITE())
			}
			ixs = append(ixs, solanago.NewInstruction(solanago.NewWallet().PublicKey(), accounts, []byte{1}))
			tx, err := solanago.NewTransaction(ixs, solanago.Hash{}, solanago.TransactionPayer(payer))
			require.NoError(t, err)

			n := len(tx.Message.AccountKeys)
			pre, post := make([]uint64, n), make([]uint64, n)
			var preTokens, postTokens []any
			for _, cr := range c.credits {
				idx := keyIndex(t, tx, cr.account)
				if cr.mint.IsZero() {
					post[idx] += cr.amount
					continue
				}
				preTokens = append(preTokens, tokenBalance(idx, cr.mint, cr.owner, cr.program, 100))
				postTokens = append(postTokens, tokenBalance(idx, cr.mint, cr.owner, cr.program, 100+cr.amount))
			}
			meta := map[string]any{"err": c.metaErr, "fee": 5000, "preBalances": pre, "postBalances": post, "preTokenBalances": preTokens, "postTokenBalances": postTokens}
			policy := MemoRequired
			if c.optional {
				policy = MemoPresenceOptional
			}
			got, err := transferChain(t, tx, meta).ObserveTransfer(context.Background(), ObserveTransferRequest{
				Signature: solanago.Signature{7}.String(), Recipient: recipient.String(), TokenMint: c.mint, Reference: reference.String(),
				MemoLocalID: localID, MemoPolicy: policy,
			})
			if c.wantErr != nil {
				require.ErrorIs(t, err, c.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.got, got.Amount)
			require.Equal(t, c.other, got.Other)
			require.Equal(t, payer.String(), got.Payer)
			require.Equal(t, time.Unix(1_700_000_000, 0).UTC(), *got.LandedAt, "#651: landed at the chain's block time")
		})
	}
}

func TestObserveTransferRequiresRecipientAndReference(t *testing.T) {
	c := &RPCClient{}
	_, err := c.ObserveTransfer(context.Background(), ObserveTransferRequest{Signature: "x", Reference: solanago.SystemProgramID.String()})
	require.ErrorContains(t, err, "recipient and reference are required")
	_, err = c.ObserveTransfer(context.Background(), ObserveTransferRequest{Signature: "x", Recipient: solanago.SystemProgramID.String()})
	require.ErrorContains(t, err, "recipient and reference are required")
}
