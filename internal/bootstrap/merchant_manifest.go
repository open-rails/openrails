package bootstrap

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/open-rails/openrails/internal/merchantbootstrap"

	"github.com/goccy/go-yaml"
	koanfyaml "github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
	jwtkit "github.com/open-rails/authkit/jwtkit"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	"github.com/open-rails/openrails/internal/signeridentity"
	"github.com/open-rails/openrails/pkg/merchant"
)

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
	docs := make([][]byte, 0, 1+len(overlays))
	for _, p := range append([]string{path}, overlays...) {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		raw, err := os.ReadFile(p) // #nosec G304 -- operator-supplied manifest path
		if err != nil {
			return nil, fmt.Errorf("read merchant config manifest %s: %w", p, err)
		}
		docs = append(docs, raw)
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("read merchant config manifest: no path given")
	}
	return LoadMerchantConfigManifestWithOverlays(docs[0], docs[1:]...)
}

// ReadMerchantManifestOverlays reads operator-mounted overlay files for
// LoadMerchantConfigManifestWithOverlays. Every listed path must exist.
func ReadMerchantManifestOverlays(paths []string) ([][]byte, error) {
	out := make([][]byte, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		raw, err := os.ReadFile(p) // #nosec G304 -- operator-configured overlay path
		if err != nil {
			return nil, fmt.Errorf("read merchant manifest overlay %s: %w", p, err)
		}
		out = append(out, raw)
	}
	return out, nil
}

// LoadMerchantConfigManifestWithOverlays merges the manifest with structured YAML
// overlays (later wins) and validates the result. Overlays are the caller's
// concern (a host's mounted secret files); the engine reads no env or files.
func LoadMerchantConfigManifestWithOverlays(raw []byte, overlays ...[]byte) (*BillingConfig, error) {
	k := koanf.New(".")
	for i, doc := range append([][]byte{raw}, overlays...) {
		if len(strings.TrimSpace(string(doc))) == 0 {
			continue
		}
		if i > 0 {
			if err := validateMerchantSecretOverlay(doc); err != nil {
				return nil, fmt.Errorf("load merchant config overlay %d: %w", i, err)
			}
		}
		if err := k.Load(rawbytes.Provider(doc), koanfyaml.Parser()); err != nil {
			if i == 0 {
				return nil, fmt.Errorf("load merchant config manifest: %w", err)
			}
			return nil, fmt.Errorf("load merchant config overlay %d: %w", i, err)
		}
	}
	for _, key := range []string{"auth", "users", "groups", "roles", "permissions", "catalogs", "products"} {
		if k.Exists(key) {
			return nil, fmt.Errorf("merchant config manifest does not accept %q; use the matching push command", key)
		}
	}
	// Strict-parse the merged tree: koanf's Unmarshal silently ignores unknown
	// fields, so route the file/overlay path through the same DisallowUnknownField
	// parser as the embedded bytes path — a typo'd field is rejected, not dropped.
	merged, err := k.Marshal(koanfyaml.Parser())
	if err != nil {
		return nil, fmt.Errorf("merge merchant config manifest: %w", err)
	}
	return ParseMerchantConfigManifest(merged)
}

// ParseMerchantConfigManifest parses the merchant config manifest consumed by
// push-merchant-config. Bootstrap authority and catalog state are intentionally
// rejected by the strict YAML decoder.
func ParseMerchantConfigManifest(raw []byte) (*BillingConfig, error) {
	if err := rejectRenamedMerchantConfigKeys(raw); err != nil {
		return nil, fmt.Errorf("parse merchant config manifest: %w", err)
	}
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
	merchantbootstrap.MergeMerchantConfig(&dst.MerchantConfig, src.MerchantConfig)
	if src.RemoteApplication != nil {
		dst.RemoteApplication = src.RemoteApplication
	}
}

type MerchantConfig struct {
	merchantbootstrap.MerchantConfig `yaml:",inline" koanf:",squash"`
	RemoteApplication                *RemoteApplicationConfig `yaml:"remote_application,omitempty" koanf:"remote_application"`
}

// RemoteApplicationConfig declares the host-app remote_application trusted for a
// merchant. Provide exactly one trust source: jwks_uri (auto-rotating), jwks
// (static JSON Web Key Set), or public_keys (static PEMs).
type RemoteApplicationConfig struct {
	// Issuer is the token `iss` value.
	Issuer string `yaml:"issuer" koanf:"issuer"`
	// JWKSURI is the remote JWKS endpoint. Mutually exclusive with JWKS/PublicKeys.
	JWKSURI string `yaml:"jwks_uri,omitempty" koanf:"jwks_uri"`
	// JWKS is a static JWKS document. Mutually exclusive with JWKSURI/PublicKeys.
	JWKS StaticJWKSConfig `yaml:"jwks,omitempty" koanf:"jwks"`
	// PublicKeys are static verification keys (PEM). Mutually exclusive with JWKSURI/JWKS.
	PublicKeys []authkit.RemoteAppKey `yaml:"public_keys,omitempty" koanf:"public_keys"`
	// Slug overrides the remote_application slug (defaults to "<merchant>-app").
	Slug string `yaml:"slug,omitempty" koanf:"slug"`
}

type StaticJWKSConfig struct {
	Keys []StaticJWKConfig `yaml:"keys,omitempty" koanf:"keys"`
}

type StaticJWKConfig struct {
	Kty string `yaml:"kty" koanf:"kty"`
	Use string `yaml:"use,omitempty" koanf:"use"`
	Kid string `yaml:"kid,omitempty" koanf:"kid"`
	Alg string `yaml:"alg,omitempty" koanf:"alg"`
	N   string `yaml:"n,omitempty" koanf:"n"`
	E   string `yaml:"e,omitempty" koanf:"e"`
	Crv string `yaml:"crv,omitempty" koanf:"crv"`
	X   string `yaml:"x,omitempty" koanf:"x"`
	Y   string `yaml:"y,omitempty" koanf:"y"`
}

func (j StaticJWKConfig) authkitJWK() jwtkit.JWK {
	return jwtkit.JWK{
		Kty: j.Kty, Use: j.Use, Kid: j.Kid, Alg: j.Alg,
		N: j.N, E: j.E, Crv: j.Crv, X: j.X, Y: j.Y,
	}
}

// ProvisionMerchant is the single OpenRails merchant-provisioning boundary
// (#527). Standalone calls it with a control plane, which creates/ensures the
// AuthKit permission-group and optional issuer-as-owner before recording
// permission_group_id. Embedded calls it with only Database, which registers an
// ownerless merchant row and applies the same profile/PSP
// configuration path without touching AuthKit or startup bootstrap markers.
type ProvisionMerchantRequest struct {
	// MerchantID is an already resolved, explicit host binding. The outer name
	// boundary must verify the supplied name before passing this immutable scope.
	MerchantID    merchant.ID
	Directory     *merchants.Service
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
	release, err := lockMerchantManifestBootstrap(ctx, cp)
	if err != nil {
		return err
	}
	defer release()

	if len(manifest.Merchants) == 0 {
		log.Info("merchant bootstrap manifest has no merchants")
		return nil
	}

	secretStore, solanaTransit, err := manifestReconcileSecretStore(ctx, cfg, cp, opts)
	if err != nil {
		return err
	}
	database, err := db.NewWithPGXPool(cp.Pool().Raw(), cp.Pool().Schema())
	if err != nil {
		return fmt.Errorf("wrap control-plane db: %w", err)
	}

	directory, err := merchants.NewDirectoryService(database.DataPool())
	if err != nil {
		return err
	}
	for _, slug := range sortedMerchantKeys(manifest.Merchants) {
		mt := manifest.Merchants[slug]
		transit := solanaTransit
		switch {
		case transit == nil:
		case opts.WrapTransit != nil:
			transit = opts.WrapTransit(slug, transit)
		default:
			// One-off tools fail closed on a changed Transit key too.
			transit = &signeridentity.Transit{TransitClient: transit, DB: database, Directory: directory, Slug: slug,
				Environment: config.ExpectedProviderEnvironment(cfg.IsTestMode())}
		}
		tn, err := ProvisionMerchant(ctx, ProvisionMerchantRequest{
			Config:        cfg,
			ControlPlane:  cp,
			Database:      database,
			SecretStore:   secretStore,
			SolanaTransit: transit,
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

// manifestReconcileSecretStore picks where manifest secrets land (#723):
// injected store (mode-1 boot plane) > mode-2 persistent backend > mode-1
// ephemeral memory (CLI runs: DB projections converge, secrets validate but
// are NOT persisted — the running server holds its own from its boot manifest).
func manifestReconcileSecretStore(ctx context.Context, cfg *config.Config, cp *controlplane.ControlPlane, opts MerchantManifestReconcileOptions) (merchants.MerchantSecretStore, solana.TransitClient, error) {
	if opts.SecretStore != nil && opts.SolanaTransit != nil {
		return opts.SecretStore, opts.SolanaTransit, nil
	}
	if opts.SecretStore != nil {
		transitStore, err := merchantsecrets.BuildTransit(ctx, cfg)
		if err == nil {
			err = transitStore.Await(ctx, merchantsecrets.AwaitTimeout)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("merchant bootstrap: %w", err)
		}
		return opts.SecretStore, transitStore.SolanaTransit, nil
	}
	if cfg.SecretStoreBackend() == config.SecretBackendSnapshot {
		log.Info("merchant bootstrap: snapshot credentials validate in memory and are not persisted")
		transitStore, err := merchantsecrets.BuildTransit(ctx, cfg)
		if err == nil {
			err = transitStore.Await(ctx, merchantsecrets.AwaitTimeout)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("merchant bootstrap: %w", err)
		}
		return merchants.NewMemorySecretStore(), transitStore.SolanaTransit, nil
	}
	secretBackend, err := merchantsecrets.Build(ctx, cfg, cp.Pool())
	if err == nil {
		err = secretBackend.Await(ctx, merchantsecrets.AwaitTimeout)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("merchant bootstrap: build secret store: %w", err)
	}
	return secretBackend.Secrets, secretBackend.SolanaTransit, nil
}

func ProvisionMerchant(ctx context.Context, req ProvisionMerchantRequest) (*merchants.Merchant, error) {
	slug := merchant.NormalizeSlug(req.Slug)
	mt := req.Merchant
	if err := merchantbootstrap.ValidateMerchantDeclaration(req.Config, mt.MerchantConfig); err != nil {
		return nil, err
	}
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

	directory := req.Directory
	if directory == nil {
		var err error
		directory, err = merchants.NewDirectoryService(database.DataPool())
		if err != nil {
			return nil, err
		}
		if req.ControlPlane != nil {
			directory.WithNameAuthority(controlplane.MerchantNameAuthority(req.ControlPlane.Core()))
		}
	}
	var tn *merchants.Merchant
	var err error
	if !req.MerchantID.IsZero() {
		tn, err = directory.Get(ctx, req.MerchantID)
		if err == nil {
			tn.Slug = slug
		}
	} else {
		tn, err = directory.GetBySlug(ctx, slug)
	}
	found := err == nil
	if errors.Is(err, merchants.ErrMerchantNotFound) && req.MerchantID.IsZero() {
		err = nil
	}
	if err != nil {
		return nil, fmt.Errorf("merchant bootstrap: lookup %q: %w", slug, err)
	}
	if !found {
		if !req.Options.Insert {
			return nil, fmt.Errorf("merchant bootstrap: merchant %q is missing; rerun with --insert to create it", slug)
		}
		tn, err = provisionMerchantIdentity(ctx, req.Config, database, req.ControlPlane, slug, mt)
		if err != nil {
			return nil, err
		}
	} else if req.ControlPlane != nil && mt.RemoteApplication != nil && req.Options.Overwrite {
		if err := configureMerchantRemoteApplication(ctx, req.ControlPlane, tn.PermissionGroupID, mt.RemoteApplication); err != nil {
			return nil, fmt.Errorf("merchant bootstrap: update merchant group/remote_application for %q: %w", slug, err)
		}
	}

	// Keep an existing merchant's display name in sync with the manifest (the
	// create path already set it). A UUID-scoped update ensures an
	// empty manifest display name leaves the stored one untouched.
	if found && req.Options.Overwrite && strings.TrimSpace(mt.DisplayName) != "" {
		directory, err := merchants.NewDirectoryService(database.DataPool())
		if err != nil {
			return nil, err
		}
		if err := directory.SetDisplayName(ctx, tn.ID, mt.DisplayName); err != nil {
			return nil, fmt.Errorf("merchant bootstrap: sync display name for %q: %w", slug, err)
		}
	}

	// Startup ensures identity and missing accounts. Existing metadata belongs
	// to ordinary Client operations; restarting a declaration cannot reassert it.
	if found && !req.Options.Overwrite {
		mt.DisplayName = ""
		mt.APIHost = ""
		mt.Profile = MerchantProfileConfig{}
		mt.Invoice = nil
		mt.DelegatedInvokerWastedSpendWindows = nil
		mt.CheckoutRouting = nil
		mt.BillingPolicies = nil
		mt.BillingPolicyBindings = nil
	}
	if err := reconcileManifestMerchantConfiguration(ctx, req.Config, database, tn.ID, slug, mt.MerchantConfig, req.SecretStore, req.SolanaTransit, req.Options); err != nil {
		return nil, fmt.Errorf("merchant bootstrap: configure %q: %w", slug, err)
	}
	return tn, nil
}

func provisionMerchantIdentity(ctx context.Context, cfg *config.Config, database *db.DB, cp *controlplane.ControlPlane, slug string, mt MerchantConfig) (*merchants.Merchant, error) {
	if cp == nil {
		// Embedded: OpenRails runs no AuthKit, so it creates/records no permission-group.
		// The merchant's permission-group is the host's AuthKit permission-group of the SAME slug
		// (#541 — merchant slug == group slug); permission_group_id stays NULL here and
		// is set only in standalone, where OpenRails owns the group.
		id, err := db.RegisterUnboundMerchant(ctx, database.Qx(ctx), db.RegisterUnboundMerchantOptions{Slug: slug, DisplayName: mt.DisplayName})
		if err != nil {
			return nil, err
		}
		tn, found, err := lookupManifestMerchant(ctx, database, cp, slug)
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
	// the merchant declares a remote_application, AuthKit nests it under the
	// merchant group with the `owner` role so host-app delegated tokens
	// administer this merchant only.
	groupID, err := provisionMerchantGroup(ctx, cp, slug, mt)
	if err != nil {
		return nil, fmt.Errorf("merchant bootstrap: provision merchant group/remote_application for %q: %w", slug, err)
	}
	svc, err := merchants.NewService(cp.Pool(), nil, config.ExpectedProviderEnvironment(cfg != nil && cfg.IsTestMode()))
	if err != nil {
		return nil, err
	}
	canonical, err := cp.Core().GroupInstanceByID(ctx, groupID)
	if err != nil {
		return nil, err
	}
	tn, _, err := svc.Provision(ctx, merchants.ProvisionRequest{
		Slug:              canonical.InstanceSlug,
		PermissionGroupID: groupID,
	})
	if err != nil {
		return nil, fmt.Errorf("merchant bootstrap: provision %q: %w", slug, err)
	}
	return tn, nil
}

func lookupManifestMerchant(ctx context.Context, database *db.DB, cp *controlplane.ControlPlane, slug string) (*merchants.Merchant, bool, error) {
	dir, err := merchants.NewDirectoryService(database.DataPool())
	if err != nil {
		return nil, false, err
	}
	if cp != nil {
		dir.WithNameAuthority(controlplane.MerchantNameAuthority(cp.Core()))
	}
	row, err := dir.GetBySlug(ctx, slug)
	if errors.Is(err, merchants.ErrMerchantNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return row, true, nil
}

func sortedMerchantKeys(in map[string]MerchantConfig) []string {
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func remoteApplicationStaticPublicKeys(app *RemoteApplicationConfig) ([]authkit.RemoteAppKey, error) {
	if app == nil || len(app.JWKS.Keys) == 0 {
		return nil, nil
	}
	keys := make([]authkit.RemoteAppKey, 0, len(app.JWKS.Keys))
	for _, raw := range app.JWKS.Keys {
		jwk := raw.authkitJWK()
		pub, err := jwtkit.JWKToPublicKey(jwk)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", strings.TrimSpace(jwk.Kid), err)
		}
		der, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			return nil, fmt.Errorf("key %q: marshal public key: %w", strings.TrimSpace(jwk.Kid), err)
		}
		keys = append(keys, authkit.RemoteAppKey{
			KID:          strings.TrimSpace(jwk.Kid),
			PublicKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
		})
	}
	return keys, nil
}

// provisionMerchantGroup ensures the merchant's top-level permission-group exists
// (`type=merchant`, `resourceRef=slug`, child of `root` — #567) and,
// when the merchant declares a remote_application, registers it as a
// remote_application nested under the merchant group and grants it the merchant
// `owner` role (full `merchant:*` authority, scoped to this merchant alone since
// federated authority claims are stripped). Idempotent: re-applying converges the
// group + remote_application state. Returns the merchant group's internal id.
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

	// Idempotently create the merchant permission-group (resolve, else create).
	groupID, err := core.ResolveGroupIDForSlug(ctx, controlplane.MerchantGroup(slug))
	if errors.Is(err, authkit.ErrGroupNotFound) {
		groupID, err = core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{
			Persona:       controlplane.MerchantType,
			InstanceSlug:  slug,
			ParentPersona: authkit.RootPersona,
		})
		if err != nil {
			// #844: concurrent first-create loser — re-read and adopt the
			// winner's group (the Reconcile path holds an advisory lock, but
			// ProvisionMerchant callers do not).
			id, rerr := core.ResolveGroupIDForSlug(ctx, controlplane.MerchantGroup(slug))
			if rerr != nil {
				return "", fmt.Errorf("merchant bootstrap: create merchant group %q: %w", slug, err)
			}
			groupID = id
		}
	} else if err != nil {
		return "", fmt.Errorf("merchant bootstrap: resolve merchant group %q: %w", slug, err)
	}

	if err := configureMerchantRemoteApplication(ctx, cp, groupID, mt.RemoteApplication); err != nil {
		return "", err
	}
	return groupID, nil
}

// Configure the remote application under the already captured group. The public
// name may be renamed or reclaimed between provisioning and role assignment.
func configureMerchantRemoteApplication(ctx context.Context, cp *controlplane.ControlPlane, groupID string, app *RemoteApplicationConfig) error {
	if app == nil {
		return nil
	}
	core := cp.Core()
	group, err := core.GroupInstanceByID(ctx, groupID)
	if err != nil {
		return err
	}
	if group.Persona != controlplane.MerchantType {
		return merchants.ErrMerchantNotFound
	}
	ctx = authcore.WithResolvedGroup(ctx, group, group.InstanceSlug)
	ra, err := manifestRemoteApplicationToAuthKit(group.InstanceSlug, group.ID, app)
	if err != nil {
		return fmt.Errorf("merchant bootstrap: remote_application for group %s: %w", groupID, err)
	}
	stored, err := core.UpsertRemoteApplication(ctx, ra)
	if err != nil {
		return fmt.Errorf("merchant bootstrap: register remote_application for group %s: %w", groupID, err)
	}
	if err := core.OperatorAssignGroupRole(ctx, controlplane.MerchantGroup(group.InstanceSlug), authkit.RemoteAppSubject(stored.ID), controlplane.MerchantRoleOwner); err != nil {
		return fmt.Errorf("merchant bootstrap: grant remote_application owner role for group %s: %w", groupID, err)
	}
	return nil
}

// manifestRemoteApplicationToAuthKit maps a merchant's manifest remote_application
// onto an AuthKit remote_application registration nested under the merchant group.
func manifestRemoteApplicationToAuthKit(merchantSlug, groupID string, app *RemoteApplicationConfig) (authkit.RemoteApplication, error) {
	appSlug := strings.TrimSpace(app.Slug)
	if appSlug == "" {
		appSlug = merchantSlug + "-app"
	}
	mode := authkit.RemoteAppModeJWKS
	publicKeys := app.PublicKeys
	if len(app.JWKS.Keys) > 0 {
		keys, err := remoteApplicationStaticPublicKeys(app)
		if err != nil {
			return authkit.RemoteApplication{}, err
		}
		publicKeys = keys
	}
	if len(publicKeys) > 0 {
		mode = authkit.RemoteAppModeStatic
	}
	return authkit.RemoteApplication{
		Slug:              appSlug,
		PermissionGroupID: groupID,
		Issuer:            strings.TrimSpace(app.Issuer),
		JWKSURI:           strings.TrimSpace(app.JWKSURI),
		Mode:              mode,
		PublicKeys:        publicKeys,
		Enabled:           true,
	}, nil
}

// PostgreSQL session locks belong to a physical connection, not a pool. Own
// this session until reconciliation returns. Hijack frees its pool slot so a
// one-connection host pool can still perform the reconciliation itself. Always
// close the owned connection instead of returning a possibly locked session.
func lockMerchantManifestBootstrap(ctx context.Context, cp *controlplane.ControlPlane) (func(), error) {
	pooled, err := cp.Pool().Raw().Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("merchant bootstrap: acquire lock connection: %w", err)
	}
	conn := pooled.Hijack()
	release := func() {
		if err := conn.Close(context.WithoutCancel(ctx)); err != nil {
			log.WithError(err).Warn("merchant bootstrap: close advisory-lock session failed")
		}
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, merchantManifestAdvisoryLock); err != nil {
		release()
		return nil, fmt.Errorf("merchant bootstrap: acquire advisory lock: %w", err)
	}
	return release, nil
}

const merchantManifestAdvisoryLock = int64(734252042137424)
const DefaultMerchantConfigManifestPath = merchantbootstrap.DefaultMerchantConfigManifestPath

var validateMerchantSecretOverlay = merchantbootstrap.ValidateMerchantSecretOverlay
var rejectRenamedMerchantConfigKeys = merchantbootstrap.RejectRenamedMerchantConfigKeys
var mergeMerchantProfileConfig = merchantbootstrap.MergeMerchantProfileConfig
var mergeInvoiceConfig = merchantbootstrap.MergeInvoiceConfig
var mergeCustodianAccountConfig = merchantbootstrap.MergeCustodianAccountConfig
var mergeProviderRailAccountConfig = merchantbootstrap.MergeProviderRailAccountConfig

type BillingPolicyConfig = merchantbootstrap.BillingPolicyConfig
type BillingPolicyBindingConfig = merchantbootstrap.BillingPolicyBindingConfig
type CheckoutRoutingRuleConfig = merchantbootstrap.CheckoutRoutingRuleConfig
type CheckoutRoutingMatchConfig = merchantbootstrap.CheckoutRoutingMatchConfig

var checkoutRoutingRules = merchantbootstrap.CheckoutRoutingRules

type InvoiceConfig = merchantbootstrap.InvoiceConfig
type BudgetWindowConfig = merchantbootstrap.BudgetWindowConfig
type MerchantProfileConfig = merchantbootstrap.MerchantProfileConfig
type PSPConfig = merchantbootstrap.PSPConfig
type CustodianConfig = merchantbootstrap.CustodianConfig
type CustodianAccountConfig = merchantbootstrap.CustodianAccountConfig
type ProviderRailAccountConfig = merchantbootstrap.ProviderRailAccountConfig
type PSPSignerConfig = merchantbootstrap.PSPSignerConfig
type MerchantManifestReconcileOptions = merchantbootstrap.MerchantManifestReconcileOptions
type ManifestProviderIdentityResolver = merchantbootstrap.ManifestProviderIdentityResolver
type manifestProviderIdentity = merchantbootstrap.ManifestProviderIdentity
type pspEntry = merchantbootstrap.PspEntry

var pspEntries = merchantbootstrap.PspEntries

type custodianEntry = merchantbootstrap.CustodianEntry

var custodianEntries = merchantbootstrap.CustodianEntries

type resolvedManifestCustodian = merchantbootstrap.ResolvedManifestCustodian

var resolveManifestCustodian = merchantbootstrap.ResolveManifestCustodian
var seedManifestCustodianSecrets = merchantbootstrap.SeedManifestCustodianSecrets
var reconcileManifestCustodians = merchantbootstrap.ReconcileManifestCustodians
var nonNilSettings = merchantbootstrap.NonNilSettings
var resolveManifestCustodianReference = merchantbootstrap.ResolveManifestCustodianReference
var reconcileManifestMerchantConfiguration = merchantbootstrap.ReconcileManifestMerchantConfiguration
var reconcileManifestBillingPolicies = merchantbootstrap.ReconcileManifestBillingPolicies
var manifestBudgetWindows = merchantbootstrap.ManifestBudgetWindows
var pruneManifestSecrets = merchantbootstrap.PruneManifestSecrets
var hasManifestProfile = merchantbootstrap.HasManifestProfile

type resolvedManifestRailAccount = merchantbootstrap.ResolvedManifestRailAccount

var resolveManifestRailAccount = merchantbootstrap.ResolveManifestRailAccount

func SeedMerchantManifestSecretPlane(ctx context.Context, cfg *config.Config, id merchant.ID, mt MerchantConfig, store merchants.MerchantSecretStore, transit solana.TransitClient) error {
	return merchantbootstrap.SeedMerchantManifestSecretPlane(ctx, cfg, id, mt.MerchantConfig, store, transit)
}

var reconcileManifestPSP = merchantbootstrap.ReconcileManifestPSP
var manifestProviderSignerEvidence = merchantbootstrap.ManifestProviderSignerEvidence
var solanaLocalKeypairPublicKey = merchantbootstrap.SolanaLocalKeypairPublicKey
var solanaTransitPublicKey = merchantbootstrap.SolanaTransitPublicKey
var manifestProviderEnvironment = merchantbootstrap.ManifestProviderEnvironment
var manifestSolanaNetwork = merchantbootstrap.ManifestSolanaNetwork
var normalizeManifestRail = merchantbootstrap.NormalizeManifestRail

type manifestSecretValues = merchantbootstrap.ManifestSecretValues

var newManifestSecretValues = merchantbootstrap.NewManifestSecretValues

type defaultManifestProviderIdentityResolver = merchantbootstrap.DefaultManifestProviderIdentityResolver

var stringPtrIfNotEmpty = merchantbootstrap.StringPtrIfNotEmpty
