// Package subscriptions builds instructions for the Solana Subscriptions
// Delegation Program (De1egAFMkMWZSN5rYXRj9CAdheBamobVNubTsi9avR44) — PDA
// derivation, instruction account layouts, and fixed-layout arg encoding.
//
// Layouts come from the program's generated IDL (idl/subscriptions.json):
// discriminators are a single leading u8; arg structs are fixed-size
// little-endian. The on-chain account orderings and the __event_authority
// derivation are DEVNET-VERIFY items (see issue #252/#254 spike) — they are
// isolated here behind named constants so a correction is a one-line change.
package subscriptions

import (
	"bytes"
	"encoding/binary"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
)

// ProgramID is the Subscriptions Delegation Program address.
var ProgramID = solanago.MustPublicKeyFromBase58("De1egAFMkMWZSN5rYXRj9CAdheBamobVNubTsi9avR44")

// AssociatedTokenProgramID is the SPL Associated Token Account program.
var AssociatedTokenProgramID = solanago.MustPublicKeyFromBase58("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL")

// Instruction discriminators (leading u8 of instruction data), from the IDL.
const (
	discInitSubscriptionAuthority byte = 0
	discRevokeDelegation          byte = 3
	discCreatePlan                byte = 7
	discTransferSubscription      byte = 10
	discSubscribe                 byte = 11
	discCancelSubscription        byte = 12
	discResumeSubscription        byte = 13
)

// PDA seed prefixes, from the IDL.
const (
	seedSubscriptionAuthority = "SubscriptionAuthority"
	seedPlan                  = "plan"
	seedSubscription          = "subscription"
	// seedEventAuthority is the CPI-event authority seed. Verified on devnet: this
	// derives to 3Hnj4BYoDgtpBuqXfiy7Y8cNa3jXaNd4oqgSXBzkMcH7 (the program's fixed
	// event authority). NOTE: it is "event_authority", not the Anchor-default
	// "__event_authority".
	seedEventAuthority = "event_authority"
)

// PlanTerms mirrors the on-chain planTerms struct (amount, periodHours, createdAt).
type PlanTerms struct {
	Amount      uint64
	PeriodHours uint64
	CreatedAt   int64
}

// DeriveSubscriptionAuthority returns the SA PDA for (user, mint):
// seeds = ["SubscriptionAuthority", user, tokenMint].
func DeriveSubscriptionAuthority(user, mint solanago.PublicKey) (solanago.PublicKey, uint8, error) {
	return solanago.FindProgramAddress([][]byte{
		[]byte(seedSubscriptionAuthority),
		user.Bytes(),
		mint.Bytes(),
	}, ProgramID)
}

// DerivePlanPDA returns the plan PDA for (owner, planID):
// seeds = ["plan", owner, planId(u64 le)].
func DerivePlanPDA(owner solanago.PublicKey, planID uint64) (solanago.PublicKey, uint8, error) {
	return solanago.FindProgramAddress([][]byte{
		[]byte(seedPlan),
		owner.Bytes(),
		u64le(planID),
	}, ProgramID)
}

// DeriveSubscriptionPDA returns the per-subscriber subscription PDA:
// seeds = ["subscription", planPda, subscriber].
func DeriveSubscriptionPDA(planPDA, subscriber solanago.PublicKey) (solanago.PublicKey, uint8, error) {
	return solanago.FindProgramAddress([][]byte{
		[]byte(seedSubscription),
		planPDA.Bytes(),
		subscriber.Bytes(),
	}, ProgramID)
}

// DeriveEventAuthority returns the CPI-event authority PDA (seed
// "__event_authority"). DEVNET-VERIFY before mainnet.
func DeriveEventAuthority() (solanago.PublicKey, uint8, error) {
	return solanago.FindProgramAddress([][]byte{[]byte(seedEventAuthority)}, ProgramID)
}

// DeriveATA returns the associated token account for (owner, mint) under the
// given token program (classic SPL or Token-2022). Seeds = [owner, tokenProgram, mint].
func DeriveATA(owner, mint, tokenProgram solanago.PublicKey) (solanago.PublicKey, uint8, error) {
	return solanago.FindProgramAddress([][]byte{
		owner.Bytes(),
		tokenProgram.Bytes(),
		mint.Bytes(),
	}, AssociatedTokenProgramID)
}

func u64le(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}

func i64le(v int64) []byte {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, v)
	return buf.Bytes()
}

// writeFixedString writes s into a fixed-size n-byte buffer (utf8, zero-padded,
// truncated if longer). Mirrors the IDL's fixed-size string encoding.
func writeFixedString(s string, n int) ([]byte, error) {
	b := make([]byte, n)
	if len(s) > n {
		return nil, fmt.Errorf("subscriptions: string %q exceeds fixed size %d", s, n)
	}
	copy(b, s)
	return b, nil
}
