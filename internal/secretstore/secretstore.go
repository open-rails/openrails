// Package secretstore builds the canonical merchant secret store from config.
//
// It is the single source of truth for how provider/processor/Solana secrets are
// protected at rest, used by BOTH the long-lived HTTP server runtime and the
// bootstrap / push-merchant-config provisioning path. Centralizing the wiring
// here closes OR-CFG-001: before this, the provisioning path constructed a raw
// DB store and wrote secrets in plaintext, bypassing the envelope encryption and
// the plaintext-Solana write-block that the runtime read path enforces. With one
// constructor the write path can never store secrets less protected than the read
// path expects.
//
// Layout matches the runtime exactly:
//   - Vault enabled  → Vault KV-v2 backend (encrypts at rest) + Transit signer.
//   - otherwise      → DB-backed store wrapped in per-merchant envelope
//     encryption; when no master key is configured, Solana private keys are
//     write-blocked so recurring signing keys are never newly stored plaintext.
package secretstore

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/crypto"
	"github.com/open-rails/openrails/internal/db"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/merchants"
)

const (
	vaultKVMount      = "secret"
	vaultTransitMount = "transit"

	// solanaPlaintextWriteBlockReason is surfaced when a DB-backed Solana private
	// key write is rejected because envelope encryption is not configured.
	solanaPlaintextWriteBlockReason = "envelope encryption master key is required before storing DB-backed Solana private keys"
)

// Build constructs the merchant secret store and, when Vault is enabled, the
// Solana Transit signing client (nil otherwise). The returned store is NOT
// wrapped in the in-process TTL cache — long-lived callers add that themselves;
// one-shot callers (bootstrap) use it directly.
func Build(ctx context.Context, cfg *config.Config, pool *db.Pool) (merchants.MerchantSecretStore, solanaint.TransitClient, error) {
	if cfg != nil && cfg.Vault != nil && cfg.Vault.Enabled {
		// Managed: Vault KV-v2 backend (#251), same (tenant, name) addressing.
		// Vault encrypts at rest, so no envelope wrapper. Transit keeps the
		// per-tenant Solana key non-extractable.
		vc := cfg.Vault
		vclient, err := vault.Login(ctx, vault.Config{
			Address: vc.Address, AuthMethod: vc.AuthMethod, Token: vc.Token, RoleID: vc.RoleID,
			SecretID: vc.SecretID, K8sRole: vc.K8sRole,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("vault login: %w", err)
		}
		store := merchants.NewVaultSecretStore(vaultKVMount, vault.NewKVv2Adapter(vclient, vaultKVMount), merchants.NewDBMerchantSlugResolver(pool))
		return store, vault.NewTransitAdapter(vclient, vaultTransitMount), nil
	}

	// Self-hosted default: DB-backed store + per-tenant envelope encryption
	// (issue #227). With no master key, most secrets keep the legacy plaintext
	// behavior, but Solana private keys are write-blocked below so recurring
	// signing keys are never newly stored plaintext.
	dbStore, err := merchants.NewDBSecretStore(pool)
	if err != nil {
		return nil, nil, fmt.Errorf("build tenant secret store: %w", err)
	}
	var masterKey string
	if cfg != nil && cfg.Encryption != nil {
		masterKey = cfg.Encryption.MasterKey
	}
	dekStore, err := crypto.NewDBDEKStore(pool)
	if err != nil {
		return nil, nil, fmt.Errorf("build tenant DEK store: %w", err)
	}
	enc, err := crypto.NewEncryptor(masterKey, dekStore)
	if err != nil {
		return nil, nil, fmt.Errorf("build tenant encryptor: %w", err)
	}
	store, err := merchants.NewEncryptedSecretStore(dbStore, enc)
	if err != nil {
		return nil, nil, fmt.Errorf("wrap tenant secret store with encryption: %w", err)
	}
	if !enc.Enabled() {
		blocked := map[string]string{solanaint.SecretSolanaPrivateKey: solanaPlaintextWriteBlockReason}
		store = merchants.NewWriteRestrictedSecretStore(store, blocked)
	}
	return store, nil, nil
}
