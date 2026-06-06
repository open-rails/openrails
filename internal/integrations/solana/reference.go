package solana

import (
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
)

// GenerateReference creates a new random public key and returns it as a base58 string.
// The private key is discarded.
func GenerateReference() (string, error) {
	privateKey, err := solanago.NewRandomPrivateKey()
	if err != nil {
		return "", fmt.Errorf("failed to generate solana private key: %w", err)
	}
	return privateKey.PublicKey().String(), nil
}
