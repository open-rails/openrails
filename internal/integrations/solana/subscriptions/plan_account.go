package subscriptions

import (
	"bytes"
	"encoding/binary"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
)

// Plan account on-chain layout, from the OFFICIAL Solana Subscriptions program
// (solana-program/subscriptions, address De1egAFMkMWZSN5rYXRj9CAdheBamobVNubTsi9avR44).
// Source: idl/subscriptions.json (Codama IDL). This is a NATIVE program (not
// Anchor): the account discriminator is a single leading u8, and every field is
// fixed-size little-endian (no length-prefixed strings), so the record is a flat
// 491-byte blob we can slice by constant offset.
//
//	discriminator  u8            (= planAccountDiscriminator, 1)
//	owner          pubkey[32]
//	bump           u8
//	status         u8
//	data.planId    u64
//	data.mint      pubkey[32]
//	data.terms.amount       u64
//	data.terms.periodHours  u64
//	data.terms.createdAt    i64
//	data.endTs     i64
//	data.destinations  pubkey[32] x4
//	data.pullers       pubkey[32] x4
//	data.metadataUri   byte[128]  (utf8, zero-padded)
const planAccountDiscriminator byte = 1

// Plan statuses (PlanAccount.Status and updatePlan's status argument), from the
// IDL planStatus enum. sunset blocks NEW subscribe calls (the program rejects
// them with "Plan is in sunset status") while existing subscriptions continue
// to bill — the on-chain equivalent of Stripe's active=false archive.
const (
	PlanStatusSunset uint8 = 0
	PlanStatusActive uint8 = 1
)

const (
	pubkeyLen      = 32
	metadataURILen = 128
	// PlanAccountSize is the exact serialized size of a Plan account. A fetched
	// account must be at least this long (the program may append nothing today,
	// but tolerate trailing bytes for forward-compat).
	PlanAccountSize = 1 + pubkeyLen + 1 + 1 + 8 + pubkeyLen + 8 + 8 + 8 + 8 + (4 * pubkeyLen) + (4 * pubkeyLen) + metadataURILen
)

// PlanAccount is the decoded on-chain Plan record. The immutable core terms
// (Amount, PeriodHours, Mint) are what catalog drift compares against the
// OpenRails-side Price.Processors["solana"] snapshot; Status / EndTs / Pullers /
// MetadataURI are mutable via updatePlan.
type PlanAccount struct {
	Discriminator byte
	Owner         solanago.PublicKey
	Bump          uint8
	Status        uint8
	PlanID        uint64
	Mint          solanago.PublicKey
	Amount        uint64 // base units of Mint, immutable
	PeriodHours   uint64 // immutable
	CreatedAt     int64  // immutable fingerprint (ghost-plan detection)
	EndTs         int64
	Destinations  [4]solanago.PublicKey
	Pullers       [4]solanago.PublicKey
	MetadataURI   string
}

// DecodePlanAccount parses raw Plan account bytes (as returned by
// RPCClient.GetAccountData) into a PlanAccount. It validates the leading
// discriminator and the minimum length, then slices each fixed field by offset.
func DecodePlanAccount(data []byte) (*PlanAccount, error) {
	if len(data) < PlanAccountSize {
		return nil, fmt.Errorf("subscriptions: plan account too short: got %d bytes, need >= %d", len(data), PlanAccountSize)
	}
	if data[0] != planAccountDiscriminator {
		return nil, fmt.Errorf("subscriptions: not a plan account: discriminator %d (want %d)", data[0], planAccountDiscriminator)
	}

	p := &PlanAccount{Discriminator: data[0]}
	off := 1
	readPubkey := func() solanago.PublicKey {
		pk := solanago.PublicKeyFromBytes(data[off : off+pubkeyLen])
		off += pubkeyLen
		return pk
	}
	readU64 := func() uint64 {
		v := binary.LittleEndian.Uint64(data[off : off+8])
		off += 8
		return v
	}
	readI64 := func() int64 {
		var v int64
		_ = binary.Read(bytes.NewReader(data[off:off+8]), binary.LittleEndian, &v)
		off += 8
		return v
	}

	p.Owner = readPubkey()
	p.Bump = data[off]
	off++
	p.Status = data[off]
	off++
	p.PlanID = readU64()
	p.Mint = readPubkey()
	p.Amount = readU64()
	p.PeriodHours = readU64()
	p.CreatedAt = readI64()
	p.EndTs = readI64()
	for i := range 4 {
		p.Destinations[i] = readPubkey()
	}
	for i := range 4 {
		p.Pullers[i] = readPubkey()
	}
	// metadataUri is a fixed 128-byte utf8 field, zero-padded; trim trailing NULs.
	raw := data[off : off+metadataURILen]
	end := len(raw)
	for end > 0 && raw[end-1] == 0x00 {
		end--
	}
	p.MetadataURI = string(raw[:end])

	return p, nil
}
