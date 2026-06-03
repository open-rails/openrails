package recurring

import (
	"fmt"

	solanago "github.com/doujins-org/solana-go"
)

// withReferenceMeta attaches a Solana Pay REFERENCE to an instruction by
// appending the reference public key as a read-only, NON-SIGNER account meta —
// the identical mechanism the one-off Solana Pay transfer path uses
// (integrations/solana/transfer.go appends `solanago.Meta(reference)` to the
// transfer instruction). A reference is a throwaway random pubkey; surfacing it
// in an instruction's account list is what lets the reference poller find the
// landed transaction via getSignaturesForAddress(reference).
//
// Appending a trailing read-only account is safe for the subscriptions Anchor
// program: extra accounts beyond the declared ones land in the program's
// remaining_accounts and are ignored by cancel_subscription (it neither reads
// nor writes them), so the on-chain action is unchanged while the tx becomes
// reference-addressable.
//
// reference must be a valid base58 pubkey; an empty reference returns the
// instruction unchanged (no tagging requested).
func withReferenceMeta(ix solanago.Instruction, reference string) (solanago.Instruction, error) {
	if reference == "" {
		return ix, nil
	}
	refKey, err := solanago.PublicKeyFromBase58(reference)
	if err != nil {
		return nil, fmt.Errorf("recurring: invalid solana pay reference: %w", err)
	}
	generic, ok := ix.(*solanago.GenericInstruction)
	if !ok {
		// The subscriptions program instructions are built via
		// solanago.NewInstruction, which returns *GenericInstruction. Guard the
		// assumption rather than silently dropping the reference.
		return nil, fmt.Errorf("recurring: cannot attach reference to instruction of type %T", ix)
	}
	tagged := *generic
	tagged.AccountValues = append(append(solanago.AccountMetaSlice{}, generic.AccountValues...), solanago.Meta(refKey))
	return &tagged, nil
}
