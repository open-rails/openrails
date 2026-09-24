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

// addressSubmitter records the merchant address the crank signs as. Its
// MerchantAddress deliberately differs from the row's recorded address.
type addressSubmitter struct {
	address      solanago.PublicKey
	instructions []solanago.Instruction
}

func (*addressSubmitter) MerchantAddress(context.Context, merchant.ID) (solanago.PublicKey, error) {
	return solanago.PublicKey{}, nil
}

func (*addressSubmitter) Submit(context.Context, merchant.ID, []solanago.Instruction) (solanago.Signature, error) {
	panic("crank must sign as the row's recorded merchant address")
}

func (s *addressSubmitter) SubmitForMerchantAddress(_ context.Context, _ merchant.ID, address solanago.PublicKey, ixs []solanago.Instruction) (solanago.Signature, error) {
	s.address, s.instructions = address, ixs
	return solanago.Signature{}, nil
}

func crankRow(t *testing.T) *models.SolanaSubscription {
	return &models.SolanaSubscription{
		MerchantAddress:  randAddr(t),
		Mint:             randAddr(t),
		SubscriberWallet: randAddr(t),
		PlanPDA:          randAddr(t),
		SubscriptionPDA:  randAddr(t),
		AuthorityPDA:     randAddr(t),
	}
}

// The pull signs as the RECORDED merchant address and, with a pull-intent id,
// is stamped [SPL Memo "openrails:1:<id>", transfer_subscription] — memo first
// (#713). Without an id it is the bare transfer.
func TestCrankPullShape(t *testing.T) {
	intentID := uuid.MustParse("6b1f3c2d-9a8e-4b7c-8d5f-0e1a2b3c4d5e")
	for _, memoID := range []uuid.UUID{intentID, uuid.Nil} {
		row := crankRow(t)
		sub := &addressSubmitter{}
		_, err := NewCrankService(sub).CrankWithPresubmit(context.Background(), testMerchantID, row, 10_000_000, memoID, nil)
		require.NoError(t, err)
		require.Equal(t, row.MerchantAddress, sub.address.String())

		transfer := sub.instructions[len(sub.instructions)-1]
		require.True(t, transfer.ProgramID().Equals(subscriptions.ProgramID))
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
	svc := NewCrankService(&addressSubmitter{})
	_, err := svc.Crank(context.Background(), testMerchantID, nil, 1)
	require.Error(t, err)
	_, err = svc.Crank(context.Background(), testMerchantID, crankRow(t), 0)
	require.Error(t, err, "a zero pull")
	row := crankRow(t)
	row.PlanPDA = "not-base58!"
	_, err = svc.Crank(context.Background(), testMerchantID, row, 1)
	require.ErrorContains(t, err, "plan_pda")
}

type writeAheadSubmitter struct {
	addressSubmitter
	submitted bool
}

func (s *writeAheadSubmitter) SubmitForMerchantAddressWithPresubmit(_ context.Context, _ merchant.ID, address solanago.PublicKey, ixs []solanago.Instruction, presubmit func(solanago.Signature) error) (solanago.Signature, error) {
	sig := solanago.Signature{7}
	if presubmit != nil {
		if err := presubmit(sig); err != nil {
			return solanago.Signature{}, err
		}
	}
	s.address, s.instructions, s.submitted = address, ixs, true
	return sig, nil
}

// #674: a write-ahead-capable submitter is preferred, and the crank hands the
// caller's hook the signed signature (as recorded for crash recovery).
func TestCrankWriteAheadPrecedesSubmit(t *testing.T) {
	var recorded string
	sub := &writeAheadSubmitter{}
	sig, err := NewCrankService(sub).CrankWithPresubmit(context.Background(), testMerchantID, crankRow(t), 1, uuid.New(),
		func(s string) error { recorded = s; return nil })
	require.NoError(t, err)
	require.True(t, sub.submitted)
	require.Equal(t, sig, recorded)
}
