package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	authcore "github.com/open-rails/authkit/core"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/secretstore"
	"github.com/open-rails/openrails/pkg/merchant"
)

const merchantManifestAdvisoryLock = int64(734252042137424)

type MerchantManifest struct {
	Version   int                `yaml:"version"`
	Merchants []ManifestMerchant `yaml:"merchants"`
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
	AccountID      string                          `yaml:"account_id,omitempty"`
	ProviderKey    string                          `yaml:"provider_key,omitempty"`
	DisplayName    string                          `yaml:"display_name,omitempty"`
	VaultSecretRef string                          `yaml:"vault_secret_ref,omitempty"`
	Role           string                          `yaml:"role,omitempty"`
	Status         string                          `yaml:"status,omitempty"`
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
}

// ReconcileMerchantManifestData provisions the merchants, service-JWT grants, and
// issuers declared by a merchant manifest. It is the single merchant-provisioning
// entry point, consumed by the unified BootstrapManifest apply path (CLI +
// server first-run startup). Issuer registration is declarative and does not
// fetch the JWKS, so it succeeds even when the issuer's app is not yet running.
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

	svc, err := merchants.NewService(cp.Pool(), nil)
	if err != nil {
		return err
	}
	// OR-CFG-001: build the secret store via the shared constructor — the SAME
	// wiring the runtime read path uses — so provisioning never writes provider /
	// processor / Solana secrets less protected (e.g. plaintext) than the runtime
	// expects. Vault-backed when configured, otherwise per-merchant envelope
	// encryption with a plaintext-Solana write-block.
	secretStore, _, err := secretstore.Build(ctx, cfg, cp.Pool())
	if err != nil {
		return fmt.Errorf("merchant bootstrap: build secret store: %w", err)
	}

	for _, mt := range manifest.Merchants {
		// #527: each merchant has a dedicated backing org (slug-derived, 1:1). When
		// the merchant declares an issuer, it is registered as the org OWNER so the
		// host app's delegated tokens fully administer this one merchant.
		org, err := provisionMerchantOrg(ctx, cp, mt)
		if err != nil {
			return fmt.Errorf("merchant bootstrap: provision backing org/issuer for %q: %w", mt.Slug, err)
		}
		tn, err := svc.Provision(ctx, merchants.ProvisionRequest{
			Slug:       mt.Slug,
			OwnerOrgID: org.ID,
		})
		if err != nil {
			return fmt.Errorf("merchant bootstrap: provision %q: %w", mt.Slug, err)
		}
		log.WithFields(log.Fields{
			"merchant":    tn.Slug,
			"merchant_id": tn.ID.String(),
		}).Info("merchant bootstrap: merchant ensured")
		if err := reconcileManifestMerchantConfiguration(ctx, cfg, cp, tn.ID, mt, secretStore, opts); err != nil {
			return fmt.Errorf("merchant bootstrap: configure %q: %w", mt.Slug, err)
		}
	}

	// #480/#481: issuer/JWKS trust is AuthKit's remote_application registry (#74),
	// not an OpenRails-owned table — the manifest no longer reconciles issuers.
	return nil
}

func reconcileManifestMerchantConfiguration(ctx context.Context, _ *config.Config, cp *controlplane.ControlPlane, merchantID merchant.ID, mt ManifestMerchant, secretStore merchants.MerchantSecretStore, opts MerchantManifestReconcileOptions) error {
	mctx := merchant.WithID(ctx, merchantID)
	database, err := db.NewWithPGXPool(cp.Pool().Raw(), cp.Pool().Schema())
	if err != nil {
		return fmt.Errorf("wrap control-plane db: %w", err)
	}
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
		if err := reconcileManifestProviderAccount(ctx, database, merchantID, account, secretStore, opts); err != nil {
			return err
		}
	}
	if opts.Prune {
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
		providerType := strings.ToLower(strings.TrimSpace(account.ProviderType))
		for key := range account.Secrets {
			name, err := merchantSecretName(providerType, key)
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

func reconcileManifestProviderAccount(ctx context.Context, database *db.DB, merchantID merchant.ID, account ManifestProviderAccount, secretStore merchants.MerchantSecretStore, opts MerchantManifestReconcileOptions) error {
	providerType := strings.ToLower(strings.TrimSpace(account.ProviderType))
	if providerType == "" {
		return fmt.Errorf("provider account provider_type is required")
	}
	for key, source := range account.Secrets {
		name, err := merchantSecretName(providerType, key)
		if err != nil {
			return err
		}
		// Seed-once (#527): unless --overwrite, leave an already-present secret
		// untouched so a value rotated out of band is never reverted to the seed.
		if !opts.Overwrite {
			if _, gerr := secretStore.Get(ctx, merchantID, name); gerr == nil {
				continue
			} else if !errors.Is(gerr, merchants.ErrSecretNotFound) {
				return fmt.Errorf("check secret %s: %w", name, gerr)
			}
		}
		value, err := source.Resolve()
		if err != nil {
			return fmt.Errorf("resolve secret %s.%s: %w", providerType, key, err)
		}
		if _, err := secretStore.Put(ctx, merchantID, name, value); err != nil {
			return fmt.Errorf("store secret %s: %w", name, err)
		}
	}

	accountID := strings.TrimSpace(account.AccountID)
	if accountID == "" {
		if len(account.Secrets) > 0 {
			return nil
		}
		return fmt.Errorf("provider account %q account_id is required when no secrets are declared", providerType)
	}
	evidence := map[string]any{"source": "merchant_config_manifest"}
	if providerKey := strings.TrimSpace(account.ProviderKey); providerKey != "" {
		evidence["provider_key"] = providerKey
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("encode provider account evidence: %w", err)
	}
	displayName := stringPtrIfNotEmpty(account.DisplayName)
	vaultSecretRef := stringPtrIfNotEmpty(account.VaultSecretRef)
	role := stringPtrIfNotEmpty(strings.ToLower(account.Role))
	status := stringPtrIfNotEmpty(strings.ToLower(account.Status))
	mctx := merchant.WithID(ctx, merchantID)
	var row gen.OpenrailsProviderAccount
	if err := database.RunInMerchantConn(mctx, func(ctx context.Context) error {
		var err error
		row, err = database.Gen(ctx).UpsertProviderAccount(ctx, gen.UpsertProviderAccountParams{
			MerchantID:     merchantID.UUID(),
			ProviderType:   providerType,
			AccountID:      accountID,
			DisplayName:    displayName,
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
				ID:           row.ID,
			}); err != nil {
				return fmt.Errorf("demote old primary provider accounts: %w", err)
			}
			if _, err := database.Gen(ctx).PromoteProviderAccountToPrimary(ctx, gen.PromoteProviderAccountToPrimaryParams{
				ID:           row.ID,
				MerchantID:   merchantID.UUID(),
				ProviderType: providerType,
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

func merchantSecretName(providerType, key string) (string, error) {
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	key = strings.ToLower(strings.TrimSpace(key))
	switch providerType {
	case config.ProcessorTypeStripe:
		switch key {
		case "secret_key":
			return merchants.SecretStripeSecretKey, nil
		case "webhook_signing_secret", "webhook_secret":
			return merchants.SecretStripeWebhookSigning, nil
		case "webhook_signing_secret_thin", "webhook_secret_thin":
			return merchants.SecretStripeWebhookSigningThin, nil
		}
	case config.ProcessorTypeNMI, "mobius":
		switch key {
		case "production_key", "security_key", "secret_key":
			return merchants.SecretNMIMobiusProductionKey, nil
		case "tokenization_key":
			return merchants.SecretNMIMobiusTokenizationKey, nil
		case "tokenization_url":
			return merchants.SecretNMIMobiusTokenizationURL, nil
		case "webhook_signing_secret", "webhook_secret":
			return merchants.SecretNMIMobiusWebhookSigning, nil
		}
	case config.ProcessorTypeCCBill:
		if key == "account_config" {
			return merchants.SecretCCBillAccountConfig, nil
		}
	case config.ProcessorTypeSolana:
		if key == "private_key" {
			return merchants.SecretSolanaPrivateKey, nil
		}
	}
	return "", fmt.Errorf("unknown merchant provider secret %s.%s", providerType, key)
}

func stringPtrIfNotEmpty(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return &v
}

// provisionMerchantOrg ensures the merchant's dedicated backing org exists and,
// when the merchant declares an issuer, registers it as the org OWNER (wildcard
// authority over this one merchant). The org slug is derived deterministically
// from the merchant slug (1:1 backing org). Idempotent: re-applying converges
// the org + issuer state. (#527)
func provisionMerchantOrg(ctx context.Context, cp *controlplane.ControlPlane, mt ManifestMerchant) (*authcore.Org, error) {
	slug := strings.ToLower(strings.TrimSpace(mt.Slug))
	if slug == "" {
		return nil, fmt.Errorf("merchant slug is required")
	}
	req := authcore.OrgProvisionRequest{Slug: slug}
	if mt.Issuer != nil {
		req.Issuers = []authcore.OrgProvisionIssuer{manifestIssuerToProvision(slug, mt.Issuer)}
	}
	res, err := cp.Core().ProvisionOrg(ctx, req, nil)
	if err != nil {
		return nil, err
	}
	return &res.Org, nil
}

// manifestIssuerToProvision maps a merchant's manifest issuer onto an AuthKit
// remote_application registration, assigning it the `owner` role on the
// merchant's org (#527: the issuer IS the merchant owner — full authority, but
// scoped to this merchant alone since federated authority claims are stripped).
func manifestIssuerToProvision(merchantSlug string, iss *ManifestIssuer) authcore.OrgProvisionIssuer {
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
	return authcore.OrgProvisionIssuer{
		Slug:           appSlug,
		Issuer:         strings.TrimSpace(iss.URI),
		JWKSURI:        strings.TrimSpace(iss.JWKSURI),
		Mode:           mode,
		PublicKeys:     iss.PublicKeys,
		Audiences:      audiences,
		AllowedOrigins: cleanStrings(iss.AllowedOrigins),
		Role:           "owner",
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
