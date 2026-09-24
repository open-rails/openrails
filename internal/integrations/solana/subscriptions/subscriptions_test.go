package subscriptions

import (
	"encoding/binary"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func key() solanago.PublicKey { return solanago.NewWallet().PublicKey() }

func le64(v int64) []byte { return binary.LittleEndian.AppendUint64(nil, uint64(v)) }

// Plan account: native program, u8 discriminator, fixed-offset LE fields.
// Distinct values so a transposed offset surfaces as a wrong field.
func TestDecodePlanAccountByteLayout(t *testing.T) {
	owner, mint := key(), key()
	var dests, pullers [4]solanago.PublicKey
	blob := append([]byte{planAccountDiscriminator}, owner.Bytes()...)
	blob = append(blob, 254, PlanStatusActive)
	blob = append(blob, le64(123456)...)
	blob = append(blob, mint.Bytes()...)
	for _, v := range []int64{990000, 720, 1717200000, -5} {
		blob = append(blob, le64(v)...)
	}
	for i := range 4 {
		dests[i], pullers[i] = key(), key()
		blob = append(blob, dests[i].Bytes()...)
	}
	for i := range 4 {
		blob = append(blob, pullers[i].Bytes()...)
	}
	uri := make([]byte, metadataURILen)
	copy(uri, "ipfs://plan-meta")
	blob = append(blob, uri...)
	require.Len(t, blob, PlanAccountSize)

	p, err := DecodePlanAccount(append(blob, 0xAA))
	require.NoError(t, err, "trailing bytes tolerated")
	require.Equal(t, PlanAccount{
		Discriminator: planAccountDiscriminator, Owner: owner, Bump: 254, Status: PlanStatusActive,
		PlanID: 123456, Mint: mint, Amount: 990000, PeriodHours: 720, CreatedAt: 1717200000, EndTs: -5,
		Destinations: dests, Pullers: pullers, MetadataURI: "ipfs://plan-meta",
	}, *p)

	_, err = DecodePlanAccount(blob[:PlanAccountSize-1])
	require.Error(t, err)
	blob[0] = subscriptionAccountDiscriminator
	_, err = DecodePlanAccount(blob)
	require.Error(t, err)
}

// SubscriptionDelegation v1 (frozen V1_LEN=155); #714 memcmp anchors are
// discriminator @0 and delegatee (plan PDA) @35.
func TestDecodeSubscriptionAccountByteLayout(t *testing.T) {
	require.Equal(t, 155, SubscriptionAccountSize)
	delegator, delegatee, payer := key(), key(), key()
	build := func(expiresAt int64) []byte {
		b := []byte{subscriptionAccountDiscriminator, 1, 0xFD}
		b = append(b, delegator.Bytes()...)
		b = append(b, delegatee.Bytes()...)
		b = append(b, payer.Bytes()...)
		for _, v := range []int64{-3, 5_000_000, 720, 1_700_000_000, 4_000_000, 1_750_000_000, expiresAt} {
			b = append(b, le64(v)...)
		}
		return b
	}
	blob := build(1_752_592_000)
	require.Len(t, blob, SubscriptionAccountSize)
	require.Equal(t, SubscriptionAccountDiscriminator, blob[0])
	off := SubscriptionAccountDelegateeOffset
	require.Equal(t, delegatee.Bytes(), blob[off:off+32])

	s, err := DecodeSubscriptionAccount(append(blob, 0xAA, 0xBB))
	require.NoError(t, err, "later versions append bytes")
	require.Equal(t, SubscriptionAccount{
		Discriminator: 4, Version: 1, Bump: 0xFD, Delegator: delegator, Delegatee: delegatee, Payer: payer,
		InitID: -3, Amount: 5_000_000, PeriodHours: 720, CreatedAt: 1_700_000_000,
		AmountPulledInPeriod: 4_000_000, CurrentPeriodStartTs: 1_750_000_000, ExpiresAtTs: 1_752_592_000,
	}, *s)
	require.True(t, s.Cancelled())

	s, err = DecodeSubscriptionAccount(build(0))
	require.NoError(t, err)
	require.False(t, s.Cancelled(), "expiresAt 0 = active")

	_, err = DecodeSubscriptionAccount(blob[:SubscriptionAccountSize-1])
	require.ErrorContains(t, err, "too short")
	blob[0] = planAccountDiscriminator
	_, err = DecodeSubscriptionAccount(blob)
	require.ErrorContains(t, err, "discriminator")
}

func TestPDADerivations(t *testing.T) {
	user, mint := key(), key()
	sa1, b1, err := DeriveSubscriptionAuthority(user, mint)
	require.NoError(t, err)
	sa2, b2, err := DeriveSubscriptionAuthority(user, mint)
	require.NoError(t, err)
	require.Equal(t, sa1, sa2)
	require.Equal(t, b1, b2)
	require.False(t, solanago.IsOnCurve(sa1.Bytes()))

	plan42, _, err := DerivePlanPDA(user, 42)
	require.NoError(t, err)
	plan43, _, err := DerivePlanPDA(user, 43)
	require.NoError(t, err)
	require.NotEqual(t, plan42, plan43)
	sub, _, err := DeriveSubscriptionPDA(plan42, user)
	require.NoError(t, err)
	require.NotEqual(t, plan42, sub)

	// Devnet-verified fixed event authority (seed "event_authority", not Anchor's "__event_authority").
	ea, _, err := DeriveEventAuthority()
	require.NoError(t, err)
	require.Equal(t, "3Hnj4BYoDgtpBuqXfiy7Y8cNa3jXaNd4oqgSXBzkMcH7", ea.String())

	ata, _, err := DeriveATA(user, mint, solanago.TokenProgramID)
	require.NoError(t, err)
	want, _, err := solanago.FindAssociatedTokenAddress(user, mint)
	require.NoError(t, err)
	require.Equal(t, want, ata)
}

// Instruction account metas (order, signer, writable) and discriminators; a
// wrong meta is an on-chain rejection or, worse, a pull signed by the wrong key.
func TestInstructionAccountLayouts(t *testing.T) {
	createPlan, err := BuildCreatePlan(CreatePlanParams{Merchant: key(), PlanPDA: key(), Mint: key(), TokenProgram: solanago.TokenProgramID})
	require.NoError(t, err)
	for _, c := range []struct {
		name     string
		ix       solanago.Instruction
		program  solanago.PublicKey
		disc     byte
		n        int
		signers  []int
		writable []int
	}{
		{"create_plan", createPlan, ProgramID, discCreatePlan, 5, []int{0}, []int{0, 1}},
		{"transfer_subscription", BuildTransferSubscription(TransferSubscriptionParams{SubscriptionPDA: key(), Caller: key()}), ProgramID, discTransferSubscription, 10, []int{5}, []int{0, 3, 4}},
		{"subscribe", BuildSubscribe(SubscribeParams{Subscriber: key()}), ProgramID, discSubscribe, 8, []int{0}, []int{0, 3}},
		{"init_authority", BuildInitSubscriptionAuthority(InitSubscriptionAuthorityParams{Owner: key()}), ProgramID, discInitSubscriptionAuthority, 6, []int{0}, []int{0, 1, 3}},
		{"cancel", BuildCancelSubscription(CancelOrResumeParams{Subscriber: key()}), ProgramID, discCancelSubscription, 5, []int{0}, []int{2}},
		{"revoke_delegation", BuildRevokeDelegation(key(), key(), key()), ProgramID, discRevokeDelegation, 3, []int{0}, []int{0, 1}},
		{"ata_create_idempotent", BuildCreateIdempotentATA(CreateIdempotentATAParams{Payer: key(), TokenProgram: solanago.TokenProgramID}), AssociatedTokenProgramID, discATACreateIdempotent, 6, []int{0}, []int{0, 1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.program, c.ix.ProgramID())
			data, err := c.ix.Data()
			require.NoError(t, err)
			require.Equal(t, c.disc, data[0])
			accs := c.ix.Accounts()
			require.Len(t, accs, c.n)
			for i, a := range accs {
				require.Equal(t, contains(c.signers, i), a.IsSigner, "signer %d", i)
				require.Equal(t, contains(c.writable, i), a.IsWritable, "writable %d", i)
			}
		})
	}

	owner, mint := key(), key()
	ata, _, _ := DeriveATA(owner, mint, solanago.TokenProgramID)
	accs := BuildCreateIdempotentATA(CreateIdempotentATAParams{Payer: key(), ATA: ata, Owner: owner, Mint: mint, TokenProgram: solanago.TokenProgramID}).Accounts()
	require.Equal(t, []solanago.PublicKey{ata, owner, mint, solanago.SystemProgramID, solanago.TokenProgramID},
		[]solanago.PublicKey{accs[1].PublicKey, accs[2].PublicKey, accs[3].PublicKey, accs[4].PublicKey, accs[5].PublicKey})
	plan := key()
	require.Equal(t, plan, BuildRevokeDelegation(key(), key(), plan).Accounts()[2].PublicKey, "trailing plan_pda or Custom:113")
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Instruction data encodings match the IDL byte for byte, and decoding is the
// exact inverse of the pull builder.
func TestInstructionDataEncoding(t *testing.T) {
	mint, pullers := key(), [4]solanago.PublicKey{key()}
	create, err := encodeCreatePlan(CreatePlanParams{Mint: mint, PlanID: 1, Terms: PlanTerms{Amount: 10_000_000, PeriodHours: 720, CreatedAt: -1}, EndTs: 9, Pullers: pullers, MetadataURI: "https://x/p.json"})
	require.NoError(t, err)
	require.Len(t, create, 1+8+32+24+8+4*32+4*32+128)
	require.Equal(t, uint64(1), binary.LittleEndian.Uint64(create[1:9]))
	require.Equal(t, mint.Bytes(), create[9:41])
	require.Equal(t, uint64(10_000_000), binary.LittleEndian.Uint64(create[41:49]))
	require.Equal(t, le64(-1), create[57:65])
	require.Equal(t, le64(9), create[65:73])
	require.Equal(t, pullers[0].Bytes(), create[73+128:73+160])
	require.Equal(t, "https://x/p.json", string(create[73+256:73+256+16]))

	update, err := BuildUpdatePlan(UpdatePlanParams{Owner: key(), PlanPDA: key(), Status: PlanStatusSunset, EndTs: -7, Pullers: pullers, MetadataURI: "ipfs://meta"})
	require.NoError(t, err)
	accs := update.Accounts()
	require.True(t, accs[0].IsSigner && !accs[0].IsWritable, "owner signs, read-only")
	require.True(t, !accs[1].IsSigner && accs[1].IsWritable, "plan writable")
	data, err := update.Data()
	require.NoError(t, err)
	require.Len(t, data, 1+1+8+4*32+128)
	require.Equal(t, []byte{discUpdatePlan, PlanStatusSunset}, data[:2])
	require.Equal(t, le64(-7), data[2:10])
	require.Equal(t, pullers[0].Bytes(), data[10:42])
	require.Equal(t, append([]byte("ipfs://meta"), make([]byte, 128-11)...), data[138:], "zero-padded uri")

	long := string(make([]byte, 129))
	_, err = BuildUpdatePlan(UpdatePlanParams{MetadataURI: long})
	require.Error(t, err)
	_, err = BuildCreatePlan(CreatePlanParams{MetadataURI: long})
	require.Error(t, err)

	sub, err := BuildSubscribe(SubscribeParams{PlanID: 3, PlanBump: 7, ExpectedMint: mint, ExpectedAmount: 99, ExpectedPeriodHours: 24, ExpectedCreatedAt: 5, ExpectedSubscriptionAuthInitID: UnknownInitID}).Data()
	require.NoError(t, err)
	require.Len(t, sub, 1+8+1+32+8+8+8+8)
	require.Equal(t, byte(7), sub[9])
	require.Equal(t, mint.Bytes(), sub[10:42])
	require.Equal(t, uint64(99), binary.LittleEndian.Uint64(sub[42:50]))
	require.Equal(t, le64(UnknownInitID), sub[66:74])

	delegator := key()
	pull, err := BuildTransferSubscription(TransferSubscriptionParams{Amount: 9_990_000, Delegator: delegator, Mint: mint}).Data()
	require.NoError(t, err)
	td, err := DecodeTransferData(pull)
	require.NoError(t, err)
	require.Equal(t, TransferData{Amount: 9_990_000, Delegator: delegator, Mint: mint}, *td)
	_, err = DecodeTransferData(pull[:3])
	require.ErrorContains(t, err, "too short")
	_, err = DecodeTransferData(make([]byte, transferDataLen))
	require.ErrorContains(t, err, "discriminator")
}

func TestParseInstructionKind(t *testing.T) {
	for disc, want := range map[byte]InstructionKind{
		0: KindInitSubscriptionAuthority, 3: KindRevokeDelegation, 7: KindCreatePlan, 8: KindUpdatePlan,
		10: KindTransferSubscription, 11: KindSubscribe, 12: KindCancelSubscription, 13: KindResumeSubscription,
	} {
		got, ok := ParseInstructionKind([]byte{disc})
		require.True(t, ok, disc)
		require.Equal(t, want, got)
	}
	for _, data := range [][]byte{nil, {99}} {
		_, ok := ParseInstructionKind(data)
		require.False(t, ok)
	}
}
