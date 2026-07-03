package subscriptions

import (
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
