// Package crypto implements per-merchant envelope (KMS-style) encryption-at-rest
// for OpenRails' merchant platform (issue #227).
//
// Design (envelope encryption):
//
//   - A single MASTER key (32 bytes, AES-256) wraps each merchant's per-merchant
//     Data Encryption Key (DEK). The master key comes from config/env in
//     self-hosted mode and from a KMS in production; it never touches the DB.
//   - Each merchant has its own 32-byte DEK, generated lazily on first use and
//     stored WRAPPED (sealed with the master key) in openrails.merchant_deks. One
//     merchant's DEK can never decrypt another merchant's ciphertext.
//   - Field values are encrypted with the merchant DEK using AES-256-GCM. Each
//     ciphertext is self-describing: nonce || ciphertext || tag, base64-encoded
//     for storage in TEXT columns.
//
// The DEK store is pluggable (DEKStore) so the wrapped-DEK directory can live in
// Postgres (openrails.merchant_deks) in self-hosted mode or be swapped for a KMS /
// external store in production without changing callers.
package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	// keySize is the AES-256 key length for both the master key and per-merchant DEKs.
	keySize = 32
	// nonceSize is the standard AES-GCM nonce length.
	nonceSize = 12
)

// ErrEncryptionDisabled is returned when an Encryptor operation is attempted but
// no master key is configured. Callers that must degrade gracefully (store
// plaintext when encryption is off) check for this with errors.Is.
var ErrEncryptionDisabled = errors.New("crypto: encryption disabled (no master key configured)")

// DEKStore persists wrapped per-merchant DEKs. Implementations are addressed only
// by merchant id; they never see plaintext DEKs (the Encryptor wraps/unwraps).
type DEKStore interface {
	// GetWrappedDEK returns the wrapped DEK for a merchant, or (nil, false) when the
	// merchant has none yet.
	GetWrappedDEK(ctx context.Context, merchantID merchant.ID) (wrapped []byte, ok bool, err error)
	// PutWrappedDEK stores the wrapped DEK for a merchant. It must be safe under
	// concurrent first-use: if a row already exists it should keep the existing
	// one and return that (so two racing creates converge on one DEK).
	PutWrappedDEK(ctx context.Context, merchantID merchant.ID, wrapped []byte) (stored []byte, err error)
}

// Encryptor provides per-merchant field encryption backed by envelope encryption.
// It is safe for concurrent use.
type Encryptor struct {
	masterGCM cipher.AEAD
	store     DEKStore

	mu      sync.Mutex
	dekGCMs map[merchant.ID]cipher.AEAD // cache of unwrapped per-merchant DEK ciphers
}

// NewEncryptor builds an Encryptor from a base64-encoded 32-byte master key and a
// DEK store. An empty master key yields a disabled Encryptor whose Encrypt/Decrypt
// return ErrEncryptionDisabled, letting callers fall back to plaintext for
// back-compat / self-hosted-without-encryption.
func NewEncryptor(masterKeyB64 string, store DEKStore) (*Encryptor, error) {
	if masterKeyB64 == "" {
		return &Encryptor{store: store, dekGCMs: map[merchant.ID]cipher.AEAD{}}, nil
	}
	key, err := base64.StdEncoding.DecodeString(masterKeyB64)
	if err != nil {
		return nil, fmt.Errorf("crypto: decode master key: %w", err)
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("crypto: master key must be %d bytes (got %d)", keySize, len(key))
	}
	if store == nil {
		return nil, errors.New("crypto: DEK store is required")
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: master key cipher: %w", err)
	}
	return &Encryptor{masterGCM: gcm, store: store, dekGCMs: map[merchant.ID]cipher.AEAD{}}, nil
}

// Enabled reports whether at-rest encryption is active (a master key is set).
func (e *Encryptor) Enabled() bool { return e != nil && e.masterGCM != nil }

// Encrypt seals plaintext with the merchant's DEK and returns a base64 string
// (nonce || ciphertext || tag). The merchant DEK is created+wrapped+stored lazily
// on first use and reused thereafter.
func (e *Encryptor) Encrypt(ctx context.Context, merchantID merchant.ID, plaintext []byte) (string, error) {
	if !e.Enabled() {
		return "", ErrEncryptionDisabled
	}
	if merchantID.IsZero() {
		return "", errors.New("crypto: encrypt requires a merchant id")
	}
	gcm, err := e.merchantGCM(ctx, merchantID)
	if err != nil {
		return "", err
	}
	sealed, err := seal(gcm, plaintext)
	if err != nil {
		return "", fmt.Errorf("crypto: encrypt: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt for the same merchant. A ciphertext produced for one
// merchant CANNOT be decrypted with another merchant's DEK (GCM auth fails).
func (e *Encryptor) Decrypt(ctx context.Context, merchantID merchant.ID, ciphertextB64 string) ([]byte, error) {
	if !e.Enabled() {
		return nil, ErrEncryptionDisabled
	}
	if merchantID.IsZero() {
		return nil, errors.New("crypto: decrypt requires a merchant id")
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return nil, fmt.Errorf("crypto: decode ciphertext: %w", err)
	}
	gcm, err := e.merchantGCM(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	pt, err := open(gcm, raw)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt: %w", err)
	}
	return pt, nil
}

// merchantGCM returns the AEAD for a merchant's DEK, lazily creating + wrapping +
// storing the DEK on first use and caching the unwrapped cipher.
func (e *Encryptor) merchantGCM(ctx context.Context, merchantID merchant.ID) (cipher.AEAD, error) {
	e.mu.Lock()
	if gcm, ok := e.dekGCMs[merchantID]; ok {
		e.mu.Unlock()
		return gcm, nil
	}
	e.mu.Unlock()

	dek, err := e.loadOrCreateDEK(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, fmt.Errorf("crypto: merchant DEK cipher: %w", err)
	}
	e.mu.Lock()
	// Another goroutine may have populated it meanwhile; keep a single instance.
	if existing, ok := e.dekGCMs[merchantID]; ok {
		gcm = existing
	} else {
		e.dekGCMs[merchantID] = gcm
	}
	e.mu.Unlock()
	return gcm, nil
}

// loadOrCreateDEK fetches and unwraps the merchant's DEK, or generates a fresh DEK,
// wraps it with the master key, and stores it (idempotently). The plaintext DEK
// is never persisted.
func (e *Encryptor) loadOrCreateDEK(ctx context.Context, merchantID merchant.ID) ([]byte, error) {
	if wrapped, ok, err := e.store.GetWrappedDEK(ctx, merchantID); err != nil {
		return nil, fmt.Errorf("crypto: load wrapped DEK: %w", err)
	} else if ok {
		return e.unwrapDEK(wrapped)
	}

	// Lazily create a new DEK, wrap it, and store it. Store may return a
	// different (pre-existing) wrapped DEK if it lost a create race; honour it.
	dek := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, fmt.Errorf("crypto: generate DEK: %w", err)
	}
	wrapped, err := seal(e.masterGCM, dek)
	if err != nil {
		return nil, fmt.Errorf("crypto: wrap DEK: %w", err)
	}
	stored, err := e.store.PutWrappedDEK(ctx, merchantID, wrapped)
	if err != nil {
		return nil, fmt.Errorf("crypto: store wrapped DEK: %w", err)
	}
	// If the store kept a pre-existing row (concurrent first-use), unwrap that so
	// every caller converges on the SAME DEK for the merchant.
	if len(stored) > 0 && !bytesEqual(stored, wrapped) {
		return e.unwrapDEK(stored)
	}
	return dek, nil
}

func (e *Encryptor) unwrapDEK(wrapped []byte) ([]byte, error) {
	dek, err := open(e.masterGCM, wrapped)
	if err != nil {
		return nil, fmt.Errorf("crypto: unwrap DEK (wrong master key?): %w", err)
	}
	if len(dek) != keySize {
		return nil, fmt.Errorf("crypto: unwrapped DEK has wrong length %d", len(dek))
	}
	return dek, nil
}

// --- low-level AES-GCM helpers ---------------------------------------------

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// seal returns nonce || ciphertext(+tag).
func seal(gcm cipher.AEAD, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// open reverses seal: it splits the leading nonce and authenticates the rest.
func open(gcm cipher.AEAD, sealed []byte) ([]byte, error) {
	if len(sealed) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ct := sealed[:nonceSize], sealed[nonceSize:]
	return gcm.Open(nil, nonce, ct, nil)
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
