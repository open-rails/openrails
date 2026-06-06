package recurring

import (
	"testing"

	solanago "github.com/doujins-org/solana-go"
)

func assertUnsignedSignatureSlots(t *testing.T, tx *solanago.Transaction) {
	t.Helper()
	want := int(tx.Message.Header.NumRequiredSignatures)
	if len(tx.Signatures) != want {
		t.Fatalf("signature slots = %d, want %d required signer slots", len(tx.Signatures), want)
	}
	for i, sig := range tx.Signatures {
		if !sig.IsZero() {
			t.Fatalf("signature slot %d is non-zero for unsigned wallet tx", i)
		}
	}
}
