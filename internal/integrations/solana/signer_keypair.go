package solana

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/pkg/merchant"
)

// DefaultSignerCacheTTL is the in-process cache lifetime for a resolved merchant
// signing key. 60s (decided) keeps revocation/rotation lag small while removing
// almost all secret-store round-trips on the hot path; the cache is invalidated
// early on a rotation (a changed secret value resolves on the next miss).
const DefaultSignerCacheTTL = 60 * time.Second

// keypairSigner is the "give me the key" Signer: it loads solana/private_key
// from a MerchantSecretGetter, parses it, and signs in-process. Works with any
// secret backend (DB+envelope self-hosted, or Vault KV managed). The plaintext
// key briefly lives in memory (cached up to ttl); for stronger custody of the
// money-signing key, prefer a Vault Transit RemoteSigner where the key never
// leaves Vault.
type keypairSigner struct {
	secrets MerchantSecretGetter
	ttl     time.Duration
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]cachedKey // keyed by merchantID.String()
}

type cachedKey struct {
	key       solanago.PrivateKey
	expiresAt time.Time
}

// NewKeypairSigner returns a per-merchant in-process signer backed by secrets. A
// zero ttl uses DefaultSignerCacheTTL.
func NewKeypairSigner(secrets MerchantSecretGetter, ttl time.Duration) Signer {
	if ttl <= 0 {
		ttl = DefaultSignerCacheTTL
	}
	return &keypairSigner{
		secrets: secrets,
		ttl:     ttl,
		now:     time.Now,
		cache:   make(map[string]cachedKey),
	}
}

// load resolves and caches the merchant's private key. Cache hits avoid the
// secret-store round-trip; misses fetch, parse, and cache with a ttl expiry.
func (k *keypairSigner) load(ctx context.Context, merchantID merchant.ID) (solanago.PrivateKey, error) {
	if k.secrets == nil {
		return nil, fmt.Errorf("solana: keypair signer has no secret store")
	}
	if merchantID.IsZero() {
		return nil, fmt.Errorf("solana: signing requires a merchant id")
	}

	id := merchantID.String()

	k.mu.Lock()
	if entry, ok := k.cache[id]; ok && k.now().Before(entry.expiresAt) {
		key := entry.key
		k.mu.Unlock()
		return key, nil
	}
	k.mu.Unlock()

	raw, err := k.secrets.GetSecret(ctx, merchantID, SecretSolanaPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("solana: load merchant signing key: %w", err)
	}
	key, err := solanago.PrivateKeyFromBase58(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("solana: parse merchant signing key: %w", err)
	}

	k.mu.Lock()
	k.cache[id] = cachedKey{key: key, expiresAt: k.now().Add(k.ttl)}
	k.mu.Unlock()

	return key, nil
}

func (k *keypairSigner) PublicKey(ctx context.Context, merchantID merchant.ID) (solanago.PublicKey, error) {
	key, err := k.load(ctx, merchantID)
	if err != nil {
		return solanago.PublicKey{}, err
	}
	return key.PublicKey(), nil
}

func (k *keypairSigner) SignMessage(ctx context.Context, merchantID merchant.ID, message []byte) (solanago.Signature, error) {
	if len(message) == 0 {
		return solanago.Signature{}, fmt.Errorf("solana: cannot sign empty message")
	}
	key, err := k.load(ctx, merchantID)
	if err != nil {
		return solanago.Signature{}, err
	}
	return key.Sign(message)
}
