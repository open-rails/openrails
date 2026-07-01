package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/goccy/go-yaml"
	"github.com/jackc/pgx/v5"
	koanfyaml "github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/pkg/merchant"
)

const merchantManifestAdvisoryLock = int64(734252042137424)

const DefaultMerchantConfigManifestPath = "/etc/openrails/merchants.yaml"

type BillingConfig struct {
	Version   int                       `yaml:"version" koanf:"version"`
	Merchants map[string]MerchantConfig `yaml:"merchants" koanf:"merchants"`
}

// LoadMerchantConfigManifest reads and validates a merchant config manifest.
func LoadMerchantConfigManifest(path string) (*BillingConfig, error) {
	return LoadMerchantConfigManifestFiles(path)
}

// LoadMerchantConfigManifestFiles loads a merchant config YAML file plus optional
// structured YAML overlays through koanf, then validates the merged tree.
func LoadMerchantConfigManifestFiles(path string, overlays ...string) (*BillingConfig, error) {
	k := koanf.New(".")
	for _, p := range append([]string{path}, overlays...) {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("read merchant config manifest %s: %w", p, err)
		}
		if err := k.Load(file.Provider(p), koanfyaml.Parser()); err != nil {
			return nil, fmt.Errorf("load merchant config manifest %s: %w", p, err)
		}
	}
	var manifest BillingConfig
	if err := k.Unmarshal("", &manifest); err != nil {
		return nil, fmt.Errorf("unmarshal merchant config manifest: %w", err)
	}
	for _, key := range []string{"auth", "users", "groups", "roles", "permissions", "catalogs", "products"} {
		if k.Exists(key) {
			return nil, fmt.Errorf("merchant config manifest does not accept %q; use the matching push command", key)
		}
	}
	if len(manifest.Merchants) == 0 {
		return nil, fmt.Errorf("merchant config manifest must declare at least one merchant")
	}
	if err := validateMerchantManifestShape(&manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func LoadMerchantConfigManifestBytes(raw []byte) (*BillingConfig, error) {
	manifest, err := ParseMerchantConfigManifest(raw)
	if err != nil {
		return nil, err
	}
	k := koanf.New(".")
	if err := k.Load(env.Provider(MerchantBillingEnvPrefix, ".", MerchantBillingEnvKey), nil); err != nil {
		return nil, fmt.Errorf("load merchant config env overlay: %w", err)
	}
	if len(k.Keys()) > 0 {
		var overlay BillingConfig
		if err := k.Unmarshal("", &overlay); err != nil {
			return nil, fmt.Errorf("unmarshal merchant config env overlay: %w", err)
		}
		mergeMerchantConfigManifest(manifest, &overlay)
		if err := validateMerchantManifestShape(manifest); err != nil {
			return nil, err
		}
	}
	return manifest, nil
}

// ParseMerchantConfigManifest parses the merchant config manifest consumed by
// push-merchant-config. Bootstrap authority and catalog state are intentionally
// rejected by the strict YAML decoder.
func ParseMerchantConfigManifest(raw []byte) (*BillingConfig, error) {
	var manifest BillingConfig
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

func mergeMerchantConfigManifest(dst, src *BillingConfig) {
	if dst == nil || src == nil {
		return
	}
	if src.Version != 0 {
		dst.Version = src.Version
	}
	if len(src.Merchants) == 0 {
		return
	}
	if dst.Merchants == nil {
		dst.Merchants = map[string]MerchantConfig{}
	}
	for slug, srcMerchant := range src.Merchants {
		dstMerchant := dst.Merchants[slug]
		mergeMerchantConfig(&dstMerchant, srcMerchant)
		dst.Merchants[slug] = dstMerchant
	}
}

func mergeMerchantConfig(dst *MerchantConfig, src MerchantConfig) {
	if strings.TrimSpace(src.DisplayName) != "" {
		dst.DisplayName = src.DisplayName
	}
	if src.Issuer != nil {
		dst.Issuer = src.Issuer
	}
	mergeMerchantProfileConfig(&dst.Profile, src.Profile)
	if src.Invoice != nil {
		if dst.Invoice == nil {
			dst.Invoice = &InvoiceConfig{}
		}
		mergeInvoiceConfig(dst.Invoice, src.Invoice)
	}
	if len(src.DelegatedInvokerWastedSpendWindows) > 0 {
		dst.DelegatedInvokerWastedSpendWindows = src.DelegatedInvokerWastedSpendWindows
	}
	if len(src.ProviderAccounts) > 0 {
		if dst.ProviderAccounts == nil {
			dst.ProviderAccounts = map[string]ProviderAccountConfig{}
		}
		for key, srcRails := range src.ProviderAccounts {
			dstRails := dst.ProviderAccounts[key]
			if dstRails == nil {
				dstRails = ProviderAccountConfig{}
			}
			for rail, srcAccount := range srcRails {
				dstAccount := dstRails[rail]
				mergeProviderRailAccountConfig(&dstAccount, srcAccount)
				dstRails[rail] = dstAccount
			}
			dst.ProviderAccounts[key] = dstRails
		}
	}
}

func mergeMerchantProfileConfig(dst *MerchantProfileConfig, src MerchantProfileConfig) {
	if strings.TrimSpace(src.DisplayName) != "" {
		dst.DisplayName = src.DisplayName
	}
	if strings.TrimSpace(src.LogoURL) != "" {
		dst.LogoURL = src.LogoURL
	}
	if strings.TrimSpace(src.FromEmail) != "" {
		dst.FromEmail = src.FromEmail
	}
	if strings.TrimSpace(src.SupportURL) != "" {
		dst.SupportURL = src.SupportURL
	}
}

func mergeInvoiceConfig(dst, src *InvoiceConfig) {
	if src.CollectionThreshold != nil {
		dst.CollectionThreshold = src.CollectionThreshold
	}
	if src.MonthlyFloor != nil {
		dst.MonthlyFloor = src.MonthlyFloor
	}
	if strings.TrimSpace(src.BillingPeriodBoundary) != "" {
		dst.BillingPeriodBoundary = src.BillingPeriodBoundary
	}
}

func mergeProviderRailAccountConfig(dst *ProviderRailAccountConfig, src ProviderRailAccountConfig) {
	if strings.TrimSpace(src.Environment) != "" {
		dst.Environment = src.Environment
	}
	if strings.TrimSpace(src.AccountID) != "" {
		dst.AccountID = src.AccountID
	}
	if strings.TrimSpace(src.VaultSecretRef) != "" {
		dst.VaultSecretRef = src.VaultSecretRef
	}
	if src.Archived {
		dst.Archived = true
	}
	if src.Signer != nil {
		dst.Signer = src.Signer
	}
	if len(src.Secrets) > 0 {
		if dst.Secrets == nil {
			dst.Secrets = map[string]string{}
		}
		for key, value := range src.Secrets {
			dst.Secrets[key] = value
		}
	}
	if len(src.Settings) > 0 {
		if dst.Settings == nil {
			dst.Settings = map[string]any{}
		}
		for key, value := range src.Settings {
			dst.Settings[key] = value
		}
	}
}

type MerchantConfig struct {
	DisplayName string `yaml:"display_name" koanf:"display_name"`
	// Issuer is the host application's JWKS/public-key trust for THIS merchant
	// (#527). When set, it is registered as a remote_application and made the
	// `owner` of the merchant's permission-group, so the host app's delegated tokens
	// fully administer this merchant — and only this merchant. Optional: a
	// merchant with no issuer (e.g. embedded mode, where the host authenticates
	// in-process) is still provisioned with its permission-group.
	Issuer  *IssuerConfig         `yaml:"issuer,omitempty" koanf:"issuer"`
	Profile MerchantProfileConfig `yaml:"profile,omitempty" koanf:"profile"`
	// Invoice is the merchant's billing/collection policy (#643/#646): when/how the
	// accrued balance is invoiced. Omitted leaves all values at the service default;
	// an omitted field within the block leaves that field as-is.
	Invoice *InvoiceConfig `yaml:"invoice,omitempty" koanf:"invoice"`
	// DelegatedInvokerWastedSpendWindows are merchant-wide abuse cutoffs for
	// delegated invokers (#646): per-window spend ceilings on wasted (failed/abused)
	// generation. Empty leaves the service-default windows (burst 15m/$5, sustained 5h/$20).
	DelegatedInvokerWastedSpendWindows []BudgetWindowConfig             `yaml:"delegated_invoker_wasted_spend_windows,omitempty" koanf:"delegated_invoker_wasted_spend_windows"`
	ProviderAccounts                   map[string]ProviderAccountConfig `yaml:"provider_accounts,omitempty" koanf:"provider_accounts"`
}

// InvoiceConfig is the merchant invoice/collection policy block, mirroring
// the merchant_configurations invoice fields. Amounts are in the currency's micros.
type InvoiceConfig struct {
	// CollectionThreshold: invoice an arrears customer once their accrued balance
	// reaches this (micros). Default 50_000_000 ($50).
	CollectionThreshold *int64 `yaml:"collection_threshold,omitempty" koanf:"collection_threshold"`
	// MonthlyFloor: don't bother collecting below this (micros). Default 1_000_000 ($1).
	MonthlyFloor *int64 `yaml:"monthly_floor,omitempty" koanf:"monthly_floor"`
	// BillingPeriodBoundary: calendar_month | anniversary | fixed_interval.
	// Default fixed_interval (rolling 30d). calendar_month resets on the 1st.
	BillingPeriodBoundary string `yaml:"billing_period_boundary,omitempty" koanf:"billing_period_boundary"`
}

// BudgetWindowConfig is one delegated-invoker wasted-spend window. Window is a
// Go duration ("15m", "5h"); Limit is the per-window ceiling in the currency's micros.
type BudgetWindowConfig struct {
	Key      string `yaml:"key" koanf:"key"`
	Window   string `yaml:"window" koanf:"window"`
	Limit    int64  `yaml:"limit" koanf:"limit"`
	Currency string `yaml:"currency,omitempty" koanf:"currency"`
}

// IssuerConfig declares the host-app issuer trusted for a merchant. Provide
// exactly one trust source: jwks_uri (preferred — AuthKit auto-refetches on
// key rotation) or public_keys (static PEMs, manual rotation).
type IssuerConfig struct {
	// URI is the issuer's `iss` value (the host app's token issuer URL).
	URI string `yaml:"uri" koanf:"uri"`
	// JWKSURI is the issuer's JWKS endpoint. Mutually exclusive with PublicKeys.
	JWKSURI string `yaml:"jwks_uri,omitempty" koanf:"jwks_uri"`
	// PublicKeys are static verification keys (PEM). Mutually exclusive with JWKSURI.
	PublicKeys []authkit.RemoteAppKey `yaml:"public_keys,omitempty" koanf:"public_keys"`
	// AllowedOrigins is the browser Origin allow-list for delegated requests.
	AllowedOrigins []string `yaml:"allowed_origins,omitempty" koanf:"allowed_origins"`
	// Slug overrides the remote_application slug (defaults to "<merchant>-app").
	Slug string `yaml:"slug,omitempty" koanf:"slug"`
}

type MerchantProfileConfig struct {
	DisplayName string `yaml:"display_name,omitempty" koanf:"display_name"`
	LogoURL     string `yaml:"logo_url,omitempty" koanf:"logo_url"`
	FromEmail   string `yaml:"from_email,omitempty" koanf:"from_email"`
	SupportURL  string `yaml:"support_url,omitempty" koanf:"support_url"`
}

type ProviderAccountConfig map[string]ProviderRailAccountConfig

type ProviderRailAccountConfig struct {
	Environment    string                       `yaml:"environment,omitempty" koanf:"environment"`
	AccountID      string                       `yaml:"account_id,omitempty" koanf:"account_id"`
	VaultSecretRef string                       `yaml:"vault_secret_ref,omitempty" koanf:"vault_secret_ref"`
	Archived       bool                         `yaml:"archived,omitempty" koanf:"archived"`
	Signer         *ProviderAccountSignerConfig `yaml:"signer,omitempty" koanf:"signer"`
	Secrets        map[string]string            `yaml:"secrets,omitempty" koanf:"secrets"`
	Settings       map[string]any               `yaml:"settings,omitempty" koanf:"settings"`
}

type ProviderAccountSignerConfig struct {
	Mode string `yaml:"mode,omitempty" koanf:"mode"`
	Key  string `yaml:"key,omitempty" koanf:"key"`
}

// MerchantManifestReconcileOptions selects the apply tier (#527). The default
// (both false) is additive + seed-once. Startup provisioning always uses the
// default; the destructive tiers are opt-in via the CLI and never run on boot.
type MerchantManifestReconcileOptions struct {
	// Insert creates missing merchant/issuer/profile/provider-account/secret
	// state declared by the manifest. Manual CLI runs default to plan-only until
	// this or another mutation flag is set.
	Insert bool
	// Overwrite re-asserts manifest values over existing state. Without it,
	// SECRETS are seed-once: a secret already present is left untouched, so a
	// value rotated out of band (via the admin API) is never reverted to the
	// manifest seed. Merchant/issuer/profile are idempotently ensured either
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
	ResolveManifestProviderAccount(ctx context.Context, cfg *config.Config, rail, environment string, account ProviderRailAccountConfig, secrets manifestSecretValues) (manifestProviderIdentity, error)
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
// AuthKit permission-group and optional issuer-as-owner before recording
// permission_group_id. Embedded calls it with only Database, which registers an
// ownerless merchant row and applies the same profile/provider-account
// configuration path without touching AuthKit or startup bootstrap markers.
type ProvisionMerchantRequest struct {
	Config        *config.Config
	ControlPlane  *controlplane.ControlPlane
	Database      *db.DB
	SecretStore   merchants.MerchantSecretStore
	SolanaTransit solana.TransitClient
	Slug          string
	Merchant      MerchantConfig
	Options       MerchantManifestReconcileOptions
}

// ReconcileMerchantManifestData provisions merchants and issuer ownership
// declared by a merchant config manifest. It is the single
// merchant-provisioning entry point for push-merchant-config and embedded
// startup paths. Issuer registration is declarative and does not fetch the
// JWKS, so it succeeds even when the issuer's app is not yet running.
func ReconcileMerchantManifestData(ctx context.Context, cfg *config.Config, cp *controlplane.ControlPlane, manifest *BillingConfig, opts MerchantManifestReconcileOptions) error {
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

	for _, slug := range sortedMerchantKeys(manifest.Merchants) {
		mt := manifest.Merchants[slug]
		tn, err := ProvisionMerchant(ctx, ProvisionMerchantRequest{
			Config:        cfg,
			ControlPlane:  cp,
			Database:      database,
			SecretStore:   secretStore,
			SolanaTransit: secretBackend.SolanaTransit,
			Slug:          slug,
			Merchant:      mt,
			Options:       opts,
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
	slug := merchant.NormalizeSlug(req.Slug)
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

	tn, found, err := lookupManifestMerchant(ctx, database, slug)
	if err != nil {
		return nil, fmt.Errorf("merchant bootstrap: lookup %q: %w", slug, err)
	}
	if !found {
		if !req.Options.Insert {
			return nil, fmt.Errorf("merchant bootstrap: merchant %q is missing; rerun with --insert to create it", slug)
		}
		tn, err = provisionMerchantIdentity(ctx, database, req.ControlPlane, slug, mt)
		if err != nil {
			return nil, err
		}
	} else if req.ControlPlane != nil && mt.Issuer != nil && req.Options.Overwrite {
		if _, err := provisionMerchantGroup(ctx, req.ControlPlane, slug, mt); err != nil {
			return nil, fmt.Errorf("merchant bootstrap: update merchant group/issuer for %q: %w", slug, err)
		}
	}

	// Keep an existing merchant's display name in sync with the manifest (the
	// create path already set it via RegisterMerchant). Idempotent upsert; an
	// empty manifest display name leaves the stored one untouched (COALESCE).
	if found && strings.TrimSpace(mt.DisplayName) != "" {
		if _, err := db.RegisterMerchant(ctx, database.Qx(ctx), db.RegisterMerchantOptions{Slug: slug, DisplayName: mt.DisplayName}); err != nil {
			return nil, fmt.Errorf("merchant bootstrap: sync display name for %q: %w", slug, err)
		}
	}

	if err := reconcileManifestMerchantConfiguration(ctx, req.Config, database, tn.ID, slug, mt, req.SecretStore, req.SolanaTransit, req.Options); err != nil {
		return nil, fmt.Errorf("merchant bootstrap: configure %q: %w", slug, err)
	}
	return tn, nil
}

func provisionMerchantIdentity(ctx context.Context, database *db.DB, cp *controlplane.ControlPlane, slug string, mt MerchantConfig) (*merchants.Merchant, error) {
	if cp == nil {
		// Embedded: OpenRails runs no AuthKit, so it creates/records no permission-group.
		// The merchant's permission-group is the host's AuthKit permission-group of the SAME slug
		// (#541 — merchant slug == group slug); permission_group_id stays NULL here and
		// is set only in standalone, where OpenRails owns the group.
		id, err := db.RegisterMerchant(ctx, database.Qx(ctx), db.RegisterMerchantOptions{Slug: slug, DisplayName: mt.DisplayName})
		if err != nil {
			return nil, err
		}
		tn, found, err := lookupManifestMerchant(ctx, database, slug)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("merchant bootstrap: registered merchant %q but could not read it back", slug)
		}
		tn.ID = id
		return tn, nil
	}

	// #567: the merchant IS a top-level permission-group (child of root). When
	// the merchant declares an issuer, AuthKit registers it as a
	// remote_application nested under the merchant group with the `owner` role so
	// host-app delegated tokens administer this merchant only.
	groupID, err := provisionMerchantGroup(ctx, cp, slug, mt)
	if err != nil {
		return nil, fmt.Errorf("merchant bootstrap: provision merchant group/issuer for %q: %w", slug, err)
	}
	svc, err := merchants.NewService(cp.Pool(), nil)
	if err != nil {
		return nil, err
	}
	tn, err := svc.Provision(ctx, merchants.ProvisionRequest{
		Slug:              slug,
		PermissionGroupID: groupID,
	})
	if err != nil {
		return nil, fmt.Errorf("merchant bootstrap: provision %q: %w", slug, err)
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

func sortedMerchantKeys(in map[string]MerchantConfig) []string {
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type providerAccountEntry struct {
	key    string
	rail   string
	config ProviderRailAccountConfig
}

func providerAccountEntries(in map[string]ProviderAccountConfig) []providerAccountEntry {
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]providerAccountEntry, 0, len(in))
	for _, key := range keys {
		rails := make([]string, 0, len(in[key]))
		for rail := range in[key] {
			rails = append(rails, rail)
		}
		sort.Strings(rails)
		for _, rail := range rails {
			out = append(out, providerAccountEntry{key: key, rail: rail, config: in[key][rail]})
		}
	}
	return out
}

func reconcileManifestMerchantConfiguration(ctx context.Context, cfg *config.Config, database *db.DB, merchantID merchant.ID, slug string, mt MerchantConfig, secretStore merchants.MerchantSecretStore, transit solana.TransitClient, opts MerchantManifestReconcileOptions) error {
	mctx := merchant.WithID(ctx, merchantID)
	// Apply the merchant_configurations payload (#646): profile, invoice/collection
	// policy, and delegated-invoker abuse windows. Load once, mutate only the
	// declared parts (omit = leave-as-is), upsert if anything changed.
	if hasManifestProfile(mt.Profile) || mt.Invoice != nil || len(mt.DelegatedInvokerWastedSpendWindows) > 0 {
		store := merchantconfig.NewStore(database)
		conf, _, err := store.Get(mctx)
		if err != nil {
			return fmt.Errorf("load merchant configuration: %w", err)
		}
		if hasManifestProfile(mt.Profile) {
			conf.Profile = models.MerchantProfileConfiguration{
				DisplayName: strings.TrimSpace(mt.Profile.DisplayName),
				LogoURL:     strings.TrimSpace(mt.Profile.LogoURL),
				FromEmail:   strings.TrimSpace(mt.Profile.FromEmail),
				SupportURL:  strings.TrimSpace(mt.Profile.SupportURL),
			}
			if conf.Profile.DisplayName == "" {
				conf.Profile.DisplayName = strings.TrimSpace(mt.DisplayName)
			}
		}
		if mt.Invoice != nil {
			if mt.Invoice.CollectionThreshold != nil {
				conf.InvoiceCollectionThreshold = mt.Invoice.CollectionThreshold
			}
			if mt.Invoice.MonthlyFloor != nil {
				conf.InvoiceMonthlyFloor = mt.Invoice.MonthlyFloor
			}
			if b := strings.TrimSpace(mt.Invoice.BillingPeriodBoundary); b != "" {
				conf.InvoiceBillingBoundary = b
			}
		}
		if len(mt.DelegatedInvokerWastedSpendWindows) > 0 {
			windows := make([]models.BudgetWindowPolicy, 0, len(mt.DelegatedInvokerWastedSpendWindows))
			for _, w := range mt.DelegatedInvokerWastedSpendWindows {
				d, err := time.ParseDuration(strings.TrimSpace(w.Window))
				if err != nil {
					return fmt.Errorf("delegated_invoker_wasted_spend_windows %q: window: %w", w.Key, err)
				}
				windows = append(windows, models.BudgetWindowPolicy{
					Key:           strings.TrimSpace(w.Key),
					WindowSeconds: int64(d / time.Second),
					Limit:         w.Limit,
					Currency:      strings.TrimSpace(w.Currency),
				})
			}
			conf.DelegatedInvokerWastedSpendWindows = windows
		}
		if err := store.Upsert(mctx, conf); err != nil {
			return fmt.Errorf("upsert merchant configuration: %w", err)
		}
	}

	for _, entry := range providerAccountEntries(mt.ProviderAccounts) {
		if secretStore == nil {
			return fmt.Errorf("merchant bootstrap: provider account secrets require a secret store")
		}
		if err := reconcileManifestProviderAccount(ctx, cfg, database, merchantID, slug, entry.key, entry.rail, entry.config, secretStore, transit, opts); err != nil {
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
func pruneManifestSecrets(ctx context.Context, merchantID merchant.ID, mt MerchantConfig, secretStore merchants.MerchantSecretStore) error {
	declared := map[string]struct{}{}
	for _, entry := range providerAccountEntries(mt.ProviderAccounts) {
		rail := normalizeManifestRail(entry.rail)
		environment, err := normalizeProviderEnvironment(entry.config.Environment)
		if err != nil {
			return err
		}
		accountID := strings.TrimSpace(entry.config.AccountID)
		if accountID == "" {
			return fmt.Errorf("provider account %q account_id is required before pruning secrets", rail)
		}
		for key := range entry.config.Secrets {
			name, err := merchants.ProviderAccountSecretName(rail, environment, accountID, key)
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

func hasManifestProfile(p MerchantProfileConfig) bool {
	return strings.TrimSpace(p.DisplayName) != "" ||
		strings.TrimSpace(p.LogoURL) != "" ||
		strings.TrimSpace(p.FromEmail) != "" ||
		strings.TrimSpace(p.SupportURL) != ""
}

func reconcileManifestProviderAccount(ctx context.Context, cfg *config.Config, database *db.DB, merchantID merchant.ID, merchantSlug, localKey, rail string, account ProviderRailAccountConfig, secretStore merchants.MerchantSecretStore, transit solana.TransitClient, opts MerchantManifestReconcileOptions) error {
	rail = normalizeManifestRail(rail)
	if rail == "" {
		return fmt.Errorf("provider account rail is required")
	}
	environment, err := normalizeProviderEnvironment(account.Environment)
	if err != nil {
		return err
	}
	secrets, err := newManifestSecretValues(rail, account.Secrets)
	if err != nil {
		return err
	}
	resolver := opts.IdentityResolver
	if resolver == nil {
		resolver = defaultManifestProviderIdentityResolver{}
	}
	identity, err := resolver.ResolveManifestProviderAccount(ctx, cfg, rail, environment, account, secrets)
	if err != nil {
		return err
	}
	accountID := strings.TrimSpace(identity.AccountID)
	if accountID == "" {
		return fmt.Errorf("provider account %q identity resolution returned an empty account id", rail)
	}
	signerEvidence, err := manifestProviderSignerEvidence(ctx, rail, accountID, account, secrets, transit)
	if err != nil {
		return err
	}
	for key, value := range account.Secrets {
		name, err := merchants.ProviderAccountSecretName(rail, environment, accountID, key)
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
		value, err := secrets.Resolve(key, value)
		if err != nil {
			return fmt.Errorf("resolve secret %s.%s: %w", rail, key, err)
		}
		if _, err := secretStore.Put(ctx, merchantID, name, value); err != nil {
			return fmt.Errorf("store secret %s: %w", name, err)
		}
	}
	vaultSecretRef := stringPtrIfNotEmpty(account.VaultSecretRef)
	reconcileStripeWebhook := func() error {
		if rail != string(models.RailStripe) {
			return nil
		}
		res, err := catalog.ReconcileManagedStripeWebhook(ctx, catalog.ManagedStripeWebhookParams{
			Config:              cfg,
			SecretStore:         secretStore,
			MerchantID:          merchantID,
			MerchantSlug:        merchantSlug,
			ProviderEnvironment: environment,
			ProviderAccountID:   accountID,
			EnabledEvents:       webhooks.HandledStripeEventTypes,
		})
		if err != nil {
			return fmt.Errorf("reconcile stripe webhook endpoint: %w", err)
		}
		fields := log.Fields{"merchant": merchantSlug, "stripe_account_id": accountID}
		if res.Skipped {
			fields["reason"] = res.SkipReason
			log.WithFields(fields).Info("merchant bootstrap: stripe webhook endpoint reconcile skipped")
		} else {
			fields["action"] = res.Result.Action
			fields["endpoint_id"] = res.Result.EndpointID
			log.WithFields(fields).Info("merchant bootstrap: stripe webhook endpoint reconciled")
		}
		return nil
	}

	found := false
	if err := database.RunInMerchantConn(merchant.WithID(ctx, merchantID), func(ctx context.Context) error {
		_, err := database.Gen(ctx).GetProviderAccountByIdentity(ctx, gen.GetProviderAccountByIdentityParams{
			MerchantID:  merchantID.UUID(),
			Rail:        rail,
			Environment: stringPtrIfNotEmpty(environment),
			AccountID:   accountID,
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
		return fmt.Errorf("lookup provider account %s:%s:%s: %w", rail, environment, accountID, err)
	}
	if !found && !opts.Insert {
		return fmt.Errorf("provider account %s:%s:%s is missing; rerun with --insert to create it", rail, environment, accountID)
	}
	if found && !opts.Overwrite {
		return reconcileStripeWebhook()
	}
	displayName := identity.DisplayName
	if n := strings.TrimSpace(localKey); n != "" {
		displayName = &n
	}
	evidence := identity.Evidence
	if evidence == nil {
		evidence = map[string]any{"source": "merchant_config_manifest"}
	}
	if signerEvidence != nil {
		evidence["signer"] = signerEvidence
	}
	if len(account.Settings) > 0 {
		evidence["settings"] = account.Settings
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("encode provider account evidence: %w", err)
	}
	// #650: a provider account belongs to exactly one merchant. Fail with a clear
	// error if another merchant already owns this identity, rather than letting the
	// global-uniqueness upsert reject it with an opaque unique-violation under RLS.
	if err := merchants.AssertProviderAccountUnowned(ctx, gen.New(database.Pool()), merchantID.UUID(), rail, environment, accountID); err != nil {
		return err
	}
	mctx := merchant.WithID(ctx, merchantID)
	if err := database.RunInMerchantConn(mctx, func(ctx context.Context) error {
		_, err := database.Gen(ctx).UpsertProviderAccount(ctx, gen.UpsertProviderAccountParams{
			MerchantID:     merchantID.UUID(),
			Rail:           rail,
			Environment:    stringPtrIfNotEmpty(environment),
			AccountID:      accountID,
			DisplayName:    displayName,
			VaultSecretRef: vaultSecretRef,
			Archived:       &account.Archived,
			Evidence:       evidenceJSON,
		})
		if err != nil {
			return fmt.Errorf("upsert provider account %s:%s: %w", rail, accountID, err)
		}
		return nil
	}); err != nil {
		return err
	}
	return reconcileStripeWebhook()
}

func manifestProviderSignerEvidence(ctx context.Context, rail, accountID string, account ProviderRailAccountConfig, secrets manifestSecretValues, transit solana.TransitClient) (map[string]string, error) {
	if account.Signer == nil {
		if _, ok := secrets.sources["private_key"]; ok && rail == string(models.RailSolana) {
			return map[string]string{"mode": "keypair"}, nil
		}
		return nil, nil
	}
	if rail != string(models.RailSolana) {
		return nil, fmt.Errorf("provider account signer is only supported for solana")
	}
	mode := strings.ToLower(strings.TrimSpace(account.Signer.Mode))
	switch mode {
	case "keypair":
		if _, ok := secrets.sources["private_key"]; !ok {
			return nil, fmt.Errorf("solana signer mode keypair requires secrets.private_key")
		}
		if strings.TrimSpace(account.Signer.Key) != "" {
			return nil, fmt.Errorf("solana signer mode keypair must not set key")
		}
		return map[string]string{"mode": "keypair"}, nil
	case "vault_transit":
		if _, ok := secrets.sources["private_key"]; ok {
			return nil, fmt.Errorf("solana signer mode vault_transit cannot also set secrets.private_key")
		}
		key := strings.TrimSpace(account.Signer.Key)
		if key == "" {
			return nil, fmt.Errorf("solana signer mode vault_transit requires key")
		}
		if transit == nil {
			return nil, fmt.Errorf("solana signer mode vault_transit requires vault.enabled")
		}
		raw, err := transit.PublicKey(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("solana vault transit signer %q public key: %w", key, err)
		}
		if len(raw) != 32 {
			return nil, fmt.Errorf("solana vault transit signer %q public key is %d bytes, want 32", key, len(raw))
		}
		pub := solanago.PublicKeyFromBytes(raw)
		if pub.String() != strings.TrimSpace(accountID) {
			return nil, fmt.Errorf("solana vault transit signer %q public key %s does not match account_id %s", key, pub.String(), accountID)
		}
		return map[string]string{"mode": "vault_transit", "key": key}, nil
	default:
		return nil, fmt.Errorf("solana signer mode must be keypair or vault_transit")
	}
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

func normalizeManifestRail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

type manifestSecretValues struct {
	rail    string
	sources map[string]string
	values  map[string]string
}

func newManifestSecretValues(rail string, sources map[string]string) (manifestSecretValues, error) {
	out := manifestSecretValues{
		rail:    rail,
		sources: map[string]string{},
		values:  map[string]string{},
	}
	for key, value := range sources {
		canonical, err := merchants.NormalizeProviderAccountSecretKey(rail, key)
		if err != nil {
			return out, err
		}
		if _, exists := out.sources[canonical]; exists {
			return out, fmt.Errorf("duplicate provider account secret key %q", canonical)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return out, fmt.Errorf("provider account secret %s.%s is empty", rail, canonical)
		}
		out.sources[canonical] = value
	}
	return out, nil
}

func (v manifestSecretValues) Resolve(key string, fallback string) (string, error) {
	canonical, err := merchants.NormalizeProviderAccountSecretKey(v.rail, key)
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
	value := strings.TrimSpace(source)
	if value == "" {
		return "", fmt.Errorf("provider account secret %s.%s is empty", v.rail, canonical)
	}
	v.values[canonical] = value
	return value, nil
}

func (v manifestSecretValues) ResolveIfPresent(key string) (string, bool, error) {
	canonical, err := merchants.NormalizeProviderAccountSecretKey(v.rail, key)
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

func (defaultManifestProviderIdentityResolver) ResolveManifestProviderAccount(ctx context.Context, cfg *config.Config, rail, environment string, account ProviderRailAccountConfig, secrets manifestSecretValues) (manifestProviderIdentity, error) {
	if accountID := strings.TrimSpace(account.AccountID); accountID != "" {
		return manifestProviderIdentity{
			AccountID: accountID,
			Evidence:  map[string]any{"source": "merchant_config_manifest.account_id"},
		}, nil
	}
	// Auto-discovery via live credentials was removed (#592): account_id must be
	// declared in the manifest.
	return manifestProviderIdentity{}, fmt.Errorf("provider account_id is required for %s (auto-discovery removed; declare account_id in the manifest)", rail)
}

func stringPtrIfNotEmpty(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return &v
}

// provisionMerchantGroup ensures the merchant's top-level permission-group exists
// (`type=merchant`, `resourceRef=slug`, child of `root` — #567) and,
// when the merchant declares an issuer, registers that issuer as a
// remote_application nested under the merchant group and grants it the merchant
// `owner` role (full `merchant:*` authority, scoped to this merchant alone since
// federated authority claims are stripped). Idempotent: re-applying converges the
// group + issuer state. Returns the merchant group's internal id.
func provisionMerchantGroup(ctx context.Context, cp *controlplane.ControlPlane, slug string, mt MerchantConfig) (string, error) {
	slug = merchant.NormalizeSlug(slug)
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
	if errors.Is(err, authkit.ErrGroupNotFound) {
		groupID, err = core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{
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
// AuthKit remote_application registration nested under the merchant group.
func manifestIssuerToRemoteApplication(merchantSlug, groupID string, iss *IssuerConfig) authkit.RemoteApplication {
	appSlug := strings.TrimSpace(iss.Slug)
	if appSlug == "" {
		appSlug = merchantSlug + "-app"
	}
	mode := authkit.RemoteAppModeJWKS
	if len(iss.PublicKeys) > 0 {
		mode = authkit.RemoteAppModeStatic
	}
	return authkit.RemoteApplication{
		Slug:              appSlug,
		PermissionGroupID: groupID,
		Issuer:            strings.TrimSpace(iss.URI),
		JWKSURI:           strings.TrimSpace(iss.JWKSURI),
		Mode:              mode,
		PublicKeys:        iss.PublicKeys,
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
