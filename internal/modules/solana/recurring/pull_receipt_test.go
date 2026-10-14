package recurring

import (
	"context"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

type receiptRead struct{ result *rpc.GetTransactionResult }

func (r receiptRead) GetTransaction(context.Context, solanago.Signature) (*rpc.GetTransactionResult, error) {
	return r.result, nil
}

// Land the actual program pull, then qualify the RPC's signed envelope and
// token balances. Negative controls retain the successful payment while
// changing the claimed agreement or making its proof unreadable.
func TestPullReceiptQualifiesExactSignedTransfer(t *testing.T) {
	c := newTierChain(t)
	id := uuid.New()
	amount := uint64(23_000_000)
	from, _, err := subscriptions.DeriveATA(c.subscriber.PublicKey(), c.mint, solanago.TokenProgramID)
	require.NoError(t, err)
	to, _, err := subscriptions.DeriveATA(testTreasury, c.mint, solanago.TokenProgramID)
	require.NoError(t, err)
	events, _, err := subscriptions.DeriveEventAuthority()
	require.NoError(t, err)
	pull := subscriptions.BuildTransferSubscription(subscriptions.TransferSubscriptionParams{SubscriptionPDA: solanago.MustPublicKeyFromBase58(c.old.SubscriptionPDA),
		PlanPDA: solanago.MustPublicKeyFromBase58(c.old.PlanPDA), SubscriptionAuthority: solanago.MustPublicKeyFromBase58(c.old.AuthorityPDA),
		DelegatorATA: from, ReceiverATA: to, Caller: c.merchant, Mint: c.mint, TokenProgram: solanago.TokenProgramID, EventAuthority: events,
		Amount: amount, Delegator: c.subscriber.PublicKey()})
	land := func(ixs []solanago.Instruction, coSign bool) solanago.Signature {
		hash, err := c.rpc.GetLatestBlockhash(t.Context())
		require.NoError(t, err)
		tx, err := solanago.NewTransaction(ixs, hash, solanago.TransactionPayer(c.merchant))
		require.NoError(t, err)
		msg, err := tx.Message.MarshalBinary()
		require.NoError(t, err)
		sig, err := c.signer.SignMessage(t.Context(), msg)
		require.NoError(t, err)
		tx.Signatures = make([]solanago.Signature, tx.Message.Header.NumRequiredSignatures)
		tx.Signatures[0] = sig
		if coSign {
			tx = signAs(t, tx, c.subscriber)
		}
		return c.land(t, tx)
	}
	sig := land([]solanago.Instruction{solanaint.NewMemoInstruction(solanaint.PurchaseMemo(id)), pull}, false)
	got, err := VerifyPullReceipt(t.Context(), c.rpc, c.old, sig.String(), amount, 720, 6, id)
	require.NoError(t, err)
	require.Equal(t, amount, got.Amount)
	require.Equal(t, c.now, got.PeriodStart)
	require.Equal(t, c.now.Add(720*time.Hour), got.PeriodEnd)
	original, err := c.rpc.GetTransaction(t.Context(), sig)
	require.NoError(t, err)
	for name, mutate := range map[string]func(*rpc.GetTransactionResult, *models.SolanaSubscription){
		"missing transaction":  func(r *rpc.GetTransactionResult, _ *models.SolanaSubscription) { r.Transaction = nil },
		"missing metadata":     func(r *rpc.GetTransactionResult, _ *models.SolanaSubscription) { r.Meta = nil },
		"missing block time":   func(r *rpc.GetTransactionResult, _ *models.SolanaSubscription) { r.BlockTime = nil },
		"reverted transaction": func(r *rpc.GetTransactionResult, _ *models.SolanaSubscription) { r.Meta.Err = "reverted" },
		"foreign treasury":     func(_ *rpc.GetTransactionResult, row *models.SolanaSubscription) { row.Treasury = randAddr(t) },
		"foreign wallet":       func(_ *rpc.GetTransactionResult, row *models.SolanaSubscription) { row.SubscriberWallet = randAddr(t) },
		"foreign mint":         func(_ *rpc.GetTransactionResult, row *models.SolanaSubscription) { row.Mint = randAddr(t) },
	} {
		t.Run(name, func(t *testing.T) {
			copy := *original
			meta := *original.Meta
			copy.Meta = &meta
			row := *c.old
			mutate(&copy, &row)
			_, err := VerifyPullReceipt(t.Context(), receiptRead{&copy}, &row, sig.String(), amount, 720, 6, id)
			require.ErrorIs(t, err, ErrPaymentUnverified)
		})
	}
	_, err = VerifyPullReceipt(t.Context(), c.rpc, c.old, sig.String(), amount+1, 720, 6, id)
	require.ErrorIs(t, err, ErrPaymentUnverified)
	_, err = VerifyPullReceipt(t.Context(), c.rpc, c.old, sig.String(), amount, 720, 5, id)
	require.ErrorIs(t, err, ErrPaymentUnverified)
	_, err = VerifyPullReceipt(t.Context(), c.rpc, c.old, sig.String(), amount, 720, 6, uuid.New())
	require.ErrorIs(t, err, ErrPaymentUnverified)
	withoutMemo := land([]solanago.Instruction{pull}, false)
	_, err = VerifyPullReceipt(t.Context(), c.rpc, c.old, withoutMemo.String(), amount, 720, 6, id)
	require.ErrorIs(t, err, ErrPaymentUnverified)
	// Extra genuine money is retained on the node but cannot be represented as
	// this episode's exact full-period payment or grant another period.
	extra := land([]solanago.Instruction{solanaint.NewMemoInstruction(solanaint.PurchaseMemo(id)), pull,
		token.NewTransferInstruction(1, from, to, c.subscriber.PublicKey(), nil).Build()}, true)
	_, err = VerifyPullReceipt(t.Context(), c.rpc, c.old, extra.String(), amount, 720, 6, id)
	require.ErrorIs(t, err, ErrPaymentUnverified)
}
