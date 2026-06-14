package tenancy

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/openrails/internal/crypto"
	"github.com/open-rails/openrails/pkg/merchant"
)

// encryptedSecretStore wraps any TenantSecretStore and transparently
// envelope-encrypts secret VALUES at rest with the per-tenant DEK (issue #227).
// Names and versions are stored in the clear; only the sensitive value (Stripe
// secret keys, webhook signing secrets) is encrypted. The underlying store sees
// only ciphertext, so a DB/Vault dump never exposes plaintext credentials.
//
// This is the layer that applies #227 per-tenant encryption-at-rest to the #225
// per-tenant processor credential store. It is wired ONLY when an encryptor is
// enabled (a master key is configured); otherwise the plain store is used as
// before (back-compat / self-hosted-without-encryption).
type encryptedSecretStore struct {
	inner TenantSecretStore
	enc   *crypto.Encryptor
}

// NewEncryptedSecretStore decorates inner with per-tenant value encryption. If
// enc is nil or disabled (no master key) it returns inner unchanged, so callers
// can wire it unconditionally.
func NewEncryptedSecretStore(inner TenantSecretStore, enc *crypto.Encryptor) (TenantSecretStore, error) {
	if inner == nil {
		return nil, errors.New("tenancy: inner secret store is required")
	}
	if enc == nil || !enc.Enabled() {
		return inner, nil
	}
	return &encryptedSecretStore{inner: inner, enc: enc}, nil
}

func (e *encryptedSecretStore) Get(ctx context.Context, tenantID merchant.ID, name string) (Secret, error) {
	s, err := e.inner.Get(ctx, tenantID, name)
	if err != nil {
		return Secret{}, err
	}
	plain, derr := e.enc.Decrypt(ctx, tenantID, s.Value)
	if derr != nil {
		return Secret{}, fmt.Errorf("tenancy: decrypt secret %q: %w", name, derr)
	}
	s.Value = string(plain)
	return s, nil
}

func (e *encryptedSecretStore) Put(ctx context.Context, tenantID merchant.ID, name, value string) (Secret, error) {
	// Preserve the store's idempotent-rotation semantics at the PLAINTEXT level:
	// ciphertext is non-deterministic (random nonce), so we must compare decrypted
	// values, not ciphertext. If the plaintext is unchanged, return the existing
	// secret without re-encrypting / bumping the version.
	if existing, err := e.Get(ctx, tenantID, name); err == nil {
		if existing.Value == value {
			return existing, nil
		}
	} else if !errors.Is(err, ErrSecretNotFound) {
		return Secret{}, err
	}

	ciphertext, err := e.enc.Encrypt(ctx, tenantID, []byte(value))
	if err != nil {
		return Secret{}, fmt.Errorf("tenancy: encrypt secret %q: %w", name, err)
	}
	stored, err := e.inner.Put(ctx, tenantID, name, ciphertext)
	if err != nil {
		return Secret{}, err
	}
	// Return the plaintext value to the caller (it round-trips), not the stored
	// ciphertext.
	stored.Value = value
	return stored, nil
}

func (e *encryptedSecretStore) Delete(ctx context.Context, tenantID merchant.ID, name string) error {
	return e.inner.Delete(ctx, tenantID, name)
}

func (e *encryptedSecretStore) List(ctx context.Context, tenantID merchant.ID) ([]string, error) {
	// Names are stored in the clear; no decryption needed.
	return e.inner.List(ctx, tenantID)
}
