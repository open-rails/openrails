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
	// DefaultVaultKVMount is used when config.VaultConfig.KVMount is empty —
	// unchanged from the previous unconditional constant, so existing
	// deployments that never set KVMount keep their current behavior.
	DefaultVaultKVMount = "secret"
	// DefaultVaultTransitMount is used when config.VaultConfig.TransitMount is
	// empty — unchanged from the previous unconditional constant.
	DefaultVaultTransitMount = "transit"
)

// resolveVaultKVMount applies the config.VaultConfig.KVMount default. vc may
// be nil (Vault disabled).
func resolveVaultKVMount(vc *config.VaultConfig) string {
	if vc == nil || vc.KVMount == "" {
		return DefaultVaultKVMount
	}
	return vc.KVMount
}

// resolveVaultTransitMount applies the config.VaultConfig.TransitMount
// default. vc may be nil (Vault disabled).
func resolveVaultTransitMount(vc *config.VaultConfig) string {
	if vc == nil || vc.TransitMount == "" {
		return DefaultVaultTransitMount
	}
	return vc.TransitMount
}

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
	// VaultAuth is the #751 auth-health probe for the Vault client backing
	// this store: nil when Vault isn't enabled, non-nil (call .AuthState())
	// otherwise. Ping folds it in so readiness sees auth death too.
	VaultAuth *vault.Supervisor
	// vclient is the authenticated Vault client Build logged in with, kept ONLY
	// when Vault actually serves the merchant-secret KV store (secret_backend=vault).
	// nil for the DB-backed store and for BuildManifest — Ping is then a no-op,
	// since those backends have no separate liveness signal beyond the runtime DB
	// ping / in-memory plane (#748 Ready()).
	vclient *vaultapi.Client
	// kvMount is the resolved KV-v2 mount (config.VaultConfig.KVMount, or
	// DefaultVaultKVMount) Build probed vclient against — Ping re-probes the
	// same mount, never the package default directly, so a non-default mount
	// stays correct post-construction.
	kvMount string
}

// Ping reports whether the merchant-secret backend is usable RIGHT NOW — the
// live counterpart to Build's construction-time capability probe (#748).
// DB-backed stores need no separate check (nil receiver client -> always nil,
// the runtime DB ping already covers them). Vault-backed stores first check
// the #751 auth supervisor (a dead/unrecoverable token is a readiness failure
// even while the server is reachable), then re-run the same
// sys/capabilities-self probe Build used, on the SAME authenticated client, so
// a Vault that goes unreachable/sealed/paused AFTER boot is caught by
// readiness instead of only surfacing at request time.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.vclient == nil {
		return nil
	}
	if s.VaultAuth != nil {
		if err := s.VaultAuth.AuthState(); err != nil {
			return fmt.Errorf("vault auth: %w", err)
		}
	}
	if _, err := vault.SelfCapabilities(ctx, s.vclient, s.kvMount); err != nil {
		return fmt.Errorf("vault unreachable: %w", err)
	}
	return nil
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
	// MODE 1 (#723): no persistent merchant-secret store exists. Callers must
	// serve the runtime's in-memory manifest plane via BuildManifest instead —
	// constructing a DB/Vault store here would run silently empty.
	if cfg.IsManifestMerchantSource() {
		return nil, fmt.Errorf("merchant_source=manifest constructs no merchant-secret store (#723): credentials live in memory from the boot manifest; use BuildManifest over Runtime.ManifestSecrets")
	}
	backend := cfg.SecretStoreBackend()

	// Open a Vault connection whenever Vault is configured, then probe what the
	// token may actually do. The connection may serve KV, Transit, both, or (with a
	// transit-only policy) only signing.
	var (
		vclient   *vaultapi.Client
		transit   solanaint.TransitClient
		caps      vault.Capabilities
		vaultAuth *vault.Supervisor
	)
	kvMount := resolveVaultKVMount(cfg.Vault)
	if cfg != nil && cfg.Vault != nil && cfg.Vault.Enabled {
		vc := cfg.Vault
		client, sup, err := vault.Login(ctx, vault.Config{
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
		vaultAuth = sup
		caps, err = vault.SelfCapabilities(ctx, client, kvMount)
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
		transit = vault.NewTransitAdapter(client, resolveVaultTransitMount(vc))
	}

	// Secret store per DECLARED backend — never auto-fallback (the data lives in one
	// place; a store that lacks it would run silently empty).
	useVault, err := gateSecretBackend(backend, vclient != nil, caps, kvMount)
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
			kvMount,
			vault.NewKVv2Adapter(vclient, kvMount).WithReauthTrigger(vaultAuth),
			merchants.NewDBMerchantSlugResolver(pool),
		)
		return &Store{
			Secrets:       merchants.NewCachedSecretStore(store, merchants.DefaultSecretCacheTTL),
			SolanaTransit: transit,
			Capabilities:  caps,
			SolanaCanSign: solanaCanSign,
			SecretWrite:   secretWrite,
			VaultAuth:     vaultAuth,
			vclient:       vclient,
			kvMount:       kvMount,
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
		VaultAuth:     vaultAuth,
	}, nil
}

// gateSecretBackend decides whether to serve the DECLARED backend from Vault KV,
// given the probed capabilities. It errors on the one unrecoverable case — secrets
// declared in Vault but the token can't read KV — and callers must NOT auto-fallback
// to the DB store (the data lives in Vault, not the DB, so DB would be empty).
func gateSecretBackend(backend string, vaultConnected bool, caps vault.Capabilities, kvMount string) (useVault bool, err error) {
	if backend != config.SecretBackendVault {
		return false, nil
	}
	if !vaultConnected {
		return false, fmt.Errorf("secret_backend=vault requires vault.enabled (no Vault connection to serve the KV store)")
	}
	if !caps.KVRead {
		return false, fmt.Errorf("secret_backend=vault but the Vault token cannot read the KV mount %q; grant KV read or set secret_backend=db", kvMount)
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
	if err := enforceEncryptionPosture(enc.Enabled(), cfg.RequiresSecretEncryption()); err != nil {
		return nil, err
	}
	store, err := merchants.NewEncryptedSecretStore(dbStore, enc)
	if err != nil {
		return nil, fmt.Errorf("wrap DB merchant secret store with encryption: %w", err)
	}
	// Only reachable in dev post-#667; Solana keys are self-custody funds, so
	// plaintext is refused even where dev deliberately allows it for the rest.
	if !enc.Enabled() {
		store = merchants.NewWriteRestrictedSecretStore(store, map[string]string{
			merchants.SolanaPrivateKeyWritePattern(): "ENCRYPTION_MASTER_KEY is required before storing DB-backed Solana private keys",
		})
	}
	return merchants.NewCachedSecretStore(store, merchants.DefaultSecretCacheTTL), nil
}

// BuildManifest returns the MODE-1 (#723) store view: the runtime store is the
// in-memory manifest plane (read-only; the manifest IS the store), and Vault —
// when enabled — serves Transit signing ONLY (KV is never consulted; #661's
// independence of KV vs Transit carries over). SolanaCanSign is true: the
// memory store can hold+serve a manifest-declared keypair, and a Vault
// connection adds transit. SecretWrite stays true so the provider-config write
// routes MOUNT and serve the pointed 405 mode rejection instead of a bare 404.
func BuildManifest(ctx context.Context, cfg *config.Config, store *merchants.ManifestSecretStore) (*Store, error) {
	if store == nil {
		return nil, fmt.Errorf("build manifest secret plane: store is required")
	}
	transitStore, err := BuildTransit(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Store{
		Secrets:       store,
		SolanaTransit: transitStore.SolanaTransit,
		SolanaCanSign: true,
		SecretWrite:   true,
		VaultAuth:     transitStore.VaultAuth,
		vclient:       transitStore.vclient,
	}, nil
}

// BuildTransit opens the Vault Transit signing client when Vault is enabled
// (a zero-value *Store, SolanaTransit nil, otherwise). Standalone of the KV
// store — MODE 1 uses it for Solana vault_transit signers with no KV backend
// at all.
//
// Threaded like the KV path in Build (#751 follow-up): the Supervisor from
// vault.Login is kept on the returned Store (VaultAuth) and vclient is kept
// too, so Store.Ping folds in auth-health + reachability for transit-only
// connections exactly as it does for the KV path — callers that only need the
// transit client read Store.SolanaTransit; callers that also want liveness
// call Store.Ping.
func BuildTransit(ctx context.Context, cfg *config.Config) (*Store, error) {
	if cfg == nil || cfg.Vault == nil || !cfg.Vault.Enabled {
		return &Store{}, nil
	}
	vc := cfg.Vault
	client, sup, err := vault.Login(ctx, vault.Config{
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
	return &Store{
		SolanaTransit: vault.NewTransitAdapter(client, resolveVaultTransitMount(vc)),
		VaultAuth:     sup,
		vclient:       client,
	}, nil
}

// enforceEncryptionPosture is the #667 boot gate on the DB-backed (fallback)
// secret store, mirroring db.EnforceRLSPosture: outside development a disabled
// encryptor refuses boot (secrets would persist plaintext at rest); development
// proceeds with one loud warning. Vault-backed deployments never reach this.
func enforceEncryptionPosture(encryptionEnabled, require bool) error {
	if encryptionEnabled {
		return nil
	}
	if require {
		return fmt.Errorf("merchant secrets: secret_backend=db with no ENCRYPTION_MASTER_KEY would persist merchant provider credentials PLAINTEXT in openrails.merchant_secrets; set ENCRYPTION_MASTER_KEY (base64 32-byte AES-256 key) or store secrets in Vault (secret_backend=vault); only development may run without")
	}
	log.Warn("merchant secrets: ENCRYPTION_MASTER_KEY is not set; DB-backed merchant secrets will persist PLAINTEXT at rest (development only — non-development boots refuse this posture, #667)")
	return nil
}
