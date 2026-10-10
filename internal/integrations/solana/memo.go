package solana

import (
	"fmt"
	"strings"

	solanago "github.com/gagliardetto/solana-go"
	memoprog "github.com/gagliardetto/solana-go/programs/memo"
	"github.com/google/uuid"
)

// SPL Memo self-recognition stamp "openrails:1:<local-id>": local-id is the
// UUID of our record for the money movement (checkout attempt for one-offs,
// pull intent for recurring); merchant, kind and amount are derivable from the
// chain. Invariants: the memo is a discovery hint, never money truth (that is
// the transfer: mint, base units, destination, signature); no MAC (a forged
// memo costs the forger real money); recognition is read-side only and never
// forces the receiving wallet hot (the crank signer is the rail's one hot key).
//
// This file owns the format; the wallet recovery scan reads it through
// PurchaseMemoLocalIDs.

// purchaseMemoPrefix pins the wire vocabulary. Bump the version segment only
// with a real format change — memos are public and immutable.
const purchaseMemoPrefix = "openrails:1:"

// PurchaseMemo renders the stamp for localID.
func PurchaseMemo(localID uuid.UUID) string {
	return purchaseMemoPrefix + localID.String()
}

// ParsePurchaseMemo recognizes a stamp. ok=false for foreign memos, other
// versions, or anything but a canonical non-nil UUID local-id.
func ParsePurchaseMemo(s string) (uuid.UUID, bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(s), purchaseMemoPrefix)
	if !found || len(rest) != 36 {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(rest)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, false
	}
	return id, true
}

// NewMemoInstruction builds an SPL Memo instruction: program
// MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr, data = the memo string,
// no accounts (an unsigned memo).
func NewMemoInstruction(memo string) solanago.Instruction {
	return memoprog.NewMemoInstruction([]byte(memo)).Build()
}

// PurchaseMemoLocalIDs extracts every recognizable stamp local-id from a
// transaction's memo instructions. Foreign/unparseable memos are ignored: a
// wallet may attach its own.
func PurchaseMemoLocalIDs(tx *solanago.Transaction) []uuid.UUID {
	if tx == nil {
		return nil
	}
	var ids []uuid.UUID
	for _, inst := range tx.Message.Instructions {
		programID, err := tx.ResolveProgramIDIndex(inst.ProgramIDIndex)
		if err != nil || !programID.Equals(solanago.MemoProgramID) {
			continue
		}
		if id, ok := ParsePurchaseMemo(string(inst.Data)); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// PurchaseMemoPolicy says how strictly the stamp is verified, by who built the
// transaction:
//
//   - MemoPresenceOptional: a wallet built it from a Solana Pay URL. A wallet
//     may drop the memo, and a settled payment must not be rejected for a
//     missing hint. A present memo must still match.
//   - MemoRequired: OpenRails built and stamped it (transaction request,
//     recurring crank), so a missing memo means it is not our transaction.
type PurchaseMemoPolicy int

const (
	MemoPresenceOptional PurchaseMemoPolicy = iota
	MemoRequired
)

// VerifyPurchaseMemo applies the stamp rule under policy: a present purchase
// memo must name want; absence fails only under MemoRequired. want ==
// uuid.Nil skips the check (no local record to recognise).
func VerifyPurchaseMemo(tx *solanago.Transaction, want uuid.UUID, policy PurchaseMemoPolicy) error {
	if want == uuid.Nil {
		return nil
	}
	found := false
	for _, got := range PurchaseMemoLocalIDs(tx) {
		if got != want {
			return fmt.Errorf("purchase memo mismatch: transaction stamped for local record %s, expected %s", got, want)
		}
		found = true
	}
	if !found && policy == MemoRequired {
		return fmt.Errorf("purchase memo missing: expected the openrails stamp for local record %s on a transaction we built", want)
	}
	return nil
}
