package recurring

import (
	"context"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

type crankAddressSubmitter struct {
	address      solanago.PublicKey
	called       bool
	instructions []solanago.Instruction
}

func (s *crankAddressSubmitter) MerchantAddress(context.Context, merchant.ID) (solanago.PublicKey, error) {
	return solanago.PublicKey{}, nil
}

func (s *crankAddressSubmitter) Submit(context.Context, merchant.ID, []solanago.Instruction) (solanago.Signature, error) {
	return solanago.Signature{}, nil
}

func (s *crankAddressSubmitter) SubmitForMerchantAddress(_ context.Context, _ merchant.ID, address solanago.PublicKey, instructions []solanago.Instruction) (solanago.Signature, error) {
	s.called = true
	s.address = address
	s.instructions = instructions
	return solanago.Signature{}, nil
}

func TestCrankUsesRecordedMerchantAddressSubmitter(t *testing.T) {
	merchantKey, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	mint, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	subscriber, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	plan, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	subscription, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	authority, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)

	submitter := &crankAddressSubmitter{}
	_, err = NewCrankService(submitter).Crank(context.Background(), merchant.ID(uuid.New()), &models.SolanaSubscription{
		MerchantAddress:  merchantKey.PublicKey().String(),
		Mint:             mint.PublicKey().String(),
		SubscriberWallet: subscriber.PublicKey().String(),
		PlanPDA:          plan.PublicKey().String(),
		SubscriptionPDA:  subscription.PublicKey().String(),
		AuthorityPDA:     authority.PublicKey().String(),
	}, 1)
	require.NoError(t, err)
	require.True(t, submitter.called)
	require.True(t, submitter.address.Equals(merchantKey.PublicKey()))
}

// TestCrankStampsPurchaseMemoBeforeTransfer wire-pins the #713 pull stamp:
// with a memo local-id the submitted tx is [SPL Memo, transfer_subscription]
// — memo FIRST, exact bytes — and without one (legacy Crank) it is just the
// transfer.
func TestCrankStampsPurchaseMemoBeforeTransfer(t *testing.T) {
	newSub := func(t *testing.T) *models.SolanaSubscription {
		t.Helper()
		key := func() string {
			k, err := solanago.NewRandomPrivateKey()
			require.NoError(t, err)
			return k.PublicKey().String()
		}
		return &models.SolanaSubscription{
			MerchantAddress:  key(),
			Mint:             key(),
			SubscriberWallet: key(),
			PlanPDA:          key(),
			SubscriptionPDA:  key(),
			AuthorityPDA:     key(),
		}
	}

	t.Run("stamped pull", func(t *testing.T) {
		submitter := &crankAddressSubmitter{}
		localID := uuid.MustParse("6b1f3c2d-9a8e-4b7c-8d5f-0e1a2b3c4d5e")
		_, err := NewCrankService(submitter).CrankWithPresubmit(
			context.Background(), merchant.ID(uuid.New()), newSub(t), 1, localID, nil)
		require.NoError(t, err)
		require.Len(t, submitter.instructions, 2)

		memoIx := submitter.instructions[0]
		require.Equal(t, "MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr", memoIx.ProgramID().String())
		data, err := memoIx.Data()
		require.NoError(t, err)
		require.Equal(t, []byte("openrails:1:6b1f3c2d-9a8e-4b7c-8d5f-0e1a2b3c4d5e"), data)
		require.Empty(t, memoIx.Accounts())

		require.Equal(t, subscriptions.ProgramID, submitter.instructions[1].ProgramID())
	})

	t.Run("legacy unstamped pull", func(t *testing.T) {
		submitter := &crankAddressSubmitter{}
		_, err := NewCrankService(submitter).Crank(
			context.Background(), merchant.ID(uuid.New()), newSub(t), 1)
		require.NoError(t, err)
		require.Len(t, submitter.instructions, 1)
		require.Equal(t, subscriptions.ProgramID, submitter.instructions[0].ProgramID())
	})
}
