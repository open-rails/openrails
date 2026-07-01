package recurring

import (
	"context"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
)

type crankAddressSubmitter struct {
	address solanago.PublicKey
	called  bool
}

func (s *crankAddressSubmitter) MerchantAddress(context.Context, merchant.ID) (solanago.PublicKey, error) {
	return solanago.PublicKey{}, nil
}

func (s *crankAddressSubmitter) Submit(context.Context, merchant.ID, []solanago.Instruction) (solanago.Signature, error) {
	return solanago.Signature{}, nil
}

func (s *crankAddressSubmitter) SubmitForMerchantAddress(_ context.Context, _ merchant.ID, address solanago.PublicKey, _ []solanago.Instruction) (solanago.Signature, error) {
	s.called = true
	s.address = address
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
