package solana

import (
	"fmt"
	"strings"

	solanago "github.com/gagliardetto/solana-go"
	memoprog "github.com/gagliardetto/solana-go/programs/memo"
	"github.com/google/uuid"
)

// SPL Memo self-recognition stamp (#713), format "openrails:1:<local-id>":
// version segment + the ONE field the chain cannot derive — local-id is the
// openrails-side UUID of OUR record for that money movement (checkout session
// for one-offs, pull intent for recurring). Merchant, kind and amount are
// deliberately omitted: derivable from the chain. STANDING INVARIANTS: the
// memo is a DISCOVERY HINT, never money truth — money truth is only the
// transfer itself (mint, base units, destination, signature); no MAC (forged
// memos cost the attacker real money to matter); the Solana rail keeps exactly
// ONE hot key (the crank signer) — memo recognition/recovery are read-side
// only and must never force the receiving wallet hot.
//
// This file is the single source of truth for the format; the #714 recovery
// lane (chain→local merchant-wallet scan) parses with ParsePurchaseMemo.

// purchaseMemoPrefix pins the wire vocabulary. Bump the version segment only
// with a real format change — memos are public and immutable.
const purchaseMemoPrefix = "openrails:1:"

// PurchaseMemo renders the #713 stamp for localID.
func PurchaseMemo(localID uuid.UUID) string {
	return purchaseMemoPrefix + localID.String()
}

// ParsePurchaseMemo recognizes a #713 stamp. ok=false for foreign memos,
// other versions, or anything but a canonical non-nil UUID local-id.
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

// PurchaseMemoLocalIDs extracts every recognizable #713 local-id from a
// transaction's memo instructions. Foreign/unparseable memos are ignored —
// a wallet may attach its own.
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

// PurchaseMemoPolicy declares how strictly the #713 stamp is verified. It is a
// property of WHO BUILT THE TRANSACTION, and or#893 makes it explicit rather
// than leaving "absence passes" as an inherited pre-#713 tolerance:
//
//   - MemoPresenceOptional — a WALLET built the transaction from a Solana Pay
//     URL. The spec asks the wallet to attach the memo, but the wallet is not
//     ours and a payment that really settled must not be rejected because a
//     wallet dropped a discovery hint. A present memo must still match.
//   - MemoRequired — OPENRAILS built and stamped the transaction (the
//     transaction-request flow, the recurring crank). We know the memo is
//     there, so its absence means the signature is not the transaction we
//     built, and that is a verification failure.
type PurchaseMemoPolicy int

const (
	MemoPresenceOptional PurchaseMemoPolicy = iota
	MemoRequired
)

// VerifyPurchaseMemo applies the #713 verify rule under policy. A present
// purchase memo must always name want (mismatch = verification failure);
// absence fails only under MemoRequired. want == uuid.Nil skips the check
// entirely (no local record to recognise).
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
