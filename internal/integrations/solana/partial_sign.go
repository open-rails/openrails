package solana

import (
	"context"
	"encoding/base64"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/pkg/tenant"
)

// blockhashGetter is the minimal RPC surface BuildPartiallySignedTx needs.
type blockhashGetter interface {
	GetLatestBlockhash(ctx context.Context) (solanago.Hash, error)
}

// BuildPartiallySignedTx assembles a transaction that requires TWO (or more)
// signers, signs ONLY the co-signer's slot with the tenant's key, and returns it
// base64-encoded with the remaining signature slot(s) left empty for a wallet to
// complete and submit.
//
// This is the on-chain-atomic primitive for the recurring tier change (#272): an
// upgrade is ONE transaction — [cancel(old) + subscribe(new) + transfer(new,
// prorated)] — whose transfer_subscription requires the merchant/cranker as the
// caller-signer while the subscriber is the fee payer + signs cancel/subscribe.
// OpenRails co-signs the cranker slot here; the user's wallet adds its signature
// (the fee-payer slot) and sends, so the whole tier change executes atomically.
//
// feePayer is account index 0 and the wallet-side signer; cosigner must be one of
// the transaction's required signers (else this errors rather than producing an
// un-submittable tx).
func BuildPartiallySignedTx(ctx context.Context, tenantID tenant.ID, cosigner Signer, rpc blockhashGetter, feePayer solanago.PublicKey, instructions []solanago.Instruction) (string, error) {
	if cosigner == nil {
		return "", fmt.Errorf("solana: cosigner is required")
	}
	if len(instructions) == 0 {
		return "", fmt.Errorf("solana: at least one instruction is required")
	}
	blockhash, err := rpc.GetLatestBlockhash(ctx)
	if err != nil {
		return "", fmt.Errorf("solana: get recent blockhash: %w", err)
	}
	tx, err := solanago.NewTransaction(instructions, blockhash, solanago.TransactionPayer(feePayer))
	if err != nil {
		return "", fmt.Errorf("solana: build transaction: %w", err)
	}

	cosignerPub, err := cosigner.PublicKey(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("solana: resolve cosigner public key: %w", err)
	}
	numSigners := int(tx.Message.Header.NumRequiredSignatures)
	idx := -1
	for i := 0; i < numSigners && i < len(tx.Message.AccountKeys); i++ {
		if tx.Message.AccountKeys[i].Equals(cosignerPub) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", fmt.Errorf("solana: cosigner %s is not a required signer of the transaction", cosignerPub)
	}

	msg, err := tx.Message.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("solana: serialize transaction message: %w", err)
	}
	sig, err := cosigner.SignMessage(ctx, tenantID, msg)
	if err != nil {
		return "", fmt.Errorf("solana: cosign transaction: %w", err)
	}

	// One slot per required signer; fill only the co-signer's, leave the rest zero
	// for the wallet to complete.
	tx.Signatures = make([]solanago.Signature, numSigners)
	tx.Signatures[idx] = sig

	raw, err := tx.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("solana: serialize partially-signed transaction: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}
