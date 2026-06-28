package solana

import (
	"testing"
)

const (
	validSignature87 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz123456789ABCDEFGHJKLMNPQRSTUV"
	validSignature88 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz123456789ABCDEFGHJKLMNPQRSTUVW"
	shortSignature   = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz123456789ABCDEFGHJKLMNPQRSTU"
	longSignature    = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz123456789ABCDEFGHJKLMNPQRSTUVWX"
	invalidCharZero  = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz123456789ABCDEFGHJKLMNPQRSTU0"
	invalidCharO     = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz123456789ABCDEFGHJKLMNPQRSTUO"
)

func TestValidateAddress(t *testing.T) {
	tests := []struct {
		name      string
		address   string
		expectErr bool
	}{
		{
			name:      "valid address",
			address:   "11111111111111111111111111111112",
			expectErr: false,
		},
		{
			name:      "valid longer address",
			address:   "So11111111111111111111111111111111111111112",
			expectErr: false,
		},
		{
			name:      "empty address",
			address:   "",
			expectErr: true,
		},
		{
			name:      "too short address",
			address:   "111111111111111111111111111111",
			expectErr: true,
		},
		{
			name:      "too long address",
			address:   "111111111111111111111111111111111111111111111",
			expectErr: true,
		},
		{
			name:      "invalid characters",
			address:   "1111111111111111111111111111111O", // O is not in base58
			expectErr: true,
		},
		{
			name:      "invalid character 0",
			address:   "1111111111111111111111111111111110", // 0 is not in base58
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAddress(tt.address)
			if tt.expectErr && err == nil {
				t.Errorf("ValidateAddress() expected error but got none")
			}
			if !tt.expectErr && err != nil {
				t.Errorf("ValidateAddress() unexpected error: %v", err)
			}
		})
	}
}

func TestValidateSignature(t *testing.T) {
	tests := []struct {
		name      string
		signature string
		expectErr bool
	}{
		{
			name:      "valid signature 87 chars",
			signature: validSignature87,
			expectErr: false,
		},
		{
			name:      "valid signature 88 chars",
			signature: validSignature88,
			expectErr: false,
		},
		{
			name:      "empty signature",
			signature: "",
			expectErr: true,
		},
		{
			name:      "too short signature",
			signature: shortSignature,
			expectErr: true,
		},
		{
			name:      "too long signature",
			signature: longSignature,
			expectErr: true,
		},
		{
			name:      "invalid character 0",
			signature: invalidCharZero,
			expectErr: true,
		},
		{
			name:      "invalid character O",
			signature: invalidCharO,
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSignature(tt.signature)
			if tt.expectErr && err == nil {
				t.Errorf("ValidateSignature() expected error but got none")
			}
			if !tt.expectErr && err != nil {
				t.Errorf("ValidateSignature() unexpected error: %v", err)
			}
		})
	}
}
