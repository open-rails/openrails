package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/jackc/pgx/v5"
	authcore "github.com/open-rails/authkit/core"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/pkg/merchant"
)

const merchantManifestAdvisoryLock = int64(734252042137424)

const DefaultMerchantConfigManifestPath = "/etc/openrails/merchants.yaml"

type MerchantManifest struct {
	Version   int                `yaml:"version"`
	Merchants []ManifestMerchant `yaml:"merchants"`
}

// LoadMerchantConfigManifest reads and validates a merchant config manifest.
func LoadMerchantConfigManifest(path string) (*MerchantManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read merchant config manifest %s: %w", path, err)
	}
	return ParseMerchantConfigManifest(raw)
}

// ParseMerchantConfigManifest parses the merchant config manifest consumed by
// push-merchant-config. Bootstrap authority and catalog state are intentionally
// rejected by the strict YAML decoder.
func ParseMerchantConfigManifest(raw []byte) (*MerchantManifest, error) {
	var manifest MerchantManifest
	if err := yaml.UnmarshalWithOptions(raw, &manifest, yaml.DisallowUnknownField()); err != nil {
		return nil, fmt.Errorf("parse merchant config manifest: %w", err)
	}
	if len(manifest.Merchants) == 0 {
		return nil, fmt.Errorf("merchant config manifest must declare at least one merchant")
	}
	if err := validateMerchantManifestShape(&manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

type ManifestMerchant struct {
	Slug        string `yaml:"slug"`
	DisplayName string `yaml:"display_name"`
	// Issuer is the host application's JWKS/public-key trust for THIS merchant
	// (#527). When set, it is registered as a remote_application and made the
	// `owner` of the merchant's backing org, so the host app's delegated tokens
	// fully administer this merchant — and only this merchant. Optional: a
	// merchant with no issuer (e.g. embedded mode, where the host authenticates
	// in-process) is still provisioned with its backing org.
	Issuer           *ManifestIssuer           `yaml:"issuer,omitempty"`
	Profile          ManifestMerchantProfile   `yaml:"profile,omitempty"`
	ProviderAccounts []ManifestProviderAccount `yaml:"provider_accounts,omitempty"`
}

// ManifestIssuer declares the host-app issuer trusted for a merchant. Provide
// exactly one trust source: jwks_uri (preferred — AuthKit auto-refetches on
// key rotation) or public_keys (static PEMs, manual rotation).
type ManifestIssuer struct {
	// URI is the issuer's `iss` value (the host app's token issuer URL).
	URI string `yaml:"uri"`
	// JWKSURI is the issuer's JWKS endpoint. Mutually exclusive with PublicKeys.
	JWKSURI string `yaml:"jwks_uri,omitempty"`
	// PublicKeys are static verification keys (PEM). Mutually exclusive with JWKSURI.
	PublicKeys []authcore.RemoteAppKey `yaml:"public_keys,omitempty"`
	// Audiences the issuer's tokens must carry (defaults to ["openrails"]).
	Audiences []string `yaml:"audiences,omitempty"`
	// AllowedOrigins is the browser Origin allow-list for delegated requests.
	AllowedOrigins []string `yaml:"allowed_origins,omitempty"`
	// Slug overrides the remote_application slug (defaults to "<merchant>-app").
	Slug string `yaml:"slug,omitempty"`
}

type ManifestMerchantProfile struct {
	DisplayName string `yaml:"display_name,omitempty"`
	LogoURL     string `yaml:"logo_url,omitempty"`
	FromEmail   string `yaml:"from_email,omitempty"`
	SupportURL  string `yaml:"support_url,omitempty"`
}

type ManifestProviderAccount struct {
	ProviderType   string                          `yaml:"provider_type"`
	Environment    string                          `yaml:"environment,omitempty"`
	AccountID      string                          `yaml:"account_id,omitempty"`
	VaultSecretRef string                          `yaml:"vault_secret_ref,omitempty"`
	Mode           string                          `yaml:"mode,omitempty"`
	Secrets        map[string]ManifestSecretSource `yaml:"secrets,omitempty"`
}

type ManifestSecretSource struct {
	Value string `yaml:"value,omitempty"`
	Env   string `yaml:"env,omitempty"`
	File  string `yaml:"file,omitempty"`
	Vault string `yaml:"vault,omitempty"`
}

// MerchantManifestReconcileOptions selects the apply tier (#527). The default
// (both false) is additive + seed-once. Startup provisioning always uses the
// default; the destructive tiers are opt-in via the CLI and never run on boot.
type MerchantManifestReconcileOptions struct {
	// Insert creates missing merchant/org/issuer/profile/provider-account/secret
	// state declared by the manifest. Manual CLI runs default to plan-only until
	// this or another mutation flag is set.
	Insert bool
	// Overwrite re-asserts manifest values over existing state. Without it,
	// SECRETS are seed-once: a secret already present is left untouched, so a
	// value rotated out of band (via the admin API) is never reverted to the
	// manifest seed. Merchant/org/issuer/profile are idempotently ensured either
	// way (they are declarative identity, not rotated out of band).
	Overwrite bool
	// Prune deletes secrets that exist for a manifest merchant but are absent
	// from the manifest, reconciling the secret set to the file. Provider-account
	// and issuer removal stay reversible/manual and are not pruned here.
	Prune bool
	// IdentityResolver is an optional test/embedding seam for provider account
	// discovery. Production uses the default resolver over provider read-only
	// identity APIs.
	IdentityResolver ManifestProviderIdentityResolver
}

type ManifestProviderIdentityResolver interface {
	ResolveManifestProviderAccount(ctx context.Context, cfg *config.Config, providerType, environment string, account ManifestProviderAccount, secrets manifestSecretValues) (manifestProviderIdentity, error)
}

type manifestProviderIdentity struct {
	AccountID   string
	DisplayName *string
	Evidence    map[string]any
}

func (o MerchantManifestReconcileOptions) HasMutations() bool {
	return o.Insert || o.Overwrite || o.Prune
}

// ProvisionMerchant is the single OpenRails merchant-provisioning boundary
// (#527). Standalone calls it with a control plane, which creates/ensures the
// backing AuthKit org and optional issuer-as-owner before recording
// permission_group_id. Embedded calls it with only Database, which registers an
// ownerless merchant row and applies the same profile/provider-account
// configuration path without touching AuthKit or startup bootstrap markers.
type ProvisionMerchantRequest struct {
	Config       *config.Config
	ControlPlane *controlplane.ControlPlane
	Database     *db.DB
	SecretStore  merchants.MerchantSecretStore
	Merchant     ManifestMerchant
	Options      MerchantManifestReconcileOptions
}

// ReconcileMerchantManifestData provisions merchants and issuer ownership
// declared by a merchant config manifest. It is the single
// merchant-provisioning entry point for push-merchant-config and embedded
// startup paths. Issuer registration is declarative and does not fetch the
// JWKS, so it succeeds even when the issuer's app is not yet running.
func ReconcileMerchantManifestData(ctx context.Context, cfg *config.Config, cp *controlplane.ControlPlane, manifest *MerchantManifest, opts MerchantManifestReconcileOptions) error {
	if cp == nil || cp.Core() == nil || cp.Pool() == nil {
		return fmt.Errorf("merchant bootstrap manifest configured but control plane is not enabled")
	}
	if manifest == nil {
		return fmt.Errorf("merchant bootstrap manifest is required")
	}
	if manifest.Version != BootstrapManifestVersion {
		return fmt.Errorf("merchant bootstrap: manifest version must be %d", BootstrapManifestVersion)
	}
	if err := lockMerchantManifestBootstrap(ctx, cp); err != nil {
		return err
	}
	defer unlockMerchantManifestBootstrap(context.Background(), cp)

	if len(manifest.Merchants) == 0 {
		log.Info("merchant bootstrap manifest has no merchants")
		return nil
	}

	secretBackend, err := merchantsecrets.Build(ctx, cfg, cp.Pool())
	if err != nil {
		return fmt.Errorf("merchant bootstrap: build secret store: %w", err)
	}
	secretStore := secretBackend.Secrets
	database, err := db.NewWithPGXPool(cp.Pool().Raw(), cp.Pool().Schema())
	if err != nil {
		return fmt.Errorf("wrap control-plane db: %w", err)
	}

	for _, mt := range manifest.Merchants {
		tn, err := ProvisionMerchant(ctx, ProvisionMerchantRequest{
			Config:       cfg,
			ControlPlane: cp,
			Database:     database,
			SecretStore:  secretStore,
			Merchant:     mt,
			Options:      opts,
		})
		if err != nil {
			return err
		}
		log.WithFields(log.Fields{
			"merchant":    tn.Slug,
			"merchant_id": tn.ID.String(),
		}).Info("merchant bootstrap: merchant ensured")
	}

	// #480/#481: issuer/JWKS trust is AuthKit's remote_application registry (#74),
	// not an OpenRails-owned table — the manifest no longer reconciles issuers.
	return nil
}

func ProvisionMerchant(ctx context.Context, req ProvisionMerchantRequest) (*merchants.Merchant, error) {
	mt := req.Merchant
	database := req.Database
	if database == nil {
		if req.ControlPlane == nil || req.ControlPlane.Pool() == nil {
			return nil, fmt.Errorf("merchant provisioning requires database or control plane")
		}
		var err error
		database, err = db.NewWithPGXPool(req.ControlPlane.Pool().Raw(), req.ControlPlane.Pool().Schema())
		if err != nil {
			return nil, fmt.Errorf("wrap control-plane db: %w", err)
		}
	}

	tn, found, err := lookupManifestMerchant(ctx, database, mt.Slug)
	if err != nil {
		return nil, fmt.Errorf("merchant bootstrap: lookup %q: %w", mt.Slug, err)
	}
	if !found {
		if !req.Options.Insert {
			return nil, fmt.Errorf("merchant bootstrap: merchant %q is missing; rerun with --insert to create it", mt.Slug)
		}
		tn, err = provisionMerchantIdentity(ctx, database, req.ControlPlane, mt)
		if err != nil {
			return nil, err
		}
	} else if req.ControlPlane != nil && mt.Issuer != nil && req.Options.Overwrite {
		if _, err := provisionMerchantGroup(ctx, req.ControlPlane, mt); err != nil {
			return nil, fmt.Errorf("merchant bootstrap: update merchant group/issuer for %q: %w", mt.Slug, err)
		}
	}

	if err := reconcileManifestMerchantConfiguration(ctx, req.Config, database, tn.ID, mt, req.SecretStore, req.Options); err != nil {
		return nil, fmt.Errorf("merchant bootstrap: configure %q: %w", mt.Slug, err)
	}
	return tn, nil
}

func provisionMerchantIdentity(ctx context.Context, database *db.DB, cp *controlplane.ControlPlane, mt ManifestMerchant) (*merchants.Merchant, error) {
	if cp == nil {
		// Embedded: OpenRails runs no AuthKit, so it creates/records no backing org.
		// The merchant's backing org is the host's AuthKit org of the SAME slug
		// (#541 — merchant slug == group slug); permission_group_id stays NULL here and
		// is set only in standalone, where OpenRails owns the org.
		id, err := db.RegisterMerchant(ctx, database.Qx(ctx), db.RegisterMerchantOptions{Slug: mt.Slug})
		if err != nil {
			return nil, err
		}
		tn, found, err := lookupManifestMerchant(ctx, database, mt.Slug)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("merchant bootstrap: registered merchant %q but could not read it back", mt.Slug)
		}
		tn.ID = id
		return tn, nil
	}

	// #567: the merchant IS a top-level permission-group (child of root, no parent
	// org). When the merchant declares an issuer, AuthKit registers it as a
	// remote_application nested under the merchant group with the `owner` role so
	// host-app delegated tokens administer this merchant only.
	groupID, err := provisionMerchantGroup(ctx, cp, mt)
	if err != nil {
		return nil, fmt.Errorf("merchant bootstrap: provision merchant group/issuer for %q: %w", mt.Slug, err)
	}
	svc, err := merchants.NewService(cp.Pool(), nil)
	if err != nil {
		return nil, err
	}
	tn, err := svc.Provision(ctx, merchants.ProvisionRequest{
		Slug:              mt.Slug,
		PermissionGroupID: groupID,
	})
	if err != nil {
		return nil, fmt.Errorf("merchant bootstrap: provision %q: %w", mt.Slug, err)
	}
	return tn, nil
}

func lookupManifestMerchant(ctx context.Context, database *db.DB, slug string) (*merchants.Merchant, bool, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return nil, false, fmt.Errorf("merchant slug is required")
	}
	var (
		id                string
		status            string
		permissionGroupID *string
	)
	err := database.Qx(ctx).QueryRow(ctx, `
		SELECT id::text, status, permission_group_id
		  FROM openrails.merchants
		 WHERE slug = $1
	`, slug).Scan(&id, &status, &permissionGroupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	merchantID, err := merchant.ParseID(id)
	if err != nil {
		return nil, false, err
	}
	owner := ""
	if permissionGroupID != nil {
		owner = *permissionGroupID
	}
	return &merchants.Merchant{
		ID:                merchantID,
		Slug:              slug,
		Status:            merchants.MerchantStatus(status),
		PermissionGroupID: owner,
	}, true, nil
}

func reconcileManifestMerchantConfiguration(ctx context.Context, cfg *config.Config, database *db.DB, merchantID merchant.ID, mt ManifestMerchant, secretStore merchants.MerchantSecretStore, opts MerchantManifestReconcileOptions) error {
	mctx := merchant.WithID(ctx, merchantID)
	if hasManifestProfile(mt.Profile) {
		store := merchantconfig.NewStore(database)
		cfg, _, err := store.Get(mctx)
		if err != nil {
			return fmt.Errorf("load merchant configuration: %w", err)
		}
		cfg.Profile = models.MerchantProfileConfiguration{
			DisplayName: strings.TrimSpace(mt.Profile.DisplayName),
			LogoURL:     strings.TrimSpace(mt.Profile.LogoURL),
			FromEmail:   strings.TrimSpace(mt.Profile.FromEmail),
			SupportURL:  strings.TrimSpace(mt.Profile.SupportURL),
		}
		if cfg.Profile.DisplayName == "" {
			cfg.Profile.DisplayName = strings.TrimSpace(mt.DisplayName)
		}
		if err := store.Upsert(mctx, cfg); err != nil {
			return fmt.Errorf("upsert merchant profile: %w", err)
		}
	}

	for _, account := range mt.ProviderAccounts {
		if secretStore == nil {
			return fmt.Errorf("merchant bootstrap: provider account secrets require a secret store")
		}
		if err := reconcileManifestProviderAccount(ctx, cfg, database, merchantID, account, secretStore, opts); err != nil {
			return err
		}
	}
	if opts.Prune {
		if secretStore == nil {
			return fmt.Errorf("merchant bootstrap: prune requires a secret store")
		}
		if err := pruneManifestSecrets(ctx, merchantID, mt, secretStore); err != nil {
			return err
		}
	}
	return nil
}

// pruneManifestSecrets deletes secrets held for the merchant that the manifest
// no longer declares (#527 --prune), reconciling the stored secret set to the
// file. Names are derived exactly as Put derives them.
func pruneManifestSecrets(ctx context.Context, merchantID merchant.ID, mt ManifestMerchant, secretStore merchants.MerchantSecretStore) error {
	declared := map[string]struct{}{}
	for _, account := range mt.ProviderAccounts {
		providerType := normalizeManifestProviderType(account.ProviderType)
		environment, err := normalizeProviderEnvironment(account.Environment)
		if err != nil {
			return err
		}
		accountID := strings.TrimSpace(account.AccountID)
		if accountID == "" {
			return fmt.Errorf("provider account %q account_id is required before pruning secrets", providerType)
		}
		for key := range account.Secrets {
			name, err := merchants.ProviderAccountSecretName(providerType, environment, accountID, key)
			if err != nil {
				return err
			}
			declared[name] = struct{}{}
		}
	}
	existing, err := secretStore.List(ctx, merchantID)
	if err != nil {
		return fmt.Errorf("list merchant secrets for prune: %w", err)
	}
	for _, name := range existing {
		if _, ok := declared[name]; ok {
			continue
		}
		if err := secretStore.Delete(ctx, merchantID, name); err != nil {
			return fmt.Errorf("prune secret %s: %w", name, err)
		}
		log.WithField("secret", name).Info("merchant bootstrap: pruned secret absent from manifest")
	}
	return nil
}

func hasManifestProfile(p ManifestMerchantProfile) bool {
	return strings.TrimSpace(p.DisplayName) != "" ||
		strings.TrimSpace(p.LogoURL) != "" ||
		strings.TrimSpace(p.FromEmail) != "" ||
		strings.TrimSpace(p.SupportURL) != ""
}

func reconcileManifestProviderAccount(ctx context.Context, cfg *config.Config, database *db.DB, merchantID merchant.ID, account ManifestProviderAccount, secretStore merchants.MerchantSecretStore, opts MerchantManifestReconcileOptions) error {
	providerType := normalizeManifestProviderType(account.ProviderType)
	if providerType == "" {
		return fmt.Errorf("provider account provider_type is required")
	}
	environment, err := normalizeProviderEnvironment(account.Environment)
	if err != nil {
		return err
	}
	secrets, err := newManifestSecretValues(providerType, account.Secrets)
	if err != nil {
		return err
	}
	resolver := opts.IdentityResolver
	if resolver == nil {
		resolver = defaultManifestProviderIdentityResolver{}
	}
	identity, err := resolver.ResolveManifestProviderAccount(ctx, cfg, providerType, environment, account, secrets)
	if err != nil {
		return err
	}
	accountID := strings.TrimSpace(identity.AccountID)
	if accountID == "" {
		return fmt.Errorf("provider account %q identity resolution returned an empty account id", providerType)
	}
	for key, source := range account.Secrets {
		name, err := merchants.ProviderAccountSecretName(providerType, environment, accountID, key)
		if err != nil {
			return err
		}
		_, gerr := secretStore.Get(ctx, merchantID, name)
		switch {
		case gerr == nil && !opts.Overwrite:
			// Seed-once (#527): unless --overwrite, leave an already-present secret
			// untouched so a value rotated out of band is never reverted to the seed.
			continue
		case errors.Is(gerr, merchants.ErrSecretNotFound) && !opts.Insert:
			return fmt.Errorf("secret %s is missing; rerun with --insert to create it", name)
		case gerr != nil && !errors.Is(gerr, merchants.ErrSecretNotFound):
			return fmt.Errorf("check secret %s: %w", name, gerr)
		}
		value, err := secrets.Resolve(key, source)
		if err != nil {
			return fmt.Errorf("resolve secret %s.%s: %w", providerType, key, err)
		}
		if _, err := secretStore.Put(ctx, merchantID, name, value); err != nil {
			return fmt.Errorf("store secret %s: %w", name, err)
		}
	}

	var existing gen.OpenrailsProviderAccount
	found := false
	if err := database.RunInMerchantConn(merchant.WithID(ctx, merchantID), func(ctx context.Context) error {
		var err error
		existing, err = database.Gen(ctx).GetProviderAccountByIdentity(ctx, gen.GetProviderAccountByIdentityParams{
			MerchantID:   merchantID.UUID(),
			ProviderType: providerType,
			Environment:  stringPtrIfNotEmpty(environment),
			AccountID:    accountID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return nil
	}); err != nil {
		return fmt.Errorf("lookup provider account %s:%s:%s: %w", providerType, environment, accountID, err)
	}
	if !found && !opts.Insert {
		return fmt.Errorf("provider account %s:%s:%s is missing; rerun with --insert to create it", providerType, environment, accountID)
	}
	if found && !opts.Overwrite {
		return nil
	}
	evidence := identity.Evidence
	if evidence == nil {
		evidence = map[string]any{"source": "merchant_config_manifest"}
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("encode provider account evidence: %w", err)
	}
	vaultSecretRef := stringPtrIfNotEmpty(account.VaultSecretRef)
	role, status, err := manifestProviderAccountMode(account.Mode)
	if err != nil {
		return err
	}
	if cfg != nil && !cfg.IsDev() && environment == "test" && role != nil && *role == configProcessorRolePrimary && (status == nil || *status == "enabled") {
		return fmt.Errorf("provider account %q cannot be mode=primary with environment=test outside development", providerType)
	}
	mctx := merchant.WithID(ctx, merchantID)
	row := existing
	if err := database.RunInMerchantConn(mctx, func(ctx context.Context) error {
		var err error
		row, err = database.Gen(ctx).UpsertProviderAccount(ctx, gen.UpsertProviderAccountParams{
			MerchantID:     merchantID.UUID(),
			ProviderType:   providerType,
			Environment:    stringPtrIfNotEmpty(environment),
			AccountID:      accountID,
			DisplayName:    identity.DisplayName,
			VaultSecretRef: vaultSecretRef,
			Role:           role,
			Status:         status,
			Evidence:       evidenceJSON,
		})
		if err != nil {
			return fmt.Errorf("upsert provider account %s:%s: %w", providerType, accountID, err)
		}
		if role != nil && *role == config.ProcessorRolePrimary && (status == nil || *status == "enabled") {
			if err := database.Gen(ctx).DemoteOtherPrimaryProviderAccounts(ctx, gen.DemoteOtherPrimaryProviderAccountsParams{
				MerchantID:   merchantID.UUID(),
				ProviderType: providerType,
				Environment:  stringPtrIfNotEmpty(environment),
				ID:           row.ID,
			}); err != nil {
				return fmt.Errorf("demote old primary provider accounts: %w", err)
			}
			if _, err := database.Gen(ctx).PromoteProviderAccountToPrimary(ctx, gen.PromoteProviderAccountToPrimaryParams{
				ID:           row.ID,
				MerchantID:   merchantID.UUID(),
				ProviderType: providerType,
				Environment:  stringPtrIfNotEmpty(environment),
			}); err != nil {
				return fmt.Errorf("promote provider account primary: %w", err)
			}
			return nil
		}
		if role != nil || status != nil {
			if _, err := database.Qx(ctx).Exec(ctx, `
				UPDATE openrails.provider_accounts
				   SET role = COALESCE($1, role),
				       status = COALESCE($2, status),
				       replaced_at = CASE
				           WHEN COALESCE($1, role) = 'legacy' THEN COALESCE(replaced_at, now())
				           WHEN COALESCE($1, role) = 'primary' THEN NULL
				           ELSE replaced_at
				       END,
				       updated_at = now()
				 WHERE id = $3
				   AND merchant_id = $4::uuid
				   AND provider_type = $5
			`, role, status, row.ID, merchantID.UUID(), providerType); err != nil {
				return fmt.Errorf("update provider account role/status: %w", err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func normalizeProviderEnvironment(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "live", "prod", "production", "mainnet":
		return "live", nil
	case "test", "sandbox", "devnet", "testnet":
		return "test", nil
	default:
		return "", fmt.Errorf("provider account environment must be live or test")
	}
}

func normalizeManifestProviderType(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "mobius":
		return config.ProcessorTypeNMI
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

func manifestProviderAccountMode(raw string) (*string, *string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return nil, nil, nil
	case configProcessorRolePrimary:
		return stringPtrIfNotEmpty(configProcessorRolePrimary), stringPtrIfNotEmpty("enabled"), nil
	case configProcessorRoleSecondary:
		return stringPtrIfNotEmpty(configProcessorRoleSecondary), stringPtrIfNotEmpty("enabled"), nil
	case configProcessorRoleLegacy:
		return stringPtrIfNotEmpty(configProcessorRoleLegacy), stringPtrIfNotEmpty("enabled"), nil
	case "disabled":
		return stringPtrIfNotEmpty(configProcessorRoleSecondary), stringPtrIfNotEmpty("disabled"), nil
	default:
		return nil, nil, fmt.Errorf("provider account mode must be primary, secondary, legacy, or disabled")
	}
}

type manifestSecretValues struct {
	providerType string
	sources      map[string]ManifestSecretSource
	values       map[string]string
}

func newManifestSecretValues(providerType string, sources map[string]ManifestSecretSource) (manifestSecretValues, error) {
	out := manifestSecretValues{
		providerType: providerType,
		sources:      map[string]ManifestSecretSource{},
		values:       map[string]string{},
	}
	for key, source := range sources {
		canonical, err := merchants.NormalizeProviderAccountSecretKey(providerType, key)
		if err != nil {
			return out, err
		}
		if _, exists := out.sources[canonical]; exists {
			return out, fmt.Errorf("duplicate provider account secret key %q", canonical)
		}
		out.sources[canonical] = source
	}
	return out, nil
}

func (v manifestSecretValues) Resolve(key string, fallback ManifestSecretSource) (string, error) {
	canonical, err := merchants.NormalizeProviderAccountSecretKey(v.providerType, key)
	if err != nil {
		return "", err
	}
	if value, ok := v.values[canonical]; ok {
		return value, nil
	}
	source, ok := v.sources[canonical]
	if !ok {
		source = fallback
	}
	value, err := source.Resolve()
	if err != nil {
		return "", err
	}
	v.values[canonical] = value
	return value, nil
}

func (v manifestSecretValues) ResolveIfPresent(key string) (string, bool, error) {
	canonical, err := merchants.NormalizeProviderAccountSecretKey(v.providerType, key)
	if err != nil {
		return "", false, err
	}
	source, ok := v.sources[canonical]
	if !ok {
		return "", false, nil
	}
	value, err := v.Resolve(canonical, source)
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

type defaultManifestProviderIdentityResolver struct{}

func (defaultManifestProviderIdentityResolver) ResolveManifestProviderAccount(ctx context.Context, cfg *config.Config, providerType, environment string, account ManifestProviderAccount, secrets manifestSecretValues) (manifestProviderIdentity, error) {
	if accountID := strings.TrimSpace(account.AccountID); accountID != "" {
		return manifestProviderIdentity{
			AccountID: accountID,
			Evidence:  map[string]any{"source": "merchant_config_manifest.account_id"},
		}, nil
	}
	switch providerType {
	case config.ProcessorTypeStripe:
		key, ok, err := secrets.ResolveIfPresent("secret_key")
		if err != nil {
			return manifestProviderIdentity{}, fmt.Errorf("resolve stripe secret_key for account discovery: %w", err)
		}
		if !ok {
			return manifestProviderIdentity{}, fmt.Errorf("provider account stripe account_id is required unless secrets.secret_key is present for account discovery")
		}
		accountID, err := discoverStripeAccountID(ctx, cfg, key)
		if err != nil {
			return manifestProviderIdentity{}, fmt.Errorf("discover stripe account identity: %w", err)
		}
		return manifestProviderIdentity{
			AccountID: accountID,
			Evidence:  map[string]any{"source": "stripe.get_account"},
		}, nil
	case config.ProcessorTypeNMI:
		key, ok, err := secrets.ResolveIfPresent("production_key")
		if err != nil {
			return manifestProviderIdentity{}, fmt.Errorf("resolve nmi production_key for account discovery: %w", err)
		}
		if !ok {
			return manifestProviderIdentity{}, fmt.Errorf("provider account nmi account_id is required unless secrets.production_key/security_key is present for profile discovery")
		}
		client, err := nmi.NewClient("bootstrap-"+providerType, &config.NMIProviderSettings{SecurityKey: key}, cfg != nil && cfg.IsTestEnv())
		if err != nil {
			return manifestProviderIdentity{}, fmt.Errorf("build nmi identity client: %w", err)
		}
		identity, err := client.AccountIdentity()
		if err != nil {
			return manifestProviderIdentity{}, fmt.Errorf("discover nmi account identity: %w", err)
		}
		accountID := strings.TrimPrefix(identity, "nmi:")
		return manifestProviderIdentity{
			AccountID:   accountID,
			DisplayName: stringPtrIfNotEmpty(accountID),
			Evidence:    map[string]any{"source": "nmi.profile"},
		}, nil
	case config.ProcessorTypeCCBill:
		raw, ok, err := secrets.ResolveIfPresent("account_config")
		if err != nil {
			return manifestProviderIdentity{}, fmt.Errorf("resolve ccbill account_config for account discovery: %w", err)
		}
		if !ok {
			return manifestProviderIdentity{}, fmt.Errorf("provider account ccbill account_id is required unless secrets.account_config is present")
		}
		accountID, err := ccbillAccountIDFromConfig(raw)
		if err != nil {
			return manifestProviderIdentity{}, err
		}
		return manifestProviderIdentity{
			AccountID: accountID,
			Evidence:  map[string]any{"source": "ccbill.account_config"},
		}, nil
	case config.ProcessorTypeSolana:
		return manifestProviderIdentity{}, fmt.Errorf("provider account solana account_id is required; declare the public wallet/authority explicitly")
	default:
		return manifestProviderIdentity{}, fmt.Errorf("provider account %q account_id is required", providerType)
	}
}

func discoverStripeAccountID(ctx context.Context, cfg *config.Config, secretKey string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.stripe.com/v1/account", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(secretKey))
	resp, err := stripeapi.Client(cfg, 0).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("stripe /v1/account returned %d", resp.StatusCode)
	}
	var payload struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if strings.TrimSpace(payload.ID) == "" {
		return "", fmt.Errorf("stripe /v1/account response missing id")
	}
	return strings.TrimSpace(payload.ID), nil
}

func ccbillAccountIDFromConfig(raw string) (string, error) {
	var cfg struct {
		ClientAccNum string `json:"client_acc_num"`
		ClientSubAcc string `json:"client_sub_acc"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return "", fmt.Errorf("parse ccbill account_config: %w", err)
	}
	accountID := strings.TrimSpace(cfg.ClientAccNum)
	if sub := strings.TrimSpace(cfg.ClientSubAcc); accountID != "" && sub != "" {
		accountID += "/" + sub
	}
	if accountID == "" {
		return "", fmt.Errorf("ccbill account_config must include client_acc_num")
	}
	return accountID, nil
}

func (s ManifestSecretSource) RefCount() int {
	refs := 0
	if strings.TrimSpace(s.Value) != "" {
		refs++
	}
	if strings.TrimSpace(s.Env) != "" {
		refs++
	}
	if strings.TrimSpace(s.File) != "" {
		refs++
	}
	if strings.TrimSpace(s.Vault) != "" {
		refs++
	}
	return refs
}

func (s ManifestSecretSource) Resolve() (string, error) {
	if s.RefCount() != 1 {
		return "", fmt.Errorf("secret source must set exactly one of value, env, file, vault")
	}
	if v := strings.TrimSpace(s.Value); v != "" {
		return v, nil
	}
	if name := strings.TrimSpace(s.Env); name != "" {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			return "", fmt.Errorf("env %s is empty", name)
		}
		return v, nil
	}
	if path := strings.TrimSpace(s.File); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read file %s: %w", path, err)
		}
		v := strings.TrimSpace(string(raw))
		if v == "" {
			return "", fmt.Errorf("file %s is empty", path)
		}
		return v, nil
	}
	return "", fmt.Errorf("vault secret-source references are not readable by push-merchant-config yet; use env or file for this release")
}

func stringPtrIfNotEmpty(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return &v
}

// provisionMerchantGroup ensures the merchant's top-level permission-group exists
// (`type=merchant`, `resourceRef=slug`, child of `root`; NO parent org — #567) and,
// when the merchant declares an issuer, registers that issuer as a
// remote_application nested under the merchant group and grants it the merchant
// `owner` role (full `merchant:*` authority, scoped to this merchant alone since
// federated authority claims are stripped). Idempotent: re-applying converges the
// group + issuer state. Returns the merchant group's internal id.
func provisionMerchantGroup(ctx context.Context, cp *controlplane.ControlPlane, mt ManifestMerchant) (string, error) {
	slug := merchant.NormalizeSlug(mt.Slug)
	if slug == "" {
		return "", fmt.Errorf("merchant slug is required")
	}
	// #548: validate the merchant slug as a legal slug up front for a clear error.
	if err := merchant.ValidateSlug(slug); err != nil {
		return "", err
	}
	core := cp.Core()
	if core == nil {
		return "", fmt.Errorf("merchant bootstrap: control plane core unavailable")
	}

	// Ensure the root group + containment exist before creating typed groups.
	if _, err := core.EnsureRootGroup(ctx); err != nil {
		return "", fmt.Errorf("merchant bootstrap: ensure root group: %w", err)
	}
	if err := core.SeedPermissionGroupContainment(ctx); err != nil {
		return "", fmt.Errorf("merchant bootstrap: seed containment: %w", err)
	}

	// Idempotently create the merchant permission-group (resolve, else create).
	groupID, err := core.ResolveGroupIDForSlug(ctx, controlplane.MerchantType, slug)
	if errors.Is(err, authcore.ErrGroupNotFound) {
		groupID, err = core.CreatePermissionGroup(ctx, authcore.CreatePermissionGroupRequest{
			Persona:       controlplane.MerchantType,
			InstanceSlug:  slug,
			ParentPersona: authcore.RootPersona,
		})
		if err != nil {
			return "", fmt.Errorf("merchant bootstrap: create merchant group %q: %w", slug, err)
		}
	} else if err != nil {
		return "", fmt.Errorf("merchant bootstrap: resolve merchant group %q: %w", slug, err)
	}

	// Register the merchant's federated issuer as a remote_application nested
	// under the merchant group, then grant it the merchant `owner` role.
	if mt.Issuer != nil {
		ra := manifestIssuerToRemoteApplication(slug, groupID, mt.Issuer)
		stored, err := core.UpsertRemoteApplication(ctx, ra)
		if err != nil {
			return "", fmt.Errorf("merchant bootstrap: register issuer for %q: %w", slug, err)
		}
		if err := core.AssignGroupRole(ctx, controlplane.MerchantType, slug, stored.ID, authcore.SubjectKindRemoteApp, controlplane.MerchantRoleOwner); err != nil {
			return "", fmt.Errorf("merchant bootstrap: grant issuer owner role for %q: %w", slug, err)
		}
	}

	return groupID, nil
}

// manifestIssuerToRemoteApplication maps a merchant's manifest issuer onto an
// AuthKit remote_application registration nested under the merchant group
// (groupID carried in the OrgID field, retained authbase name — #567).
func manifestIssuerToRemoteApplication(merchantSlug, groupID string, iss *ManifestIssuer) authcore.RemoteApplication {
	appSlug := strings.TrimSpace(iss.Slug)
	if appSlug == "" {
		appSlug = merchantSlug + "-app"
	}
	mode := authcore.RemoteAppModeJWKS
	if len(iss.PublicKeys) > 0 {
		mode = authcore.RemoteAppModeStatic
	}
	audiences := cleanStrings(iss.Audiences)
	if len(audiences) == 0 {
		audiences = []string{"openrails"}
	}
	return authcore.RemoteApplication{
		Slug:              appSlug,
		PermissionGroupID: groupID,
		Issuer:            strings.TrimSpace(iss.URI),
		JWKSURI:           strings.TrimSpace(iss.JWKSURI),
		Mode:              mode,
		PublicKeys:        iss.PublicKeys,
		Audiences:         audiences,
		AllowedOrigins:    cleanStrings(iss.AllowedOrigins),
		Enabled:           true,
	}
}

func lockMerchantManifestBootstrap(ctx context.Context, cp *controlplane.ControlPlane) error {
	_, err := cp.Pool().Exec(ctx, `SELECT pg_advisory_lock($1)`, merchantManifestAdvisoryLock)
	if err != nil {
		return fmt.Errorf("merchant bootstrap: acquire advisory lock: %w", err)
	}
	return nil
}

func unlockMerchantManifestBootstrap(ctx context.Context, cp *controlplane.ControlPlane) {
	if _, err := cp.Pool().Exec(ctx, `SELECT pg_advisory_unlock($1)`, merchantManifestAdvisoryLock); err != nil {
		log.WithError(err).Warn("merchant bootstrap: release advisory lock failed")
	}
}

// AnyMerchantProvisioned reports whether any of the given org slugs is already
// provisioned in the control plane. The server uses this for first-run
// detection: it auto-applies the bootstrap manifest only when NONE of the
// manifest's merchants exist yet (#327). Checking the manifest's own slugs — not a
// blanket merchant count — is required because the control plane's own bootstrap
// always creates a "default" merchant, so the table is never empty.
func AnyMerchantProvisioned(ctx context.Context, cp *controlplane.ControlPlane, slugs []string) (bool, error) {
	if cp == nil || cp.Pool() == nil {
		return false, nil
	}
	norm := make([]string, 0, len(slugs))
	for _, s := range slugs {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			norm = append(norm, s)
		}
	}
	if len(norm) == 0 {
		return false, nil
	}
	var exists bool
	err := cp.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM openrails.merchants WHERE slug = ANY($1) AND deleted_at IS NULL)`, norm).Scan(&exists)
	return exists, err
}

func cleanStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
