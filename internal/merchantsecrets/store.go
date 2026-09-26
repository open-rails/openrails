// Package merchantsecrets builds the runtime merchant secret backend.
package merchantsecrets

import (
	"context"
	"fmt"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/crypto"
	"github.com/open-rails/openrails/internal/db"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/retry"
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

// Store contains the selected merchant secret backend, the optional Vault
// Transit client for Solana signing, and the assumed Vault capabilities (#661).
// Capabilities gate which routes light up; they never authorize (Vault's
// runtime 403 is the boundary).
type Store struct {
	closeAuth     context.CancelFunc
	Secrets       merchants.MerchantSecretStore
	SolanaTransit solanaint.TransitClient
	Capabilities  vault.Capabilities
	// SolanaCanSign: a Vault connection OR a local Solana key is supported.
	// SecretWrite: provider-secret writes / config-push are possible.
	SolanaCanSign bool
	SecretWrite   bool
	// VaultAuth supervises the owned Vault login in the background; nil when
	// Vault is disabled or the client is borrowed from the host.
	VaultAuth *vault.Supervisor
}

// Close stops only authentication supervision owned by this runtime. It never
// revokes a borrowed host token or closes a borrowed client.
func (s *Store) Close() {
	if s != nil && s.closeAuth != nil {
		s.closeAuth()
	}
}

// State is the cached Vault auth state: nil when no owned Vault is in use or
// it is authenticated. It never touches the network.
func (s *Store) State() error {
	if s == nil {
		return nil
	}
	return s.VaultAuth.AuthState()
}

// Await waits up to timeout for the background Vault login. One-off tools
// (bootstrap, pull-provider) use it; a serving process never waits.
func (s *Store) Await(ctx context.Context, timeout time.Duration) error {
	if s == nil {
		return nil
	}
	if err := s.VaultAuth.Wait(ctx, timeout); err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	return nil
}

// AwaitTimeout bounds how long a one-off tool waits for Vault.
const AwaitTimeout = 30 * time.Second

// Probe is the live Vault check for a host dependency supervisor; nil when no
// owned Vault is in use.
func (s *Store) Probe(ctx context.Context) error {
	if s == nil {
		return nil
	}
	return s.VaultAuth.Probe(ctx)
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
// BuildOptions are runtime-owned credential dependencies. Borrowed Vault clients
// remain authenticated and lifecycle-managed by their host.
type BuildOptions struct {
	Snapshot     *merchants.ManifestSecretStore
	VaultClient  *vaultapi.Client
	ReadOnly     bool
	AlertBackend string
}

func Build(ctx context.Context, cfg *config.Config, pool *db.Pool, options ...BuildOptions) (*Store, error) {
	if pool == nil || cfg == nil {
		return nil, fmt.Errorf("build merchant secret store: config and db pool are required")
	}
	opts := BuildOptions{ReadOnly: cfg.CredentialReadOnly, AlertBackend: cfg.AlertSecretBackend}
	if len(options) > 1 {
		return nil, fmt.Errorf("one credential options value is required")
	}
	if len(options) == 1 {
		supplied := options[0]
		if supplied.Snapshot != nil {
			opts.Snapshot = supplied.Snapshot
		}
		opts.VaultClient = supplied.VaultClient
		opts.ReadOnly = opts.ReadOnly || supplied.ReadOnly
		if supplied.AlertBackend != "" {
			opts.AlertBackend = supplied.AlertBackend
		}
	}
	if cfg.SecretStoreBackend() != "snapshot" {
		return buildManaged(ctx, cfg, pool, opts)
	}
	if opts.Snapshot == nil {
		var err error
		opts.Snapshot, err = merchants.NewManifestSecretStoreWithIdentity(cfg.CredentialSnapshotID)
		if err != nil {
			return nil, err
		}
	}
	var alert merchants.MerchantSecretStore = merchants.NewReadOnlySecretStore(merchants.NewMemorySecretStore())
	var result *Store
	if opts.AlertBackend != "" {
		if opts.AlertBackend != config.SecretBackendDB && opts.AlertBackend != config.SecretBackendVault {
			return nil, fmt.Errorf("invalid alert credential backend")
		}
		copy := *cfg
		copy.SecretBackend = opts.AlertBackend
		var err error
		result, err = buildManaged(ctx, &copy, pool, BuildOptions{VaultClient: opts.VaultClient})
		if err != nil {
			return nil, err
		}
		alert = result.Secrets
	} else {
		var err error
		if opts.VaultClient != nil {
			result = &Store{SolanaTransit: vault.NewTransitAdapter(opts.VaultClient, resolveVaultTransitMount(cfg.Vault))}
		} else {
			result, err = BuildTransit(ctx, cfg)
		}
		if err != nil {
			return nil, err
		}
	}
	result.Secrets = merchants.NewManifestManagedSecretStore(opts.Snapshot, alert)
	result.SecretWrite = false
	result.SolanaCanSign = true
	return result, nil
}

// buildManaged constructs the existing persistent secret backend. Manifest
// deployments use it only for the explicitly routed alert-webhook namespace.
func buildManaged(ctx context.Context, cfg *config.Config, pool *db.Pool, options ...BuildOptions) (result *Store, buildErr error) {
	var ownedCancel context.CancelFunc
	defer func() {
		if buildErr != nil && ownedCancel != nil {
			ownedCancel()
		}
	}()
	var opts BuildOptions
	if len(options) > 0 {
		opts = options[0]
	}
	backend := cfg.SecretStoreBackend()

	// Open a Vault connection whenever Vault is configured. Login runs in the
	// background (only Postgres may block construction); capabilities are
	// assumed from the declared backend and verified once authenticated.
	var (
		vclient   *vaultapi.Client
		transit   solanaint.TransitClient
		caps      vault.Capabilities
		vaultAuth *vault.Supervisor
	)
	kvMount := DefaultVaultKVMount
	if opts.VaultClient != nil || (cfg != nil && cfg.Vault != nil && cfg.Vault.Enabled) {
		vc := cfg.Vault
		if vc == nil {
			vc = &config.VaultConfig{}
		}
		kvMount = resolveVaultKVMount(vc)
		client := opts.VaultClient
		if client == nil {
			authCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			ownedCancel = cancel
			var err error
			client, vaultAuth, err = vault.Login(authCtx, vaultConfig(vc))
			if err != nil {
				return nil, fmt.Errorf("vault login: %w", err)
			}
			if backend == config.SecretBackendVault {
				go verifyCapabilities(authCtx, client, vaultAuth, kvMount, vc.ScopePrefix)
			}
		}
		if backend == config.SecretBackendVault {
			caps = vault.Capabilities{KVRead: true, KVWrite: true}
		}
		vclient = client
		transit = vault.NewTransitAdapter(client, resolveVaultTransitMount(vc)).WithSupervisor(vaultAuth)
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
		prefix := ""
		if cfg.Vault != nil {
			prefix = cfg.Vault.ScopePrefix
		}
		store, err := merchants.NewVaultSecretStoreWithPrefix(kvMount, prefix, vault.NewKVv2Adapter(vclient, kvMount).WithReauthTrigger(vaultAuth))
		if err != nil {
			return nil, err
		}
		if opts.ReadOnly || !caps.KVWrite {
			store = merchants.NewReadOnlySecretStore(store)
		}
		database, err := db.NewWithPGXPool(pool.Raw(), pool.Schema())
		if err != nil {
			return nil, err
		}
		return &Store{
			closeAuth:     ownedCancel,
			Secrets:       merchants.NewLifecycleSecretStore(database, store),
			SolanaTransit: transit,
			Capabilities:  caps,
			SolanaCanSign: solanaCanSign,
			SecretWrite:   secretWrite && !opts.ReadOnly,
			VaultAuth:     vaultAuth,
		}, nil
	}

	secrets, err := buildDBSecretStore(cfg, pool)
	if err != nil {
		return nil, err
	}
	if opts.ReadOnly {
		secrets = merchants.NewReadOnlySecretStore(secrets)
	}
	return &Store{
		closeAuth:     ownedCancel,
		Secrets:       secrets,
		SolanaTransit: transit,
		Capabilities:  caps,
		SolanaCanSign: solanaCanSign,
		SecretWrite:   secretWrite && !opts.ReadOnly,
		VaultAuth:     vaultAuth,
	}, nil
}

// gateSecretBackend decides whether to serve the DECLARED backend from Vault KV,
// given the probed capabilities. It errors on the one unrecoverable case — secrets
// declared in Vault but the token can't read KV — and callers must NOT auto-fallback
// to the DB store (the data lives in Vault, not the DB, so DB would be empty).
func gateSecretBackend(backend string, vaultConnected bool, caps vault.Capabilities, kvMount string) (useVault bool, err error) {
	if backend == config.SecretBackendDB {
		return false, nil
	}
	if backend != config.SecretBackendVault {
		return false, fmt.Errorf("unknown credential backend")
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
	if err := enforceEncryptionPosture(enc.Enabled(), true); err != nil {
		return nil, err
	}
	store, err := merchants.NewEncryptedSecretStore(dbStore, enc)
	if err != nil {
		return nil, fmt.Errorf("wrap DB merchant secret store with encryption: %w", err)
	}
	return merchants.NewCachedSecretStore(store, merchants.DefaultSecretCacheTTL), nil
}

// BuildManifest combines immutable manifest provider credentials with the
// operator-owned alert-webhook namespace in the configured managed backend.
// Provider writes still receive the manifest-mode refusal; managed URL writes
// require encryption or Vault. A missing DB encryption key leaves the optional
// webhook feature disabled without requiring provider credentials in the DB.
func BuildManifest(ctx context.Context, cfg *config.Config, snapshot *merchants.ManifestSecretStore, pool *db.Pool) (*Store, error) {
	if cfg == nil {
		return nil, fmt.Errorf("credential config is required")
	}
	copy := *cfg
	copy.SecretBackend = "snapshot"
	return Build(ctx, &copy, pool, BuildOptions{Snapshot: snapshot})
}

// BuildTransit opens the Vault Transit signing client when Vault is enabled
// (a zero-value *Store, SolanaTransit nil, otherwise). MODE 1 uses it for
// Solana vault_transit signers with no KV backend. Login runs in the
// background; Transit answers vault.ErrNotAuthenticated until it succeeds.
func BuildTransit(ctx context.Context, cfg *config.Config) (*Store, error) {
	if cfg == nil || cfg.Vault == nil || !cfg.Vault.Enabled {
		return &Store{}, nil
	}
	authCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	client, sup, err := vault.Login(authCtx, vaultConfig(cfg.Vault))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("vault login: %w", err)
	}
	return &Store{
		closeAuth:     cancel,
		SolanaTransit: vault.NewTransitAdapter(client, resolveVaultTransitMount(cfg.Vault)).WithSupervisor(sup),
		VaultAuth:     sup,
	}, nil
}

func vaultConfig(vc *config.VaultConfig) vault.Config {
	return vault.Config{
		Address:    vc.Address,
		Namespace:  vc.Namespace,
		AuthMethod: vc.AuthMethod,
		Token:      vc.Token,
		RoleID:     vc.RoleID,
		SecretID:   vc.SecretID,
		K8sRole:    vc.K8sRole,
	}
}

// verifyCapabilities reports, once Vault authenticates, a token that cannot
// serve the declared KV store. Diagnostics only: reads fail at runtime either way.
func verifyCapabilities(ctx context.Context, client *vaultapi.Client, sup *vault.Supervisor, kvMount, scopePrefix string) {
	_ = retry.Forever(ctx, func(ctx context.Context) error {
		if err := sup.AuthState(); err != nil {
			return err
		}
		caps, err := vault.SelfCapabilities(ctx, client, kvMount, scopePrefix)
		if err != nil {
			return err
		}
		if !caps.KVRead {
			log.Errorf("vault: secret_backend=vault but the token cannot read the KV mount %q; merchant secrets are unavailable", kvMount)
		} else if !caps.KVWrite {
			log.Warn("vault: secret_backend=vault with read-only KV capability; merchant-secret writes fail")
		}
		return nil
	}, nil)
}

// enforceEncryptionPosture is the #667 boot gate on the DB-backed (fallback)
// secret store, mirroring db.EnforceRLSPosture: outside development a disabled
// encryptor refuses boot (secrets would persist plaintext at rest); development
// proceeds with one loud warning. Vault-backed deployments never reach this.
func enforceEncryptionPosture(encryptionEnabled, _ bool) error {
	if encryptionEnabled {
		return nil
	}
	return fmt.Errorf("merchant secrets: encrypted DB custody requires ENCRYPTION_MASTER_KEY; select secret_backend=snapshot or secret_backend=vault otherwise")
}
