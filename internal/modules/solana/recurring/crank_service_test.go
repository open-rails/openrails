package recurring

import (
	"context"
	"fmt"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

// crankSubmitter is the cranker's submitter: it records what it signs.
type crankSubmitter struct {
	cranker      solanago.PublicKey
	instructions []solanago.Instruction
}

func (s *crankSubmitter) Publisher(context.Context) (solanago.PublicKey, error) {
	return s.cranker, nil
}

func (s *crankSubmitter) Signer(_ context.Context, owner solanago.PublicKey) (solanaint.Signer, error) {
	if !owner.Equals(s.cranker) {
		return nil, fmt.Errorf("%w (owner %s)", ErrUnheldPlan, owner)
	}
	return nil, nil
}

func (s *crankSubmitter) Submit(_ context.Context, _ billing.MerchantID, _ solanago.PublicKey, ixs []solanago.Instruction) (solanago.Signature, error) {
	s.instructions = ixs
	return solanago.Signature{}, nil
}

func crankRow(t *testing.T, cranker solanago.PublicKey) *models.SolanaSubscription {
	return &models.SolanaSubscription{
		MerchantAddress:  cranker.String(),
		Treasury:         randAddr(t),
		Mint:             randAddr(t),
		SubscriberWallet: randAddr(t),
		PlanPDA:          randAddr(t),
		SubscriptionPDA:  randAddr(t),
		AuthorityPDA:     randAddr(t),
	}
}

// The cranker pulls its plan into the subscription's treasury and, with a
// pull-intent id, the pull is stamped [SPL Memo "openrails:1:<id>",
// transfer_subscription], memo first. Without an id it is the bare transfer.
func TestCrankPullShape(t *testing.T) {
	intentID := uuid.MustParse("6b1f3c2d-9a8e-4b7c-8d5f-0e1a2b3c4d5e")
	for _, memoID := range []uuid.UUID{intentID, uuid.Nil} {
		cranker := randKey(t).PublicKey()
		row := crankRow(t, cranker)
		sub := &crankSubmitter{cranker: cranker}
		_, err := NewCrankService(sub).CrankWithPresubmit(context.Background(), testMerchantID, row, 10_000_000, memoID, nil)
		require.NoError(t, err)

		transfer := sub.instructions[len(sub.instructions)-1]
		require.True(t, transfer.ProgramID().Equals(subscriptions.ProgramID))
		accounts := transfer.Accounts()
		receiver, _, _ := subscriptions.DeriveATA(solanago.MustPublicKeyFromBase58(row.Treasury), solanago.MustPublicKeyFromBase58(row.Mint), solanago.TokenProgramID)
		require.True(t, accounts[4].PublicKey.Equals(receiver), "the pull lands in the treasury's token account")
		require.True(t, accounts[5].PublicKey.Equals(cranker) && accounts[5].IsSigner, "the cranker calls")
		if memoID == uuid.Nil {
			require.Len(t, sub.instructions, 1)
			continue
		}
		require.Len(t, sub.instructions, 2)
		memo := sub.instructions[0]
		require.Equal(t, "MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr", memo.ProgramID().String())
		data, err := memo.Data()
		require.NoError(t, err)
		require.Equal(t, "openrails:1:"+intentID.String(), string(data))
		require.Empty(t, memo.Accounts())
	}
}

func TestCrankRejectsBadInput(t *testing.T) {
	cranker := randKey(t).PublicKey()
	svc := NewCrankService(&crankSubmitter{cranker: cranker})
	_, err := svc.Crank(context.Background(), testMerchantID, nil, 1)
	require.Error(t, err)
	_, err = svc.Crank(context.Background(), testMerchantID, crankRow(t, cranker), 0)
	require.Error(t, err, "a zero pull")
	row := crankRow(t, cranker)
	row.PlanPDA = "not-base58!"
	_, err = svc.Crank(context.Background(), testMerchantID, row, 1)
	require.ErrorContains(t, err, "plan_pda")

	// Only the plan's owner pulls it: a plan another key owns is refused.
	_, err = svc.Crank(context.Background(), testMerchantID, crankRow(t, randKey(t).PublicKey()), 1)
	require.ErrorIs(t, err, ErrUnheldPlan)
	require.True(t, IsOperationalFailure(err), "an unheld plan is never the subscriber's failure")
	_, err = NewCrankService(NewSignerSubmitter(nil, nil)).Crank(context.Background(), testMerchantID, crankRow(t, cranker), 1)
	require.ErrorIs(t, err, ErrNoCranker)
}

type writeAheadSubmitter struct {
	crankSubmitter
	submitted bool
}

func (s *writeAheadSubmitter) SubmitWithPresubmit(_ context.Context, _ billing.MerchantID, _ solanago.PublicKey, ixs []solanago.Instruction, presubmit func(solanago.Signature) error) (solanago.Signature, error) {
	sig := solanago.Signature{7}
	if presubmit != nil {
		if err := presubmit(sig); err != nil {
			return solanago.Signature{}, err
		}
	}
	s.instructions, s.submitted = ixs, true
	return sig, nil
}

// A write-ahead-capable submitter is preferred, and the crank hands the
// caller's hook the signed signature (as recorded for crash recovery).
func TestCrankWriteAheadPrecedesSubmit(t *testing.T) {
	var recorded string
	cranker := randKey(t).PublicKey()
	sub := &writeAheadSubmitter{crankSubmitter: crankSubmitter{cranker: cranker}}
	sig, err := NewCrankService(sub).CrankWithPresubmit(context.Background(), testMerchantID, crankRow(t, cranker), 1, uuid.New(),
		func(s string) error { recorded = s; return nil })
	require.NoError(t, err)
	require.True(t, sub.submitted)
	require.Equal(t, sig, recorded)
}
