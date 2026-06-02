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

// NewSignerSubmitterFromStore builds the production Submitter: a per-tenant
// keypair signer reading solana/private_key from the tenant secret store, wired
// to the Solana RPC. ttl 0 uses the default signer cache TTL.
func NewSignerSubmitterFromStore(store tenancy.TenantSecretStore, rpc *solanaint.RPCClient, ttl time.Duration) Submitter {
	signer := solanaint.NewKeypairSigner(secretStoreGetter{store: store}, ttl)
	return NewSignerSubmitter(signer, rpc)
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
