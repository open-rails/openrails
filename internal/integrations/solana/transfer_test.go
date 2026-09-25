package solana

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/token"
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
			return signatureStatus("confirmed", nil)
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

func tokenBalance(idx int, mint solanago.PublicKey, amount uint64) map[string]any {
	return map[string]any{"accountIndex": idx, "mint": mint.String(), "uiTokenAmount": map[string]any{"amount": strconv.FormatUint(amount, 10), "decimals": 6}}
}

// An observation reports what the recipient received in the mint, bounded by
// both the instruction and the balance delta; the reference and memo decide
// whether the transaction belongs to the reference at all.
func TestObserveTransferReportsWhatTheRecipientReceived(t *testing.T) {
	payer := solanago.NewWallet().PublicKey()
	recipient := solanago.NewWallet().PublicKey()
	reference := solanago.NewWallet().PublicKey()
	mint := solanago.NewWallet().PublicKey()
	localID := uuid.New()
	const want = uint64(10_000_000)

	type tc struct {
		name     string
		symbol   string // "SOL" or an SPL symbol
		sent     uint64
		credited uint64
		creditIn solanago.PublicKey // SPL: mint recorded in token balances
		noRef    bool
		memo     string
		checked  *solanago.PublicKey // SPL: TransferChecked naming this mint (into the expected ATA)
		metaErr  any
		req      func(*ObserveTransferRequest)
		got      uint64
		wantErr  string
	}
	stamp := PurchaseMemo(localID)
	otherMint := solanago.NewWallet().PublicKey()
	// Paying straight into the token account skips ATA derivation, so only the mint check decides.
	recipientATA, _, err := solanago.FindAssociatedTokenAddress(recipient, mint)
	require.NoError(t, err)
	intoATA := func(expectedMint string) func(*ObserveTransferRequest) {
		return func(r *ObserveTransferRequest) {
			r.Recipient, r.TokenMint = recipientATA.String(), expectedMint
		}
	}
	swapCase := func(s string) string {
		out := []rune(s)
		for i, r := range out {
			switch {
			case r >= 'a' && r <= 'z':
				out[i] = r - 'a' + 'A'
			case r >= 'A' && r <= 'Z':
				out[i] = r - 'A' + 'a'
			}
		}
		return string(out)
	}
	cases := []tc{
		{name: "SOL exact", symbol: "SOL", sent: want, credited: want, memo: stamp, got: want},
		{name: "SOL overpay", symbol: "SOL", sent: want + 1, credited: want + 1, memo: stamp, got: want + 1},
		{name: "SOL underpay", symbol: "SOL", sent: want - 1, credited: want - 1, memo: stamp, got: want - 1},
		{name: "SOL instruction without balance movement", symbol: "SOL", sent: want, credited: want / 2, memo: stamp, got: want / 2},
		{name: "SOL to someone else", symbol: "SOL", sent: want, credited: want, memo: stamp, req: func(r *ObserveTransferRequest) { r.Recipient = payer.String() }, got: 0},
		{name: "SOL reference absent", symbol: "SOL", sent: want, credited: want, memo: stamp, noRef: true, wantErr: "reference key not included"},
		{name: "SOL when SPL mint expected", symbol: "SOL", sent: want, credited: want, memo: stamp, req: func(r *ObserveTransferRequest) { r.TokenMint = mint.String() }, got: 0},
		{name: "wrapped SOL mint accepts native SOL", symbol: "SOL", sent: want, credited: want, memo: stamp, req: func(r *ObserveTransferRequest) { r.TokenMint = wrappedSOLMint }, got: want},
		{name: "failed on chain", symbol: "SOL", sent: want, credited: want, memo: stamp, metaErr: map[string]any{"InstructionError": []any{0, "Custom"}}, wantErr: "failed on-chain"},
		{name: "memo for another record", symbol: "SOL", sent: want, credited: want, memo: PurchaseMemo(uuid.New()), wantErr: "purchase memo mismatch"},
		{name: "wallet dropped memo, optional", symbol: "SOL", sent: want, credited: want, req: func(r *ObserveTransferRequest) { r.MemoPolicy = MemoPresenceOptional }, got: want},
		{name: "we built it, memo missing", symbol: "SOL", sent: want, credited: want, wantErr: "purchase memo missing"},
		{name: "SPL exact", symbol: "USDC", sent: want, credited: want, creditIn: mint, memo: stamp, req: func(r *ObserveTransferRequest) { r.TokenMint = mint.String() }, got: want},
		{name: "SPL other mint", symbol: "USDC", sent: want, credited: want, creditIn: otherMint, memo: stamp, req: func(r *ObserveTransferRequest) { r.TokenMint = mint.String() }, got: 0},
		{name: "SPL short credit", symbol: "USDC", sent: want, credited: want - 1, creditIn: mint, memo: stamp, req: func(r *ObserveTransferRequest) { r.TokenMint = mint.String() }, got: want - 1},
		{name: "SPL when native SOL expected", symbol: "USDC", sent: want, credited: want, creditIn: mint, memo: stamp, got: 0},
		{name: "SPL into token account", symbol: "USDC", sent: want, credited: want, creditIn: mint, memo: stamp, req: intoATA(mint.String()), got: want},
		{name: "SPL mint differing only in case", symbol: "USDC", sent: want, credited: want, creditIn: mint, memo: stamp, req: intoATA(swapCase(mint.String())), got: 0},
		{name: "SPL into token account when native SOL expected", symbol: "USDC", sent: want, credited: want, creditIn: mint, memo: stamp, req: intoATA(""), got: 0},
		{name: "SPL checked exact", symbol: "USDC", sent: want, credited: want, creditIn: mint, checked: &mint, memo: stamp, req: func(r *ObserveTransferRequest) { r.TokenMint = mint.String() }, got: want},
		{name: "SPL checked other mint", symbol: "USDC", sent: want, credited: want, creditIn: otherMint, checked: &otherMint, memo: stamp, req: func(r *ObserveTransferRequest) { r.TokenMint = mint.String() }, got: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := TransferRequest{FromWallet: payer.String(), ToWallet: recipient.String(), TokenSymbol: c.symbol, TokenMint: mint.String(), Amount: c.sent, Reference: reference.String(), Memo: c.memo}
			if c.noRef {
				tr.Reference = ""
			}
			ixs, err := buildTransferInstructions(tr, payer, recipient)
			require.NoError(t, err)
			if c.checked != nil {
				src, _, _ := solanago.FindAssociatedTokenAddress(payer, mint)
				dst, _, _ := solanago.FindAssociatedTokenAddress(recipient, mint)
				b := token.NewTransferCheckedInstruction(c.sent, 6, src, *c.checked, dst, payer, nil)
				b.Accounts = append(b.Accounts, solanago.Meta(reference))
				ixs[len(ixs)-1] = b.Build()
			}
			tx, err := solanago.NewTransaction(ixs, solanago.Hash{}, solanago.TransactionPayer(payer))
			require.NoError(t, err)

			n := len(tx.Message.AccountKeys)
			pre, post := make([]uint64, n), make([]uint64, n)
			meta := map[string]any{"err": c.metaErr, "fee": 5000, "preBalances": pre, "postBalances": post, "preTokenBalances": []any{}, "postTokenBalances": []any{}}
			if c.symbol == "SOL" {
				pre[0], post[0] = 1_000_000_000, 1_000_000_000-c.credited
				post[keyIndex(t, tx, recipient)] = c.credited
			} else {
				ata, _, err := solanago.FindAssociatedTokenAddress(recipient, mint)
				require.NoError(t, err)
				idx := keyIndex(t, tx, ata)
				meta["preTokenBalances"] = []any{tokenBalance(idx, c.creditIn, 100)}
				meta["postTokenBalances"] = []any{tokenBalance(idx, c.creditIn, 100+c.credited)}
			}

			req := ObserveTransferRequest{
				Signature:   solanago.Signature{7}.String(),
				Recipient:   recipient.String(),
				Reference:   reference.String(),
				MemoLocalID: localID,
				MemoPolicy:  MemoRequired,
			}
			if c.req != nil {
				c.req(&req)
			}
			got, err := transferChain(t, tx, meta).ObserveTransfer(context.Background(), req)
			if c.wantErr != "" {
				require.ErrorContains(t, err, c.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.got, got.Amount)
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
