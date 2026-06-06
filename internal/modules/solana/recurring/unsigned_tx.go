package recurring

import (
	"encoding/base64"
	"fmt"

	solanago "github.com/doujins-org/solana-go"
)

// marshalUnsignedTxBase64 serializes an unsigned transaction in the shape wallet
// adapters and RPC simulation expect: one zeroed signature slot per required
// signer. A legacy transaction with zero signature slots but required signers is
// malformed and fails RPC sanitization before program execution.
func marshalUnsignedTxBase64(tx *solanago.Transaction) (string, error) {
	if tx == nil {
		return "", fmt.Errorf("recurring: transaction is required")
	}

	numSigners := int(tx.Message.Header.NumRequiredSignatures)
	tx.Signatures = make([]solanago.Signature, numSigners)
	raw, err := tx.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("recurring: serialize transaction: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}
