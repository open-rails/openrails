package subscriptions

import (
	"encoding/binary"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

// buildSubscriptionBlob assembles a synthetic SubscriptionDelegation account
// using the program's encoding (fixed-offset little-endian, header first) —
// the same layout subscribe.rs writes.
func buildSubscriptionBlob(t *testing.T, delegator, delegatee, payer solanago.PublicKey, initID int64, amount, periodHours uint64, createdAt int64, pulled uint64, periodStart, expiresAt int64) []byte {
	t.Helper()
	blob := make([]byte, 0, SubscriptionAccountSize)
	blob = append(blob, 4)    // discriminator (accountDiscriminator::subscriptionDelegation)
	blob = append(blob, 1)    // version
	blob = append(blob, 0xFD) // bump
	blob = append(blob, delegator.Bytes()...)
	blob = append(blob, delegatee.Bytes()...)
	blob = append(blob, payer.Bytes()...)
	blob = binary.LittleEndian.AppendUint64(blob, uint64(initID))
	blob = binary.LittleEndian.AppendUint64(blob, amount)
	blob = binary.LittleEndian.AppendUint64(blob, periodHours)
	blob = binary.LittleEndian.AppendUint64(blob, uint64(createdAt))
	blob = binary.LittleEndian.AppendUint64(blob, uint64(pulled))
	blob = binary.LittleEndian.AppendUint64(blob, uint64(periodStart))
	blob = binary.LittleEndian.AppendUint64(blob, uint64(expiresAt))
	require.Len(t, blob, SubscriptionAccountSize)
	return blob
}

func TestSubscriptionAccountSize(t *testing.T) {
	t.Parallel()
	// The program's frozen V1_LEN (state/subscription_delegation.rs).
	require.Equal(t, 155, SubscriptionAccountSize)
}

// TestSubscriptionAccountFilterAnchors pins the #714 getProgramAccounts
// memcmp anchors against the program encoding: discriminator at offset 0,
// delegatee (plan PDA) at offset 35.
func TestSubscriptionAccountFilterAnchors(t *testing.T) {
	t.Parallel()

	delegator := solanago.NewWallet().PublicKey()
	delegatee := solanago.NewWallet().PublicKey()
	blob := buildSubscriptionBlob(t, delegator, delegatee, solanago.NewWallet().PublicKey(),
		1, 1, 720, 1_700_000_000, 0, 0, 0)

	require.Equal(t, SubscriptionAccountDiscriminator, blob[0])
	off := SubscriptionAccountDelegateeOffset
	require.Equal(t, delegatee.Bytes(), blob[off:off+32])
}

func TestDecodeSubscriptionAccount(t *testing.T) {
	t.Parallel()

	delegator := solanago.NewWallet().PublicKey()
	delegatee := solanago.NewWallet().PublicKey()
	payer := solanago.NewWallet().PublicKey()

	blob := buildSubscriptionBlob(t, delegator, delegatee, payer,
		7, 5_000_000, 720, 1_700_000_000, 5_000_000, 1_750_000_000, 1_752_592_000)

	s, err := DecodeSubscriptionAccount(blob)
	require.NoError(t, err)
	require.Equal(t, byte(4), s.Discriminator)
	require.Equal(t, uint8(1), s.Version)
	require.Equal(t, uint8(0xFD), s.Bump)
	require.Equal(t, delegator, s.Delegator)
	require.Equal(t, delegatee, s.Delegatee)
	require.Equal(t, payer, s.Payer)
	require.Equal(t, int64(7), s.InitID)
	require.Equal(t, uint64(5_000_000), s.Amount)
	require.Equal(t, uint64(720), s.PeriodHours)
	require.Equal(t, int64(1_700_000_000), s.CreatedAt)
	require.Equal(t, uint64(5_000_000), s.AmountPulledInPeriod)
	require.Equal(t, int64(1_750_000_000), s.CurrentPeriodStartTs)
	require.Equal(t, int64(1_752_592_000), s.ExpiresAtTs)
	require.True(t, s.Cancelled())
}

func TestDecodeSubscriptionAccount_ActiveAndNegative(t *testing.T) {
	t.Parallel()

	blob := buildSubscriptionBlob(t, solanago.PublicKey{}, solanago.PublicKey{}, solanago.PublicKey{},
		-3, 1, 24, -1, 0, 0, 0)
	s, err := DecodeSubscriptionAccount(blob)
	require.NoError(t, err)
	require.Equal(t, int64(-3), s.InitID) // i64 fields round-trip signed
	require.Equal(t, int64(-1), s.CreatedAt)
	require.False(t, s.Cancelled()) // expiresAtTs == 0 => not cancelled
}

func TestDecodeSubscriptionAccount_TrailingBytesTolerated(t *testing.T) {
	t.Parallel()
	// Later schema versions append trailing bytes; the v1 prefix is frozen.
	blob := buildSubscriptionBlob(t, solanago.PublicKey{}, solanago.PublicKey{}, solanago.PublicKey{},
		1, 2, 3, 4, 5, 6, 7)
	blob = append(blob, 0xAA, 0xBB)
	s, err := DecodeSubscriptionAccount(blob)
	require.NoError(t, err)
	require.Equal(t, int64(7), s.ExpiresAtTs)
}

func TestDecodeSubscriptionAccount_Rejects(t *testing.T) {
	t.Parallel()

	_, err := DecodeSubscriptionAccount(make([]byte, SubscriptionAccountSize-1))
	require.ErrorContains(t, err, "too short")

	blob := buildSubscriptionBlob(t, solanago.PublicKey{}, solanago.PublicKey{}, solanago.PublicKey{},
		1, 2, 3, 4, 5, 6, 7)
	blob[0] = planAccountDiscriminator // a plan account is not a subscription
	_, err = DecodeSubscriptionAccount(blob)
	require.ErrorContains(t, err, "discriminator")
}

func TestParseInstructionKind(t *testing.T) {
	t.Parallel()

	// Pin every discriminator this package builds (IDL + program source).
	cases := map[byte]InstructionKind{
		0:  KindInitSubscriptionAuthority,
		3:  KindRevokeDelegation,
		7:  KindCreatePlan,
		8:  KindUpdatePlan,
		10: KindTransferSubscription,
		11: KindSubscribe,
		12: KindCancelSubscription,
		13: KindResumeSubscription,
	}
	for disc, want := range cases {
		kind, ok := ParseInstructionKind([]byte{disc})
		require.True(t, ok, "disc %d", disc)
		require.Equal(t, want, kind)
	}

	_, ok := ParseInstructionKind([]byte{99})
	require.False(t, ok)
	_, ok = ParseInstructionKind(nil)
	require.False(t, ok)
}

func TestDecodeTransferData_RoundTrip(t *testing.T) {
	t.Parallel()

	p := TransferSubscriptionParams{
		SubscriptionPDA:       solanago.NewWallet().PublicKey(),
		PlanPDA:               solanago.NewWallet().PublicKey(),
		SubscriptionAuthority: solanago.NewWallet().PublicKey(),
		DelegatorATA:          solanago.NewWallet().PublicKey(),
		ReceiverATA:           solanago.NewWallet().PublicKey(),
		Caller:                solanago.NewWallet().PublicKey(),
		Mint:                  solanago.NewWallet().PublicKey(),
		TokenProgram:          solanago.TokenProgramID,
		EventAuthority:        solanago.NewWallet().PublicKey(),
		Amount:                9_990_000,
		Delegator:             solanago.NewWallet().PublicKey(),
	}
	ix := BuildTransferSubscription(p)
	data, err := ix.Data()
	require.NoError(t, err)

	td, err := DecodeTransferData(data)
	require.NoError(t, err)
	require.Equal(t, uint64(9_990_000), td.Amount)
	require.Equal(t, p.Delegator, td.Delegator)
	require.Equal(t, p.Mint, td.Mint)

	kind, ok := ParseInstructionKind(data)
	require.True(t, ok)
	require.Equal(t, KindTransferSubscription, kind)
}

func TestDecodeTransferData_Rejects(t *testing.T) {
	t.Parallel()

	_, err := DecodeTransferData([]byte{discTransferSubscription, 1, 2})
	require.ErrorContains(t, err, "too short")

	_, err = DecodeTransferData(make([]byte, transferDataLen)) // disc 0 = init authority
	require.ErrorContains(t, err, "discriminator")
}
