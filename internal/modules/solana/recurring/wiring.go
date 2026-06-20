package recurring

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// SeedConfiguredMerchantSolanaSecret bridges a single-merchant install that
// configures its Solana signing key via global config into the per-merchant
// secret store the recurring Solana services read (issue #253). The
// Submitter/signer always reads solana/private_key from the merchant secret store;
// a global-config-only install would otherwise have an EMPTY store for its
// merchant and could not sign without a manual secret write.
//
// merchantID is the engine's configured merchant (#336 — there is no default
// merchant). It is idempotent and safe to call on every boot:
//
//   - merchantID zero        -> no-op (no configured merchant to seed for).
//   - configKey empty        -> no-op (Solana not configured via global config).
//   - secret already present -> no-op (NEVER overwrites a manual/rotated key).
//   - secret absent          -> seed it from configKey and log once.
//
// A backend error (store unavailable) is returned so the caller can fail boot
// loudly rather than start a signer that will never resolve a key.
func SeedConfiguredMerchantSolanaSecret(ctx context.Context, store merchants.MerchantSecretStore, merchantID merchant.ID, configKey string) error {
	if merchantID.IsZero() {
		return nil
	}
	return SeedMerchantSolanaSecret(ctx, store, merchantID, configKey)
}

func SeedMerchantSolanaSecret(ctx context.Context, store merchants.MerchantSecretStore, merchantID merchant.ID, configKey string) error {
	key := strings.TrimSpace(configKey)
	if key == "" {
		return nil // nothing configured via global config
	}
	if store == nil {
		return fmt.Errorf("recurring: seed merchant solana secret: nil secret store")
	}
	_, err := store.Get(ctx, merchantID, solanaint.SecretSolanaPrivateKey)
	switch {
	case err == nil:
		return nil // already present — never overwrite an existing secret
	case errors.Is(err, merchants.ErrSecretNotFound):
		// fall through to seed
	default:
		return fmt.Errorf("recurring: check merchant solana secret: %w", err)
	}
	if _, err := store.Put(ctx, merchantID, solanaint.SecretSolanaPrivateKey, key); err != nil {
		return fmt.Errorf("recurring: seed merchant solana secret: %w", err)
	}
	log.WithField("merchant", merchantID.String()).
		Info("seeded merchant Solana signing key from global config (solana/private_key)")
	return nil
}

// secretStoreGetter adapts the per-merchant merchants.MerchantSecretStore to the
// solana.MerchantSecretGetter the keypair signer needs. The Solana signing key is
// the secret named solana/private_key; the store backend (DB+envelope or Vault)
// is chosen at the composition root and is transparent here.
type secretStoreGetter struct {
	store merchants.MerchantSecretStore
}

func (g secretStoreGetter) GetSecret(ctx context.Context, merchantID merchant.ID, name string) (string, error) {
	sec, err := g.store.Get(ctx, merchantID, name)
	if err != nil {
		return "", err
	}
	return sec.Value, nil
}

// NewSignerFromStore builds the per-merchant keypair signer reading
// solana/private_key from the merchant secret store. Exposed (alongside the
// Submitter constructors) so the composition root can share the SAME signer
// between the Submitter-backed services and the signer-backed
// PrepareTierChangeService (#272), which co-signs with the merchant key directly.
func NewSignerFromStore(store merchants.MerchantSecretStore, ttl time.Duration) solanaint.Signer {
	return solanaint.NewKeypairSigner(secretStoreGetter{store: store}, ttl)
}

// NewSignerFromTransit builds the per-merchant signer whose key lives in Vault
// Transit (non-extractable). Shared with PrepareTierChangeService (#272).
func NewSignerFromTransit(transit solanaint.TransitClient, ttl time.Duration) solanaint.Signer {
	return solanaint.NewTransitSigner(transit, nil, ttl)
}

// NewSignerSubmitterFromStore builds the production Submitter: a per-merchant
// keypair signer reading solana/private_key from the merchant secret store, wired
// to the Solana RPC. ttl 0 uses the default signer cache TTL.
func NewSignerSubmitterFromStore(store merchants.MerchantSecretStore, rpc *solanaint.RPCClient, ttl time.Duration) Submitter {
	return NewSignerSubmitter(NewSignerFromStore(store, ttl), rpc)
}

// NewCrankServiceFromStore builds a CrankService backed by the merchant secret
// store + RPC — the value the composition root injects into the cranker worker.
func NewCrankServiceFromStore(store merchants.MerchantSecretStore, rpc *solanaint.RPCClient, ttl time.Duration) *CrankService {
	return NewCrankService(NewSignerSubmitterFromStore(store, rpc, ttl))
}

// NewPlanServiceFromStore builds a PlanService backed by the merchant secret store
// + RPC for the given network (mainnet/devnet). The same RPC client serves as the
// plan reader, enabling the idempotent re-publish guard (#254).
func NewPlanServiceFromStore(store merchants.MerchantSecretStore, rpc *solanaint.RPCClient, network string, tokens map[string]config.TokenConfig, ttl time.Duration) *PlanService {
	return NewPlanServiceWithReader(NewSignerSubmitterFromStore(store, rpc, ttl), rpc, network, tokens)
}

// NewSignerSubmitterFromTransit builds a Submitter whose signing key lives in
// Vault Transit (non-extractable) — the key never enters this process.
func NewSignerSubmitterFromTransit(transit solanaint.TransitClient, rpc *solanaint.RPCClient, ttl time.Duration) Submitter {
	return NewSignerSubmitter(NewSignerFromTransit(transit, ttl), rpc)
}

// NewCrankServiceFromTransit builds a CrankService whose per-merchant Solana key is
// signed via Vault Transit.
func NewCrankServiceFromTransit(transit solanaint.TransitClient, rpc *solanaint.RPCClient, ttl time.Duration) *CrankService {
	return NewCrankService(NewSignerSubmitterFromTransit(transit, rpc, ttl))
}
