package solana

import (
	"context"
	"fmt"
	"sync"
	"time"

	solanago "github.com/gagliardetto/solana-go"
)

// TransitClient is the Vault Transit surface the remote signer needs, declared
// here so this package has no Vault dependency; vault.TransitAdapter
// implements it.
type TransitClient interface {
	// Sign returns the raw 64-byte Ed25519 signature for input over key `name`.
	Sign(ctx context.Context, name string, input []byte) ([]byte, error)
	// PublicKey returns the raw 32-byte Ed25519 public key for key `name`.
	PublicKey(ctx context.Context, name string) ([]byte, error)
}

// transitSigner is the cranker whose Ed25519 key lives inside Vault Transit
// (non-extractable) and never enters this process. Its public key is cached;
// signatures always round-trip Vault.
type transitSigner struct {
	transit TransitClient
	key     string
	ttl     time.Duration
	now     func() time.Time

	mu        sync.Mutex
	pub       solanago.PublicKey
	expiresAt time.Time
}

// NewTransitSigner returns the cranker signing with Transit key. A zero ttl
// uses DefaultSignerCacheTTL for the public-key cache.
func NewTransitSigner(transit TransitClient, key string, ttl time.Duration) Signer {
	if ttl <= 0 {
		ttl = DefaultSignerCacheTTL
	}
	return &transitSigner{transit: transit, key: key, ttl: ttl, now: time.Now}
}

func (t *transitSigner) PublicKey(ctx context.Context) (solanago.PublicKey, error) {
	if t.transit == nil {
		return solanago.PublicKey{}, fmt.Errorf("solana: transit signer has no client")
	}
	t.mu.Lock()
	if t.now().Before(t.expiresAt) {
		key := t.pub
		t.mu.Unlock()
		return key, nil
	}
	t.mu.Unlock()

	raw, err := t.transit.PublicKey(ctx, t.key)
	if err != nil {
		return solanago.PublicKey{}, fmt.Errorf("solana: transit public key: %w", err)
	}
	if len(raw) != 32 {
		return solanago.PublicKey{}, fmt.Errorf("solana: transit public key is %d bytes, want 32", len(raw))
	}
	key := solanago.PublicKeyFromBytes(raw)

	t.mu.Lock()
	t.pub, t.expiresAt = key, t.now().Add(t.ttl)
	t.mu.Unlock()
	return key, nil
}

func (t *transitSigner) SignMessage(ctx context.Context, message []byte) (solanago.Signature, error) {
	if t.transit == nil {
		return solanago.Signature{}, fmt.Errorf("solana: transit signer has no client")
	}
	if len(message) == 0 {
		return solanago.Signature{}, fmt.Errorf("solana: cannot sign empty message")
	}
	raw, err := t.transit.Sign(ctx, t.key, message)
	if err != nil {
		return solanago.Signature{}, fmt.Errorf("solana: transit sign: %w", err)
	}
	if len(raw) != 64 {
		return solanago.Signature{}, fmt.Errorf("solana: transit signature is %d bytes, want 64", len(raw))
	}
	return solanago.SignatureFromBytes(raw), nil
}
