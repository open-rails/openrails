package subscriptions

import (
	"bytes"
	"encoding/binary"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
)

// InstructionKind names a subscriptions-program instruction, identified by the
// leading discriminator byte of its instruction data (the inverse of the
// builders in instructions.go).
type InstructionKind string

const (
	KindInitSubscriptionAuthority InstructionKind = "initialize_subscription_authority"
	KindRevokeDelegation          InstructionKind = "revoke_delegation"
	KindCreatePlan                InstructionKind = "create_plan"
	KindUpdatePlan                InstructionKind = "update_plan"
	KindTransferSubscription      InstructionKind = "transfer_subscription"
	KindSubscribe                 InstructionKind = "subscribe"
	KindCancelSubscription        InstructionKind = "cancel_subscription"
	KindResumeSubscription        InstructionKind = "resume_subscription"
)

// ParseInstructionKind classifies raw instruction data by its discriminator.
// ok=false for empty data or a discriminator this package does not build.
func ParseInstructionKind(data []byte) (InstructionKind, bool) {
	if len(data) == 0 {
		return "", false
	}
	switch data[0] {
	case discInitSubscriptionAuthority:
		return KindInitSubscriptionAuthority, true
	case discRevokeDelegation:
		return KindRevokeDelegation, true
	case discCreatePlan:
		return KindCreatePlan, true
	case discUpdatePlan:
		return KindUpdatePlan, true
	case discTransferSubscription:
		return KindTransferSubscription, true
	case discSubscribe:
		return KindSubscribe, true
	case discCancelSubscription:
		return KindCancelSubscription, true
	case discResumeSubscription:
		return KindResumeSubscription, true
	}
	return "", false
}

// TransferData mirrors the on-chain transferData struct carried by
// transfer_subscription (the pull): the amount to move, the subscriber, and
// the mint. Fixed-size little-endian, matching BuildTransferSubscription.
type TransferData struct {
	Amount    uint64 // mint base units
	Delegator solanago.PublicKey
	Mint      solanago.PublicKey
}

// transferDataLen = disc(1) + amount(8) + delegator(32) + mint(32).
const transferDataLen = 1 + 8 + (2 * pubkeyLen)

// DecodeTransferData parses transfer_subscription instruction data (the exact
// inverse of BuildTransferSubscription's encoding).
func DecodeTransferData(data []byte) (*TransferData, error) {
	if len(data) < transferDataLen {
		return nil, fmt.Errorf("subscriptions: transfer data too short: got %d bytes, need %d", len(data), transferDataLen)
	}
	if data[0] != discTransferSubscription {
		return nil, fmt.Errorf("subscriptions: not transfer_subscription data: discriminator %d (want %d)", data[0], discTransferSubscription)
	}
	return &TransferData{
		Amount:    binary.LittleEndian.Uint64(data[1:9]),
		Delegator: solanago.PublicKeyFromBytes(data[9 : 9+pubkeyLen]),
		Mint:      solanago.PublicKeyFromBytes(data[9+pubkeyLen : 9+2*pubkeyLen]),
	}, nil
}

// SubscribeData mirrors subscribe's instruction data (the inverse of
// BuildSubscribe): the plan and the terms the subscriber agreed to.
type SubscribeData struct {
	PlanID                 uint64
	PlanBump               uint8
	Mint                   solanago.PublicKey
	Amount                 uint64
	PeriodHours            uint64
	CreatedAt              int64
	SubscriptionAuthInitID int64
}

// subscribeDataLen = disc(1) + planId(8) + bump(1) + mint(32) + amount(8) +
// periodHours(8) + createdAt(8) + initId(8).
const subscribeDataLen = 1 + 8 + 1 + pubkeyLen + 8 + 8 + 8 + 8

// DecodeSubscribeData parses subscribe instruction data.
func DecodeSubscribeData(data []byte) (*SubscribeData, error) {
	if len(data) < subscribeDataLen {
		return nil, fmt.Errorf("subscriptions: subscribe data too short: got %d bytes, need %d", len(data), subscribeDataLen)
	}
	if data[0] != discSubscribe {
		return nil, fmt.Errorf("subscriptions: not subscribe data: discriminator %d (want %d)", data[0], discSubscribe)
	}
	off := 1 + 8 + 1 + pubkeyLen
	return &SubscribeData{
		PlanID:                 binary.LittleEndian.Uint64(data[1:9]),
		PlanBump:               data[9],
		Mint:                   solanago.PublicKeyFromBytes(data[10:off]),
		Amount:                 binary.LittleEndian.Uint64(data[off : off+8]),
		PeriodHours:            binary.LittleEndian.Uint64(data[off+8 : off+16]),
		CreatedAt:              i64FromLE(data[off+16 : off+24]),
		SubscriptionAuthInitID: i64FromLE(data[off+24 : off+32]),
	}, nil
}

func i64FromLE(b []byte) int64 {
	var v int64
	_ = binary.Read(bytes.NewReader(b), binary.LittleEndian, &v)
	return v
}
