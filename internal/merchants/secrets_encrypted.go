package merchants

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/openrails/internal/crypto"
	"github.com/open-rails/openrails/pkg/merchant"
)

// encryptedSecretStore wraps any MerchantSecretStore and transparently
// envelope-encrypts secret VALUES at rest with the per-merchant DEK (issue #227).
// Names and versions are stored in the clear; only the sensitive value (Stripe
// secret keys, webhook signing secrets) is encrypted. The underlying store sees
// only ciphertext, so a DB/Vault dump never exposes plaintext credentials.
//
// This is the layer that applies #227 per-merchant encryption-at-rest to the #225
// per-merchant rail credential store. It is wired ONLY when an encryptor is
// enabled (a master key is configured); otherwise the plain store is used as
// before (back-compat / self-hosted-without-encryption).
type encryptedSecretStore struct {
	inner MerchantSecretStore
	enc   *crypto.Encryptor
}

// NewEncryptedSecretStore decorates inner with per-merchant value encryption. If
// enc is nil or disabled (no master key) it returns inner unchanged, so callers
// can wire it unconditionally.
func NewEncryptedSecretStore(inner MerchantSecretStore, enc *crypto.Encryptor) (MerchantSecretStore, error) {
	if inner == nil {
		return nil, errors.New("merchants: inner secret store is required")
	}
	if enc == nil || !enc.Enabled() {
		return inner, nil
	}
	return &encryptedSecretStore{inner: inner, enc: enc}, nil
}

func (e *encryptedSecretStore) Get(ctx context.Context, merchantID merchant.ID, name string) (Secret, error) {
	s, err := e.inner.Get(ctx, merchantID, name)
	if err != nil {
		return Secret{}, err
	}
	plain, derr := e.enc.Decrypt(ctx, merchantID, s.Value)
	if derr != nil {
		return Secret{}, fmt.Errorf("merchants: decrypt secret %q: %w", name, derr)
	}
	s.Value = string(plain)
	return s, nil
}

func (e *encryptedSecretStore) Put(ctx context.Context, merchantID merchant.ID, name, value string) (Secret, error) {
	// Preserve the store's idempotent-rotation semantics at the PLAINTEXT level:
	// ciphertext is non-deterministic (random nonce), so we must compare decrypted
	// values, not ciphertext. If the plaintext is unchanged, return the existing
	// secret without re-encrypting / bumping the version.
	if existing, err := e.Get(ctx, merchantID, name); err == nil {
		if existing.Value == value {
			return existing, nil
		}
	} else if !errors.Is(err, ErrSecretNotFound) {
		return Secret{}, err
	}

	ciphertext, err := e.enc.Encrypt(ctx, merchantID, []byte(value))
	if err != nil {
		return Secret{}, fmt.Errorf("merchants: encrypt secret %q: %w", name, err)
	}
	stored, err := e.inner.Put(ctx, merchantID, name, ciphertext)
	if err != nil {
		return Secret{}, err
	}
	// Return the plaintext value to the caller (it round-trips), not the stored
	// ciphertext.
	stored.Value = value
	return stored, nil
}

func (e *encryptedSecretStore) Delete(ctx context.Context, merchantID merchant.ID, name string) error {
	return e.inner.Delete(ctx, merchantID, name)
}

func (e *encryptedSecretStore) List(ctx context.Context, merchantID merchant.ID) ([]string, error) {
	// Names are stored in the clear; no decryption needed.
	return e.inner.List(ctx, merchantID)
}
