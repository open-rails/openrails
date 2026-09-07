package recurring

import (
	"encoding/binary"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
)

// systemTransferIndex is the System Program's Transfer instruction discriminator.
const systemTransferIndex uint32 = 2

// referenceTagInstruction returns an instruction whose only job is to make the
// transaction addressable by a Solana Pay REFERENCE: a System Program transfer
// of 0 lamports from payer to itself, with the reference appended as a
// read-only, NON-SIGNER account. That is the mechanism the Solana Pay reference
// spec (and @solana/pay's own createTransfer) uses for SOL transfers, and it is
// what lets the reference poller find the landed tx via
// getSignaturesForAddress(reference).
//
// The reference deliberately rides its OWN instruction rather than being
// appended to a subscriptions-program instruction. The program treats any
// trailing account as an OPTIONAL PAYER that must sign
// (initialize_subscription_authority, subscribe: `rem @ ..` +
// resolve_optional_payer), or destructures an exact account count
// (cancel_subscription) — so a trailing read-only reference makes those
// instructions fail (`NotSigner`, code 100, or `NotEnoughAccountKeys`) before
// they do anything. The System Program ignores accounts beyond the two it uses.
//
// An empty reference returns (nil, nil): nothing to tag.
func referenceTagInstruction(payer solanago.PublicKey, reference string) (solanago.Instruction, error) {
	if reference == "" {
		return nil, nil
	}
	refKey, err := solanago.PublicKeyFromBase58(reference)
	if err != nil {
		return nil, fmt.Errorf("recurring: invalid solana pay reference: %w", err)
	}
	data := make([]byte, 12) // u32 instruction index + u64 lamports (0)
	binary.LittleEndian.PutUint32(data[0:4], systemTransferIndex)
	return solanago.NewInstruction(solanago.SystemProgramID, solanago.AccountMetaSlice{
		solanago.NewAccountMeta(payer, true, true),
		solanago.NewAccountMeta(payer, true, false),
		solanago.Meta(refKey),
	}, data), nil
}

// withReference appends the reference tag instruction to ixs when a reference
// is set; without one the instruction list is returned unchanged.
func withReference(ixs []solanago.Instruction, payer solanago.PublicKey, reference string) ([]solanago.Instruction, error) {
	tag, err := referenceTagInstruction(payer, reference)
	if err != nil {
		return nil, err
	}
	if tag == nil {
		return ixs, nil
	}
	return append(append([]solanago.Instruction{}, ixs...), tag), nil
}
