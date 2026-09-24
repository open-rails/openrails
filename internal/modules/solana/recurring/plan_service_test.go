package recurring

import (
	"context"
	"encoding/binary"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"

	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// testPlanCreatedAt is the cluster clock the fake chain stamps on create_plan;
// it differs from the service's own clock on purpose (#254 Custom:519).
const testPlanCreatedAt = int64(1_717_200_000)

type recordingSubmitter struct {
	merchantPub solanago.PublicKey
	submits     [][]solanago.Instruction
}

func (s *recordingSubmitter) MerchantAddress(context.Context, merchant.ID) (solanago.PublicKey, error) {
	return s.merchantPub, nil
}

func (s *recordingSubmitter) Submit(_ context.Context, _ merchant.ID, ins []solanago.Instruction) (solanago.Signature, error) {
	s.submits = append(s.submits, ins)
	return solanago.Signature{byte(len(s.submits))}, nil
}

// planChain serves the mint account (decimals) and the plan PDA: `existing`
// before any submit, then a plan with `created` terms once create_plan is sent.
type planChain struct {
	mintDecimals int // -1: mint account absent
	existing     []byte
	created      *[2]uint64 // amount, period
	sub          *recordingSubmitter
}

func (c planChain) GetAccountData(_ context.Context, addr solanago.PublicKey) ([]byte, error) {
	if addr.String() == testDevnetUSDCMint {
		if c.mintDecimals < 0 {
			return nil, nil
		}
		blob := make([]byte, solanaint.MintAccountSize)
		blob[44] = byte(c.mintDecimals)
		blob[45] = 1
		return blob, nil
	}
	if c.created != nil && len(c.sub.submits) > 0 {
		return planBlob(c.sub.merchantPub, solanago.MustPublicKeyFromBase58(testDevnetUSDCMint), c.created[0], c.created[1], testPlanCreatedAt), nil
	}
	return c.existing, nil
}

// planBlob lays out an on-chain Plan account in DecodePlanAccount's byte order.
func planBlob(owner, mint solanago.PublicKey, amount, periodHours uint64, createdAt int64) []byte {
	blob := make([]byte, 0, subscriptions.PlanAccountSize)
	u64 := func(v uint64) { blob = binary.LittleEndian.AppendUint64(blob, v) }
	blob = append(blob, 1) // discriminator
	blob = append(blob, owner.Bytes()...)
	blob = append(blob, 254, 0) // bump, status
	u64(7)
	blob = append(blob, mint.Bytes()...)
	u64(amount)
	u64(periodHours)
	u64(uint64(createdAt))
	u64(0)                                     // end_ts
	blob = append(blob, make([]byte, 8*32)...) // destinations + pullers
	return append(blob, make([]byte, 128)...)  // metadata uri
}

func newPlanService(t *testing.T, c planChain) (*PlanService, *recordingSubmitter) {
	t.Helper()
	sub := &recordingSubmitter{merchantPub: randKey(t).PublicKey()}
	c.sub = sub
	return NewPlanServiceWithReader(sub, c, "devnet", testTokens()), sub
}

func planInput(amount uint64, decimals int) PublishPlanInput {
	return PublishPlanInput{PlanID: 7, TokenSymbol: "usdc", AmountBaseUnits: amount, AmountDecimals: decimals, PeriodHours: 720}
}

// ensuredATAOwners returns the owners of every CreateIdempotent ATA instruction
// submitted, asserting each is paid by the cranker.
func ensuredATAOwners(t *testing.T, sub *recordingSubmitter) []solanago.PublicKey {
	t.Helper()
	var owners []solanago.PublicKey
	for _, set := range sub.submits {
		for _, ix := range set {
			if !ix.ProgramID().Equals(subscriptions.AssociatedTokenProgramID) {
				continue
			}
			accs := ix.Accounts()
			data, _ := ix.Data()
			require.Equal(t, []byte{1}, data, "CreateIdempotent")
			require.True(t, accs[0].PublicKey.Equals(sub.merchantPub) && accs[0].IsSigner && accs[0].IsWritable, "cranker pays")
			wantATA, _, _ := subscriptions.DeriveATA(accs[2].PublicKey, accs[3].PublicKey, solanago.TokenProgramID)
			require.True(t, accs[1].PublicKey.Equals(wantATA))
			require.Equal(t, testDevnetUSDCMint, accs[3].PublicKey.String())
			owners = append(owners, accs[2].PublicKey)
		}
	}
	return owners
}

// A fresh publish submits create_plan, provisions every receiving ATA (a pull
// into a missing ATA reverts), and returns the CHAIN's created_at, which
// subscribe must echo exactly.
func TestPublishPlanCreatesPlanAndReceivingATAs(t *testing.T) {
	amount := [2]uint64{10_000_000, 720}
	cold := randKey(t).PublicKey()
	for _, receiving := range []string{"", cold.String()} {
		svc, sub := newPlanService(t, planChain{mintDecimals: 6, created: &amount})
		in := planInput(10_000_000, 6)
		in.ReceivingWallet = receiving

		h, err := svc.PublishPlan(context.Background(), in)
		require.NoError(t, err)
		planPDA, _, _ := subscriptions.DerivePlanPDA(sub.merchantPub, 7)
		require.Equal(t, planPDA.String(), h.PlanPDA)
		require.Equal(t, testPlanCreatedAt, h.CreatedAt)
		require.Equal(t, uint64(10_000_000), h.AmountBaseUnits)
		require.Equal(t, "USDC", h.MintSymbol)
		require.NotEmpty(t, h.Signature)
		require.True(t, sub.submits[0][0].ProgramID().Equals(subscriptions.ProgramID), "create_plan first")

		want := []solanago.PublicKey{sub.merchantPub}
		if receiving != "" {
			want = append(want, cold)
		}
		require.Equal(t, want, ensuredATAOwners(t, sub))
	}
}

// #817: the plan amount is immutable on-chain, so the caller's decimals must
// equal the mint's on-chain decimals; nothing is submitted otherwise.
func TestPublishPlanRequiresOnChainDecimals(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mintDecimals  int
		amount        uint64
		inputDecimals int
		ok            bool
	}{
		{"6 decimals", 6, 10_000_000, 6, true},
		{"9 decimals", 9, 10_000_000_000, 9, true},
		{"8 decimals", 8, 1_000_000_000, 8, true},
		{"converted at 6 for a 9-decimal mint", 9, 10_000_000, 6, false},
		{"omitted decimals", 6, 10_000_000, 0, false},
		{"mint unreadable", -1, 10_000_000, 6, false},
		{"unpayable 0-decimal mint", 0, 10, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created := [2]uint64{tc.amount, 720}
			svc, sub := newPlanService(t, planChain{mintDecimals: tc.mintDecimals, created: &created})
			h, err := svc.PublishPlan(context.Background(), planInput(tc.amount, tc.inputDecimals))
			if !tc.ok {
				require.Error(t, err)
				require.Empty(t, sub.submits)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.amount, h.AmountBaseUnits)
		})
	}

	svc, _ := newPlanService(t, planChain{mintDecimals: 9})
	_, err := svc.PublishPlan(context.Background(), planInput(10_000_000, 6))
	require.ErrorContains(t, err, "9 on-chain")

	d, err := svc.MintDecimals(context.Background(), "USDC")
	require.NoError(t, err)
	require.Equal(t, 9, d)
	_, err = NewPlanServiceWithReader(&recordingSubmitter{}, nil, "devnet", testTokens()).MintDecimals(context.Background(), "USDC")
	require.Error(t, err, "no reader armed has no fallback precision")
}

// #254: an occupied plan PDA with matching terms is an idempotent no-op; any
// differing immutable term is refused (publish a new plan_id instead).
func TestPublishPlanRepublish(t *testing.T) {
	mint := solanago.MustPublicKeyFromBase58(testDevnetUSDCMint)
	other := randKey(t).PublicKey()
	for _, tc := range []struct {
		name          string
		mint          solanago.PublicKey
		amount, hours uint64
		idempotent    bool
	}{
		{"same terms", mint, 10_000_000, 720, true},
		{"different amount", mint, 5_000_000, 720, false},
		{"different period", mint, 10_000_000, 744, false},
		{"different mint", other, 10_000_000, 720, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := &recordingSubmitter{merchantPub: randKey(t).PublicKey()}
			chain := planChain{mintDecimals: 6, existing: planBlob(sub.merchantPub, tc.mint, tc.amount, tc.hours, 1_600_000_000), sub: sub}
			h, err := NewPlanServiceWithReader(sub, chain, "devnet", testTokens()).PublishPlan(context.Background(), planInput(10_000_000, 6))
			require.Empty(t, sub.submits)
			if !tc.idempotent {
				require.ErrorContains(t, err, "IMMUTABLE")
				return
			}
			require.NoError(t, err)
			require.Equal(t, int64(1_600_000_000), h.CreatedAt, "echo the existing on-chain created_at")
			require.Empty(t, h.Signature)
		})
	}
}

func TestPublishPlanValidatesTermsBeforeSubmit(t *testing.T) {
	for name, mutate := range map[string]func(*PublishPlanInput){
		"zero amount":              func(in *PublishPlanInput) { in.AmountBaseUnits = 0 },
		"zero period":              func(in *PublishPlanInput) { in.PeriodHours = 0 },
		"period over a year":       func(in *PublishPlanInput) { in.PeriodHours = maxPeriodHours + 1 },
		"billing cycle mismatch":   func(in *PublishPlanInput) { in.BillingCycleHours = 744 },
		"ineligible token":         func(in *PublishPlanInput) { in.TokenSymbol = "PYUSD" },
		"invalid receiving wallet": func(in *PublishPlanInput) { in.ReceivingWallet = "not-base58!" },
	} {
		svc, sub := newPlanService(t, planChain{mintDecimals: 6})
		in := planInput(10_000_000, 6)
		mutate(&in)
		_, err := svc.PublishPlan(context.Background(), in)
		require.Error(t, err, name)
		require.Empty(t, sub.submits, name)
	}
}

// #358: sunset only a plan this merchant owns, echoing its mutable fields.
func TestSunsetPlanOwnership(t *testing.T) {
	svc, sub := newPlanService(t, planChain{mintDecimals: 6})
	planPDA := randKey(t).PublicKey()

	_, err := svc.SunsetPlan(context.Background(), testMerchantID, planPDA, &subscriptions.PlanAccount{Owner: randKey(t).PublicKey()})
	require.ErrorIs(t, err, ErrPlanSunsetNotOwned)
	_, err = svc.SunsetPlan(context.Background(), testMerchantID, planPDA, nil)
	require.Error(t, err)
	require.Empty(t, sub.submits)

	sig, err := svc.SunsetPlan(context.Background(), testMerchantID, planPDA, &subscriptions.PlanAccount{Owner: sub.merchantPub})
	require.NoError(t, err)
	require.NotEmpty(t, sig)
	require.Len(t, sub.submits, 1)
}
