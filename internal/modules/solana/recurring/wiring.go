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
	"github.com/open-rails/openrails/internal/tenancy"
	"github.com/open-rails/openrails/pkg/tenant"
)

// SeedConfiguredTenantSolanaSecret bridges a SINGLE-TENANT install that
// configures its Solana signing key via global config into the per-tenant
// secret store the recurring Solana services read (issue #253). The
// Submitter/signer always reads solana/private_key from the tenant secret store;
// a global-config-only install would otherwise have an EMPTY store for its
// tenant and could not sign without a manual secret write.
//
// tenantID is the engine's configured tenant (#336 — there is no default
// tenant). It is idempotent and safe to call on every boot:
//
//   - tenantID zero          -> no-op (no configured tenant to seed for).
//   - configKey empty        -> no-op (Solana not configured via global config).
//   - secret already present -> no-op (NEVER overwrites a manual/rotated key).
//   - secret absent          -> seed it from configKey and log once.
//
// A backend error (store unavailable) is returned so the caller can fail boot
// loudly rather than start a signer that will never resolve a key.
func SeedConfiguredTenantSolanaSecret(ctx context.Context, store tenancy.TenantSecretStore, tenantID tenant.ID, configKey string) error {
	if tenantID.IsZero() {
		return nil
	}
	return SeedTenantSolanaSecret(ctx, store, tenantID, configKey)
}

func SeedTenantSolanaSecret(ctx context.Context, store tenancy.TenantSecretStore, tenantID tenant.ID, configKey string) error {
	key := strings.TrimSpace(configKey)
	if key == "" {
		return nil // nothing configured via global config
	}
	if store == nil {
		return fmt.Errorf("recurring: seed tenant solana secret: nil secret store")
	}
	_, err := store.Get(ctx, tenantID, solanaint.SecretSolanaPrivateKey)
	switch {
	case err == nil:
		return nil // already present — never overwrite an existing secret
	case errors.Is(err, tenancy.ErrSecretNotFound):
		// fall through to seed
	default:
		return fmt.Errorf("recurring: check tenant solana secret: %w", err)
	}
	if _, err := store.Put(ctx, tenantID, solanaint.SecretSolanaPrivateKey, key); err != nil {
		return fmt.Errorf("recurring: seed tenant solana secret: %w", err)
	}
	log.WithField("tenant", tenantID.String()).
		Info("seeded tenant Solana signing key from global config (solana/private_key)")
	return nil
}

// secretStoreGetter adapts the per-tenant tenancy.TenantSecretStore to the
// solana.TenantSecretGetter the keypair signer needs. The Solana signing key is
// the secret named solana/private_key; the store backend (DB+envelope or Vault)
// is chosen at the composition root and is transparent here.
type secretStoreGetter struct {
	store tenancy.TenantSecretStore
}

func (g secretStoreGetter) GetSecret(ctx context.Context, tenantID tenant.ID, name string) (string, error) {
	sec, err := g.store.Get(ctx, tenantID, name)
	if err != nil {
		return "", err
	}
	return sec.Value, nil
}

// NewSignerFromStore builds the per-tenant keypair signer reading
// solana/private_key from the tenant secret store. Exposed (alongside the
// Submitter constructors) so the composition root can share the SAME signer
// between the Submitter-backed services and the signer-backed
// PrepareTierChangeService (#272), which co-signs with the merchant key directly.
func NewSignerFromStore(store tenancy.TenantSecretStore, ttl time.Duration) solanaint.Signer {
	return solanaint.NewKeypairSigner(secretStoreGetter{store: store}, ttl)
}

// NewSignerFromTransit builds the per-tenant signer whose key lives in Vault
// Transit (non-extractable). Shared with PrepareTierChangeService (#272).
func NewSignerFromTransit(transit solanaint.TransitClient, ttl time.Duration) solanaint.Signer {
	return solanaint.NewTransitSigner(transit, nil, ttl)
}

// NewSignerSubmitterFromStore builds the production Submitter: a per-tenant
// keypair signer reading solana/private_key from the tenant secret store, wired
// to the Solana RPC. ttl 0 uses the default signer cache TTL.
func NewSignerSubmitterFromStore(store tenancy.TenantSecretStore, rpc *solanaint.RPCClient, ttl time.Duration) Submitter {
	return NewSignerSubmitter(NewSignerFromStore(store, ttl), rpc)
}

// NewCrankServiceFromStore builds a CrankService backed by the tenant secret
// store + RPC — the value the composition root injects into the cranker worker.
func NewCrankServiceFromStore(store tenancy.TenantSecretStore, rpc *solanaint.RPCClient, ttl time.Duration) *CrankService {
	return NewCrankService(NewSignerSubmitterFromStore(store, rpc, ttl))
}

// NewPlanServiceFromStore builds a PlanService backed by the tenant secret store
// + RPC for the given network (mainnet/devnet). The same RPC client serves as the
// plan reader, enabling the idempotent re-publish guard (#254).
func NewPlanServiceFromStore(store tenancy.TenantSecretStore, rpc *solanaint.RPCClient, network string, tokens map[string]config.SolanaToken, ttl time.Duration) *PlanService {
	return NewPlanServiceWithReader(NewSignerSubmitterFromStore(store, rpc, ttl), rpc, network, tokens)
}

// NewSignerSubmitterFromTransit builds a Submitter whose signing key lives in
// Vault Transit (non-extractable) — the key never enters this process.
func NewSignerSubmitterFromTransit(transit solanaint.TransitClient, rpc *solanaint.RPCClient, ttl time.Duration) Submitter {
	return NewSignerSubmitter(NewSignerFromTransit(transit, ttl), rpc)
}

// NewCrankServiceFromTransit builds a CrankService whose per-tenant Solana key is
// signed via Vault Transit.
func NewCrankServiceFromTransit(transit solanaint.TransitClient, rpc *solanaint.RPCClient, ttl time.Duration) *CrankService {
	return NewCrankService(NewSignerSubmitterFromTransit(transit, rpc, ttl))
}
