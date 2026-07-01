// Package merchantsecrets builds the runtime merchant secret backend.
package merchantsecrets

import (
	"context"
	"fmt"

	vaultapi "github.com/hashicorp/vault/api"
	log "github.com/sirupsen/logrus"

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

// Store contains the selected merchant secret backend, the optional Vault Transit
// client for Solana signing, and the probed Vault capabilities (#661). Capabilities
// gate which operations/routes light up — they are advisory, never authorization.
type Store struct {
	Secrets       merchants.MerchantSecretStore
	SolanaTransit solanaint.TransitClient
	Capabilities  vault.Capabilities
	// Derived route-gating signals (#661). Advisory only — they hide/degrade
	// routes, never authorize. SolanaCanSign: a Vault connection OR a local Solana
	// key is supported. SecretWrite: provider-secret writes / config-push are possible.
	SolanaCanSign bool
	SecretWrite   bool
}

// deriveRouteGates computes the advisory route-gating signals. localKeys: the
// store can hold+serve a Solana keypair (Vault KV read, or the DB store with
// encryption enabled). A Vault connection counts as can-sign: transit key names
// are operator-chosen so there is no path to probe — the real key is read at
// provision time and runtime 403 stays the boundary.
func deriveRouteGates(useVault, vaultConnected, encryptionEnabled bool, caps vault.Capabilities) (solanaCanSign, secretWrite bool) {
	localKeys := (useVault && caps.KVRead) || (!useVault && encryptionEnabled)
	solanaCanSign = vaultConnected || localKeys
	secretWrite = (useVault && caps.KVWrite) || !useVault
	return solanaCanSign, secretWrite
}

// Build returns the canonical runtime merchant secret store for this process.
// Manual/startup bootstrap and the HTTP server both use this path so imported
// provider secrets are written into the same backend runtime reads from.
//
// The KV secret backend and Vault Transit signing are INDEPENDENT (#661): where
// secrets live is declared intent (secret_backend: db|vault, never auto-fallback),
// while what the token may actually do is capability-driven. A transit-only policy
// yields signing with zero KV access; a KV policy yields the secret store; both can
// coexist.
func Build(ctx context.Context, cfg *config.Config, pool *db.Pool) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("build merchant secret store: db pool is required")
	}
	backend := cfg.SecretStoreBackend()

	// Open a Vault connection whenever Vault is configured, then probe what the
	// token may actually do. The connection may serve KV, Transit, both, or (with a
	// transit-only policy) only signing.
	var (
		vclient *vaultapi.Client
		transit solanaint.TransitClient
		caps    vault.Capabilities
	)
	if cfg != nil && cfg.Vault != nil && cfg.Vault.Enabled {
		vc := cfg.Vault
		client, err := vault.Login(ctx, vault.Config{
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
		caps, err = vault.SelfCapabilities(ctx, client, vaultKVMount)
		if err != nil {
			// Only fatal when secrets are declared to live in Vault: the KV store
			// can't be verified. Otherwise degrade — transit signing doesn't need
			// the probe (runtime 403 is the boundary).
			if backend == config.SecretBackendVault {
				return nil, fmt.Errorf("vault capability probe: %w", err)
			}
			log.WithError(err).Warn("vault: capability probe failed; continuing (secret_backend=db, Vault used for transit signing only)")
		}
		vclient = client
		transit = vault.NewTransitAdapter(client, vaultTransitMount)
	}

	// Secret store per DECLARED backend — never auto-fallback (the data lives in one
	// place; a store that lacks it would run silently empty).
	useVault, err := gateSecretBackend(backend, vclient != nil, caps)
	if err != nil {
		return nil, err
	}
	encryptionEnabled := cfg != nil && cfg.Encryption != nil && cfg.Encryption.MasterKey != ""
	solanaCanSign, secretWrite := deriveRouteGates(useVault, vclient != nil, encryptionEnabled, caps)
	if useVault {
		if !caps.KVWrite {
			log.Warn("vault: secret_backend=vault with read-only KV capability; merchant-secret writes / config-push are disabled")
		}
		store := merchants.NewVaultSecretStore(
			vaultKVMount,
			vault.NewKVv2Adapter(vclient, vaultKVMount),
			merchants.NewDBMerchantSlugResolver(pool),
		)
		return &Store{
			Secrets:       merchants.NewCachedSecretStore(store, merchants.DefaultSecretCacheTTL),
			SolanaTransit: transit,
			Capabilities:  caps,
			SolanaCanSign: solanaCanSign,
			SecretWrite:   secretWrite,
		}, nil
	}

	secrets, err := buildDBSecretStore(cfg, pool)
	if err != nil {
		return nil, err
	}
	return &Store{
		Secrets:       secrets,
		SolanaTransit: transit,
		Capabilities:  caps,
		SolanaCanSign: solanaCanSign,
		SecretWrite:   secretWrite,
	}, nil
}

// gateSecretBackend decides whether to serve the DECLARED backend from Vault KV,
// given the probed capabilities. It errors on the one unrecoverable case — secrets
// declared in Vault but the token can't read KV — and callers must NOT auto-fallback
// to the DB store (the data lives in Vault, not the DB, so DB would be empty).
func gateSecretBackend(backend string, vaultConnected bool, caps vault.Capabilities) (useVault bool, err error) {
	if backend != config.SecretBackendVault {
		return false, nil
	}
	if !vaultConnected {
		return false, fmt.Errorf("secret_backend=vault requires vault.enabled (no Vault connection to serve the KV store)")
	}
	if !caps.KVRead {
		return false, fmt.Errorf("secret_backend=vault but the Vault token cannot read the KV mount %q; grant KV read or set secret_backend=db", vaultKVMount)
	}
	return true, nil
}

// buildDBSecretStore builds the DEK-encrypted Postgres merchant secret store (the
// default backend, and the one used with a transit-only or no-KV Vault policy).
func buildDBSecretStore(cfg *config.Config, pool *db.Pool) (merchants.MerchantSecretStore, error) {
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
			"provider_accounts/solana/*/*/private_key": "ENCRYPTION_MASTER_KEY is required before storing DB-backed Solana private keys",
		})
	}
	return merchants.NewCachedSecretStore(store, merchants.DefaultSecretCacheTTL), nil
}
