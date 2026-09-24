package recurring

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

func trailingInitID(ix solanago.CompiledInstruction) int64 {
	return int64(binary.LittleEndian.Uint64(ix.Data[len(ix.Data)-8:]))
}

func newSubscribeSvc(t *testing.T, rpc chain) (*PrepareSubscribeService, solanago.PublicKey) {
	t.Helper()
	signer, submitter, merchantPub := newCranker(t)
	svc := NewPrepareSubscribeService(submitter, signer, rpc, "devnet", testTokens())
	svc.authorityReadBackoff = time.Nanosecond
	return svc, merchantPub
}

func subscribeInput(t *testing.T) PrepareSubscribeInput {
	return PrepareSubscribeInput{
		MerchantID:       testMerchantID,
		SubscriberWallet: randAddr(t),
		PlanID:           7,
		MintSymbol:       "USDC",
		AmountBaseUnits:  10_000_000,
		PeriodHours:      720,
		PlanCreatedAt:    1_700_000_000,
	}
}

// #286: one transaction, the co-signed [subscribe + first-period transfer]
// (cranker pre-signed, wallet completes), with initialize_subscription_authority
// folded in front for a first-timer using the UNKNOWN_INIT_ID sentinel. A
// Solana Pay reference rides a trailing tag and never changes program
// instruction account counts.
func TestPrepareSubscribeBundle(t *testing.T) {
	for _, tc := range []struct {
		name       string
		authority  *int64
		reference  bool
		wantShape  txShape
		wantInitID int64
	}{
		{"returning", withAuthority(42), false, txShape{2, 2, 1, []int{8, 10}}, 42},
		{"returning with reference", withAuthority(42), true, txShape{3, 2, 1, []int{8, 10}}, 42},
		{"first-timer", nil, false, txShape{3, 2, 1, []int{6, 8, 10}}, subscriptions.UnknownInitID},
		{"first-timer with reference", nil, true, txShape{4, 2, 1, []int{6, 8, 10}}, subscriptions.UnknownInitID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, merchantPub := newSubscribeSvc(t, chain{authorityInitID: tc.authority, balance: 50_000_000})
			in := subscribeInput(t)
			if tc.reference {
				in.Reference = randAddr(t)
			}
			res, err := svc.Prepare(context.Background(), in)
			require.NoError(t, err)
			require.Len(t, res.Transactions, 1)
			require.Equal(t, tc.authority != nil, res.AuthorityExists)

			tx := decodeTx(t, res.Transactions[0])
			require.Equal(t, tc.wantShape, shapeOf(t, tx))
			subscribeAt := 0
			if tc.authority == nil {
				require.Equal(t, byte(0), tx.Message.Instructions[0].Data[0], "instruction 0 must be initialize_subscription_authority")
				subscribeAt = 1
			}
			require.Equal(t, byte(11), tx.Message.Instructions[subscribeAt].Data[0], "subscribe discriminator")
			require.Equal(t, tc.wantInitID, trailingInitID(tx.Message.Instructions[subscribeAt]))
			if tc.reference {
				requireReferenceTag(t, tx, in.SubscriberWallet, in.Reference)
			}

			// The confirm step re-derives these; they must agree.
			planPDA, _, _ := subscriptions.DerivePlanPDA(merchantPub, in.PlanID)
			subPDA, _, _ := subscriptions.DeriveSubscriptionPDA(planPDA, solanago.MustPublicKeyFromBase58(in.SubscriberWallet))
			require.Equal(t, merchantPub.String(), res.MerchantAddress)
			require.Equal(t, planPDA.String(), res.PlanPDA)
			require.Equal(t, subPDA.String(), res.SubscriptionPDA)
			require.Equal(t, testDevnetUSDCMint, res.Mint)
		})
	}
}

// #286 pre-flight: an underfunded wallet gets a typed insufficient error before
// anything is built; a failed balance read fails OPEN (the atomic tx is the
// real guarantee).
func TestPrepareSubscribePreflight(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rpc       chain
		wantHave  uint64
		wantError bool
	}{
		{"returning underfunded", chain{authorityInitID: withAuthority(42), balance: 1_000_000}, 1_000_000, true},
		{"first-timer underfunded", chain{balance: 1_000_000}, 1_000_000, true},
		{"no token account", chain{authorityInitID: withAuthority(42)}, 0, true},
		{"balance read failure proceeds", chain{authorityInitID: withAuthority(42), balanceErr: errors.New("rpc blip")}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, _ := newSubscribeSvc(t, tc.rpc)
			res, err := svc.Prepare(context.Background(), subscribeInput(t))
			if !tc.wantError {
				require.NoError(t, err)
				require.Len(t, res.Transactions, 1)
				return
			}
			require.ErrorIs(t, err, ErrInsufficientUSDC)
			var ie *InsufficientUSDCError
			require.ErrorAs(t, err, &ie)
			require.Equal(t, InsufficientUSDCError{HaveBaseUnits: tc.wantHave, NeedBaseUnits: 10_000_000}, *ie)
		})
	}
}

func TestPrepareSubscribeRejectsBadInput(t *testing.T) {
	svc, _ := newSubscribeSvc(t, chain{authorityInitID: withAuthority(1), balance: 1 << 40})
	for name, mutate := range map[string]func(*PrepareSubscribeInput){
		"ineligible token": func(in *PrepareSubscribeInput) { in.MintSymbol = "PYUSD" },
		"zero amount":      func(in *PrepareSubscribeInput) { in.AmountBaseUnits = 0 },
		"zero period":      func(in *PrepareSubscribeInput) { in.PeriodHours = 0 },
		"no wallet":        func(in *PrepareSubscribeInput) { in.SubscriberWallet = "" },
		"bad reference":    func(in *PrepareSubscribeInput) { in.Reference = "not-base58!" },
	} {
		in := subscribeInput(t)
		mutate(&in)
		_, err := svc.Prepare(context.Background(), in)
		require.Error(t, err, name)
	}
}

func tierChangeInput(t *testing.T) PrepareTierChangeInput {
	return PrepareTierChangeInput{
		MerchantID:         testMerchantID,
		SubscriberWallet:   randAddr(t),
		MintSymbol:         "USDC",
		OldPlanPDA:         randAddr(t),
		OldSubscriptionPDA: randAddr(t),
		NewPlanID:          99,
		NewAmountBaseUnits: 50_000_000,
		NewPeriodHours:     720,
		NewPlanCreatedAt:   1_700_000_000,
	}
}

// #272: a tier change is ONE atomic tx. Upgrade = [cancel, subscribe, prorated
// transfer] co-signed by the cranker; downgrade = [cancel, subscribe] signed by
// the subscriber alone, no charge.
func TestPrepareTierChangeBundle(t *testing.T) {
	for _, tc := range []struct {
		name      string
		upgrade   bool
		reference bool
		want      txShape
	}{
		{"upgrade", true, false, txShape{3, 2, 1, []int{5, 8, 10}}},
		{"upgrade with reference", true, true, txShape{4, 2, 1, []int{5, 8, 10}}},
		{"downgrade", false, false, txShape{2, 1, 0, []int{5, 8}}},
		{"downgrade with reference", false, true, txShape{3, 1, 0, []int{5, 8}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signer, _, merchantPub := newCranker(t)
			svc := NewPrepareTierChangeService(signer, chain{authorityInitID: withAuthority(42), balance: 1 << 40}, "devnet", testTokens())
			in := tierChangeInput(t)
			in.IsUpgrade = tc.upgrade
			in.FirstChargeBaseUnits = 31_330_000
			if tc.reference {
				in.Reference = randAddr(t)
			}
			res, err := svc.Prepare(context.Background(), in)
			require.NoError(t, err)
			wantKind := "downgrade"
			if tc.upgrade {
				wantKind = "upgrade"
			}
			require.Equal(t, wantKind, res.Kind)

			tx := decodeTx(t, res.Transaction)
			require.Equal(t, tc.want, shapeOf(t, tx))
			require.Equal(t, int64(42), trailingInitID(tx.Message.Instructions[1]))
			if tc.reference {
				requireReferenceTag(t, tx, in.SubscriberWallet, in.Reference)
			}
			planPDA, _, _ := subscriptions.DerivePlanPDA(merchantPub, in.NewPlanID)
			subPDA, _, _ := subscriptions.DeriveSubscriptionPDA(planPDA, solanago.MustPublicKeyFromBase58(in.SubscriberWallet))
			require.Equal(t, subPDA.String(), res.NewSubscriptionPDA)
		})
	}
}

func TestPrepareTierChangeRefusals(t *testing.T) {
	signer, _, _ := newCranker(t)
	prepare := func(rpc chain, mutate func(*PrepareTierChangeInput)) error {
		in := tierChangeInput(t)
		in.IsUpgrade = true
		in.FirstChargeBaseUnits = 31_330_000
		mutate(&in)
		_, err := NewPrepareTierChangeService(signer, rpc, "devnet", testTokens()).Prepare(context.Background(), in)
		return err
	}
	funded := chain{authorityInitID: withAuthority(42), balance: 1 << 40}

	require.Error(t, prepare(funded, func(in *PrepareTierChangeInput) { in.FirstChargeBaseUnits = 0 }), "upgrade needs a prorated charge")
	require.Error(t, prepare(funded, func(in *PrepareTierChangeInput) { in.MintSymbol = "PYUSD" }))
	require.Error(t, prepare(funded, func(in *PrepareTierChangeInput) { in.OldPlanPDA = "bad" }))

	err := prepare(chain{authorityInitID: withAuthority(42), balance: 1_000}, func(*PrepareTierChangeInput) {})
	var ie *InsufficientUSDCError
	require.ErrorAs(t, err, &ie)
	require.Equal(t, InsufficientUSDCError{HaveBaseUnits: 1_000, NeedBaseUnits: 31_330_000}, *ie)

	require.NoError(t, prepare(chain{authorityInitID: withAuthority(42), balanceErr: errors.New("blip")}, func(*PrepareTierChangeInput) {}),
		"a failed balance read fails open")
}

type cancelRows struct{ row *models.SolanaSubscription }

func (c cancelRows) GetBySubscriptionID(context.Context, uuid.UUID) (*models.SolanaSubscription, error) {
	return c.row, nil
}

// #266: a per-subscription cancel_subscription (never a delegate Revoke, which
// would cancel every subscription on the mint), subscriber-signed and unsigned.
func TestPrepareCancel(t *testing.T) {
	row := &models.SolanaSubscription{
		SubscriberWallet: randAddr(t),
		PlanPDA:          randAddr(t),
		SubscriptionPDA:  randAddr(t),
		MerchantAddress:  randAddr(t),
	}
	svc := NewPrepareCancelService(cancelRows{row}, chain{})

	res, err := svc.Prepare(context.Background(), uuid.New())
	require.NoError(t, err)
	require.Equal(t, row.SubscriptionPDA, res.SubscriptionPDA)
	require.Equal(t, txShape{1, 1, 0, []int{5}}, shapeOf(t, decodeTx(t, res.Transaction)))

	reference := randAddr(t)
	res, err = svc.PrepareWithReference(context.Background(), uuid.New(), reference)
	require.NoError(t, err)
	tx := decodeTx(t, res.Transaction)
	require.Equal(t, txShape{2, 1, 0, []int{5}}, shapeOf(t, tx))
	requireReferenceTag(t, tx, row.SubscriberWallet, reference)

	_, err = svc.Prepare(context.Background(), uuid.Nil)
	require.Error(t, err)
	_, err = NewPrepareCancelService(cancelRows{}, chain{}).Prepare(context.Background(), uuid.New())
	require.Error(t, err, "no mirrored row")
}

type laggingAuthority struct {
	empties int
	data    []byte
	err     error
	calls   int
}

func (f *laggingAuthority) GetAccountData(context.Context, solanago.PublicKey) ([]byte, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.calls <= f.empties {
		return nil, nil
	}
	return f.data, nil
}

// #274: the authority read tolerates read-after-write lag: empties retry up to
// the bound and then mean first-timer; a present-but-short account never
// settles and errors; hard RPC errors and cancellation surface.
func TestReadAuthorityInitID(t *testing.T) {
	authority := func(id int64) []byte {
		b := make([]byte, subscriptionAuthorityInitIDOffset+8)
		binary.LittleEndian.PutUint64(b[subscriptionAuthorityInitIDOffset:], uint64(id))
		return b
	}
	rpcErr := errors.New("boom")
	for _, tc := range []struct {
		name      string
		rpc       *laggingAuthority
		wantID    int64
		wantExist bool
		wantErr   error
		wantCalls int
	}{
		{"present", &laggingAuthority{data: authority(7)}, 7, true, nil, 1},
		{"settles after lag", &laggingAuthority{empties: 3, data: authority(424242)}, 424242, true, nil, 4},
		{"never present is first-timer", &laggingAuthority{empties: 1000}, 0, false, nil, authorityReadMaxAttempts},
		{"short account never settles", &laggingAuthority{data: make([]byte, subscriptionAuthorityInitIDOffset)}, 0, false, errAny, authorityReadMaxAttempts},
		{"rpc error surfaces", &laggingAuthority{err: rpcErr}, 0, false, rpcErr, 1},
	} {
		id, exists, err := readAuthorityInitIDWithBackoff(context.Background(), time.Nanosecond, tc.rpc, solanago.PublicKey{})
		switch tc.wantErr {
		case nil:
			require.NoError(t, err, tc.name)
		case errAny:
			require.Error(t, err, tc.name)
		default:
			require.ErrorIs(t, err, tc.wantErr, tc.name)
		}
		require.Equal(t, tc.wantID, id, tc.name)
		require.Equal(t, tc.wantExist, exists, tc.name)
		require.Equal(t, tc.wantCalls, tc.rpc.calls, tc.name)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := readAuthorityInitIDWithBackoff(ctx, time.Hour, &laggingAuthority{empties: 1000}, solanago.PublicKey{})
	require.ErrorIs(t, err, context.Canceled)
}

var errAny = errors.New("any error")
