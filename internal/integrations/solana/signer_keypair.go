package solana

import (
	"context"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
)

// keypairSigner signs in-process with a key the deployment holds; Vault
// Transit keeps it out of the process instead.
type keypairSigner struct{ key solanago.PrivateKey }

// NewKeypairSigner returns the cranker signing with key.
func NewKeypairSigner(key solanago.PrivateKey) Signer { return keypairSigner{key: key} }

func (k keypairSigner) PublicKey(context.Context) (solanago.PublicKey, error) {
	return k.key.PublicKey(), nil
}

func (k keypairSigner) SignMessage(_ context.Context, message []byte) (solanago.Signature, error) {
	if len(message) == 0 {
		return solanago.Signature{}, fmt.Errorf("solana: cannot sign empty message")
	}
	return k.key.Sign(message)
}
