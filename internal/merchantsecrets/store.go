// Package merchantsecrets builds the runtime merchant secret backend.
package merchantsecrets

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
)

// Store contains the selected merchant secret backend and optional Vault Transit
// client for Solana signing.
type Store struct {
	Secrets       merchants.MerchantSecretStore
	SolanaTransit solanaint.TransitClient
}

// Build returns the canonical runtime merchant secret store for this process.
// Manual/startup bootstrap and the HTTP server both use this path so imported
// provider secrets are written into the same backend runtime reads from.
func Build(ctx context.Context, cfg *config.Config, pool *db.Pool) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("build merchant secret store: db pool is required")
	}
	if cfg != nil && cfg.Vault != nil && cfg.Vault.Enabled {
		vc := cfg.Vault
		vclient, err := vault.Login(ctx, vault.Config{
			Address:    vc.Address,
			AuthMethod: vc.AuthMethod,
			Token:      vc.Token,
			RoleID:     vc.RoleID,
			SecretID:   vc.SecretID,
			K8sRole:    vc.K8sRole,
		})
		if err != nil {
			return nil, fmt.Errorf("vault login: %w", err)
		}
		store := merchants.NewVaultSecretStore(
			vaultKVMount,
			vault.NewKVv2Adapter(vclient, vaultKVMount),
			merchants.NewDBMerchantSlugResolver(pool),
		)
		return &Store{
			Secrets:       merchants.NewCachedSecretStore(store, merchants.DefaultSecretCacheTTL),
			SolanaTransit: vault.NewTransitAdapter(vclient, vaultTransitMount),
		}, nil
	}

	dbStore, err := merchants.NewDBSecretStore(pool)
	if err != nil {
		return nil, fmt.Errorf("build DB merchant secret store: %w", err)
	}
	masterKey := ""
	if cfg != nil && cfg.Encryption != nil {
		masterKey = cfg.Encryption.MasterKey
	}
	dekStore, err := crypto.NewDBDEKStore(pool)
	if err != nil {
		return nil, fmt.Errorf("build merchant DEK store: %w", err)
	}
	enc, err := crypto.NewEncryptor(masterKey, dekStore)
	if err != nil {
		return nil, fmt.Errorf("build merchant encryptor: %w", err)
	}
	store, err := merchants.NewEncryptedSecretStore(dbStore, enc)
	if err != nil {
		return nil, fmt.Errorf("wrap DB merchant secret store with encryption: %w", err)
	}
	if !enc.Enabled() {
		store = merchants.NewWriteRestrictedSecretStore(store, map[string]string{
			solanaint.SecretSolanaPrivateKey: "ENCRYPTION_MASTER_KEY is required before storing DB-backed Solana private keys",
		})
	}
	return &Store{Secrets: merchants.NewCachedSecretStore(store, merchants.DefaultSecretCacheTTL)}, nil
}
