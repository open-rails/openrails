package recurring

import (
	"encoding/binary"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
)

// systemTransferIndex is the System Program's Transfer instruction discriminator.
const systemTransferIndex uint32 = 2

// referenceTagInstruction makes the transaction findable by a Solana Pay
// reference (getSignaturesForAddress): a 0-lamport System transfer from payer to
// itself with the reference as a read-only non-signer account, as @solana/pay's
// createTransfer does. It is its own instruction because the subscriptions
// program treats a trailing account as a payer that must sign, or expects an
// exact account count, and fails (NotSigner, NotEnoughAccountKeys). An empty
// reference returns (nil, nil).
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
