package recurring

import (
	"context"
	"time"

	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/tenancy"
	"github.com/open-rails/openrails/pkg/tenant"
)

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
// + RPC for the given network (mainnet/devnet).
func NewPlanServiceFromStore(store tenancy.TenantSecretStore, rpc *solanaint.RPCClient, network string, ttl time.Duration) *PlanService {
	return NewPlanService(NewSignerSubmitterFromStore(store, rpc, ttl), network)
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
