package solana

import (
	"context"
	"fmt"
	"sync"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TransitClient is the minimal Vault Transit surface the remote signer needs. It
// is declared HERE (no hashicorp/vault/api dependency) so this package builds and
// is unit-testable without Vault; a managed deployment satisfies it with a real
// adapter (internal/integrations/vault, issue #251) over the transit engine.
type TransitClient interface {
	// Sign returns the raw 64-byte Ed25519 signature for input over key `name`.
	Sign(ctx context.Context, name string, input []byte) ([]byte, error)
	// PublicKey returns the raw 32-byte Ed25519 public key for key `name`.
	PublicKey(ctx context.Context, name string) ([]byte, error)
}

// TransitKeyName maps a tenant to its Vault Transit key name.
type TransitKeyName func(merchant.ID) string

// DefaultTransitKeyName names a tenant's signing key "openrails-solana-<tenantID>".
func DefaultTransitKeyName(id merchant.ID) string {
	return "openrails-solana-" + id.String()
}

// transitSigner is the "sign this" Signer: the tenant's Ed25519 key lives inside
// Vault Transit (non-extractable) and NEVER enters this process. PublicKey is the
// Solana address (base58 of the Ed25519 public key) and is cached since it is
// stable for the key version; signatures always round-trip Vault.
type transitSigner struct {
	transit TransitClient
	keyName TransitKeyName
	ttl     time.Duration
	now     func() time.Time

	mu   sync.Mutex
	pubs map[string]cachedPub // keyed by tenantID.String()
}

type cachedPub struct {
	key       solanago.PublicKey
	expiresAt time.Time
}

// NewTransitSigner returns a per-tenant Signer backed by Vault Transit. A nil
// keyName uses DefaultTransitKeyName; a zero ttl uses DefaultSignerCacheTTL for
// the public-key cache.
func NewTransitSigner(transit TransitClient, keyName TransitKeyName, ttl time.Duration) Signer {
	if keyName == nil {
		keyName = DefaultTransitKeyName
	}
	if ttl <= 0 {
		ttl = DefaultSignerCacheTTL
	}
	return &transitSigner{
		transit: transit,
		keyName: keyName,
		ttl:     ttl,
		now:     time.Now,
		pubs:    make(map[string]cachedPub),
	}
}

func (t *transitSigner) PublicKey(ctx context.Context, tenantID merchant.ID) (solanago.PublicKey, error) {
	if t.transit == nil {
		return solanago.PublicKey{}, fmt.Errorf("solana: transit signer has no client")
	}
	if tenantID.IsZero() {
		return solanago.PublicKey{}, fmt.Errorf("solana: signing requires a tenant id")
	}
	id := tenantID.String()

	t.mu.Lock()
	if entry, ok := t.pubs[id]; ok && t.now().Before(entry.expiresAt) {
		key := entry.key
		t.mu.Unlock()
		return key, nil
	}
	t.mu.Unlock()

	raw, err := t.transit.PublicKey(ctx, t.keyName(tenantID))
	if err != nil {
		return solanago.PublicKey{}, fmt.Errorf("solana: transit public key: %w", err)
	}
	if len(raw) != 32 {
		return solanago.PublicKey{}, fmt.Errorf("solana: transit public key is %d bytes, want 32", len(raw))
	}
	key := solanago.PublicKeyFromBytes(raw)

	t.mu.Lock()
	t.pubs[id] = cachedPub{key: key, expiresAt: t.now().Add(t.ttl)}
	t.mu.Unlock()

	return key, nil
}

func (t *transitSigner) SignMessage(ctx context.Context, tenantID merchant.ID, message []byte) (solanago.Signature, error) {
	if t.transit == nil {
		return solanago.Signature{}, fmt.Errorf("solana: transit signer has no client")
	}
	if tenantID.IsZero() {
		return solanago.Signature{}, fmt.Errorf("solana: signing requires a tenant id")
	}
	if len(message) == 0 {
		return solanago.Signature{}, fmt.Errorf("solana: cannot sign empty message")
	}
	raw, err := t.transit.Sign(ctx, t.keyName(tenantID), message)
	if err != nil {
		return solanago.Signature{}, fmt.Errorf("solana: transit sign: %w", err)
	}
	if len(raw) != 64 {
		return solanago.Signature{}, fmt.Errorf("solana: transit signature is %d bytes, want 64", len(raw))
	}
	return solanago.SignatureFromBytes(raw), nil
}
