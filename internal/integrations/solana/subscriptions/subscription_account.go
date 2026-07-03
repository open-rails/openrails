package subscriptions

import (
	"bytes"
	"encoding/binary"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
)

// SubscriptionDelegation account on-chain layout, from the OFFICIAL program
// (solana-program/subscriptions): program/src/state/{subscription_delegation,header}.rs
// and the Codama IDL account `subscriptionDelegation`. Cross-validated against
// this repo's devnet-verified artifacts: the plan decoder (same IDL, byte-for-byte),
// the SubscriptionAuthority initId offset 98, and the instruction discriminators.
//
//	header.discriminator  u8      (= 4, accountDiscriminator::subscriptionDelegation)
//	header.version        u8      (schema version; v1 today, later versions APPEND bytes only)
//	header.bump           u8
//	header.delegator      pubkey  (the subscriber wallet)
//	header.delegatee      pubkey  (the plan PDA)
//	header.payer          pubkey  (funded rent; receives it on close)
//	header.initId         i64     (SubscriptionAuthority init_id snapshot)
//	terms.amount          u64     (plan-terms snapshot at subscribe, mint base units)
//	terms.periodHours     u64
//	terms.createdAt       i64
//	amountPulledInPeriod  u64
//	currentPeriodStartTs  i64
//	expiresAtTs           i64     (0 = not cancelled; else the valid-until boundary
//	                               cancel_subscription set — end of the then-current
//	                               period. resume_subscription clears it back to 0.)
const subscriptionAccountDiscriminator byte = 4

// getProgramAccounts filter anchors (#714): enumerate a plan's subscription
// accounts by memcmp(discriminator @ 0) AND memcmp(plan PDA/delegatee @ 35).
// The v1 field prefix is frozen, so the offsets hold for later versions too.
const (
	SubscriptionAccountDiscriminator          = subscriptionAccountDiscriminator
	SubscriptionAccountDelegateeOffset uint64 = 3 + pubkeyLen // disc + version + bump + delegator
)

// SubscriptionAccountSize is the v1 serialized size (the program's frozen
// V1_LEN). Later schema versions append trailing bytes, never move fields, so
// a fetched account must be at least this long.
const SubscriptionAccountSize = 1 + 1 + 1 + (3 * pubkeyLen) + 8 + 8 + 8 + 8 + 8 + 8 + 8

// SubscriptionAccount is the decoded on-chain SubscriptionDelegation record —
// one subscriber's enrollment in a plan (PDA seeds ["subscription", planPda,
// subscriber]).
type SubscriptionAccount struct {
	Discriminator byte
	Version       uint8
	Bump          uint8
	Delegator     solanago.PublicKey // subscriber wallet
	Delegatee     solanago.PublicKey // plan PDA
	Payer         solanago.PublicKey
	InitID        int64
	// Amount / PeriodHours / CreatedAt are the plan-terms snapshot taken at
	// subscribe time; the program rejects pulls when the live plan diverges.
	Amount               uint64 // mint base units per period
	PeriodHours          uint64
	CreatedAt            int64
	AmountPulledInPeriod uint64
	CurrentPeriodStartTs int64
	ExpiresAtTs          int64 // 0 = active (not cancelled)
}

// Cancelled reports whether a cancel is in effect: expiresAtTs was set by
// cancel_subscription and not cleared by resume_subscription.
func (s *SubscriptionAccount) Cancelled() bool { return s.ExpiresAtTs != 0 }

// DecodeSubscriptionAccount parses raw SubscriptionDelegation account bytes
// (as returned by RPCClient.GetAccountData) by fixed offset, validating the
// discriminator and the v1 minimum length (trailing bytes from later schema
// versions are tolerated; the v1 field prefix is frozen).
func DecodeSubscriptionAccount(data []byte) (*SubscriptionAccount, error) {
	if len(data) < SubscriptionAccountSize {
		return nil, fmt.Errorf("subscriptions: subscription account too short: got %d bytes, need >= %d", len(data), SubscriptionAccountSize)
	}
	if data[0] != subscriptionAccountDiscriminator {
		return nil, fmt.Errorf("subscriptions: not a subscription account: discriminator %d (want %d)", data[0], subscriptionAccountDiscriminator)
	}

	s := &SubscriptionAccount{Discriminator: data[0], Version: data[1], Bump: data[2]}
	off := 3
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

	s.Delegator = readPubkey()
	s.Delegatee = readPubkey()
	s.Payer = readPubkey()
	s.InitID = readI64()
	s.Amount = readU64()
	s.PeriodHours = readU64()
	s.CreatedAt = readI64()
	s.AmountPulledInPeriod = readU64()
	s.CurrentPeriodStartTs = readI64()
	s.ExpiresAtTs = readI64()

	return s, nil
}
