package solana

import (
	"errors"
	"fmt"

	solanago "github.com/doujins-org/solana-go"
)

var (
	ErrInvalidSignature = errors.New("invalid Solana signature")
	ErrInvalidAddress   = errors.New("invalid Solana address")
)

// ValidateAddress checks if a string is a valid Solana wallet address
func ValidateAddress(address string) error {
	if address == "" {
		return fmt.Errorf("%w: address cannot be empty", ErrInvalidAddress)
	}
	if _, err := solanago.PublicKeyFromBase58(address); err != nil {
		return fmt.Errorf("%w: address format invalid", ErrInvalidAddress)
	}
	return nil
}

// ValidateSignature checks if a string is a valid Solana transaction signature format
func ValidateSignature(signature string) error {
	if signature == "" {
		return fmt.Errorf("%w: signature cannot be empty", ErrInvalidSignature)
	}
	if _, err := solanago.SignatureFromBase58(signature); err != nil {
		return fmt.Errorf("%w: signature format invalid", ErrInvalidSignature)
	}
	return nil
}

// IsValidAddress returns true if the address format is valid
func IsValidAddress(address string) bool {
	return ValidateAddress(address) == nil
}

// IsValidSignature returns true if the signature format is valid
func IsValidSignature(signature string) bool {
	return ValidateSignature(signature) == nil
}
