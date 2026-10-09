package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/open-rails/openrails/internal/merchantbootstrap"

	"github.com/goccy/go-yaml"
	"github.com/jackc/pgx/v5"
	koanfyaml "github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	"github.com/open-rails/openrails/internal/signeridentity"
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
	if err := rejectMisplacedMerchantConfigKeys(raw); err != nil {
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
	merchantbootstrap.MergeMerchantConfig(&dst.MerchantDeclaration, src.MerchantDeclaration)
	if src.RemoteApplication != nil {
		dst.RemoteApplication = src.RemoteApplication
	}
}

type MerchantConfig struct {
	config.MerchantDeclaration `yaml:",inline" koanf:",squash"`
	RemoteApplication          *RemoteApplicationConfig `yaml:"remote_application,omitempty" koanf:"remote_application"`
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
	PublicKeys []iam.RemoteApplicationKey `yaml:"public_keys,omitempty" koanf:"public_keys"`
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

func (j StaticJWKConfig) authkitJWK() iam.JWK {
	return iam.JWK{
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
type ProvisionMerchantParams struct {
	// MerchantID is an already resolved, explicit host binding. The outer name
	// boundary must verify the supplied name before passing this immutable scope.
	MerchantID    billing.MerchantID
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
				Environment: config.ExpectedProviderEnvironment(config.IsTestMode(cfg))}
		}
		tn, err := ProvisionMerchant(ctx, ProvisionMerchantParams{
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
	if config.SecretStoreBackend(cfg) == config.SecretBackendSnapshot {
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

func ProvisionMerchant(ctx context.Context, req ProvisionMerchantParams) (*merchants.Merchant, error) {
	slug := billing.NormalizeMerchantSlug(req.Slug)
	mt := req.Merchant
	if err := merchantbootstrap.ValidateMerchantDeclaration(req.Config, mt.MerchantDeclaration); err != nil {
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
		if err := configureMerchantRemoteApplication(ctx, req.ControlPlane, iam.GroupByID(tn.PermissionGroupID), mt.RemoteApplication); err != nil {
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
		mt.Settings = billing.MerchantSettings{}
	}
	if err := reconcileManifestMerchantConfiguration(ctx, req.Config, database, tn.ID, slug, mt.MerchantDeclaration, req.SecretStore, req.SolanaTransit, req.Options); err != nil {
		return nil, fmt.Errorf("merchant bootstrap: configure %q: %w", slug, err)
	}
	return tn, nil
}

func provisionMerchantIdentity(ctx context.Context, cfg *config.Config, database *db.DB, cp *controlplane.ControlPlane, slug string, mt MerchantConfig) (*merchants.Merchant, error) {
	if cp == nil {
		// Embedded: OpenRails runs no AuthKit, so it records no permission-group;
		// permission_group_id stays NULL and the host owns authority.
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
	// administer this merchant only. It is configured before the name is
	// claimed, so a failure leaves nothing to repair.
	if err := billing.ValidateMerchantSlug(slug); err != nil {
		return nil, err
	}
	tn, err := cp.CreateMerchant(ctx, slug, "", func(ctx context.Context, tx pgx.Tx, group iam.GroupRef) error {
		return configureMerchantRemoteApplication(ctx, cp, group, mt.RemoteApplication, authkit.InTx(tx))
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

// remoteApplicationStaticPublicKeys passes the manifest's static JWKs to
// AuthKit, which validates them at registration.
func remoteApplicationStaticPublicKeys(app *RemoteApplicationConfig) []iam.RemoteApplicationKey {
	if app == nil || len(app.JWKS.Keys) == 0 {
		return nil
	}
	out := make([]iam.RemoteApplicationKey, 0, len(app.JWKS.Keys))
	for _, raw := range app.JWKS.Keys {
		jwk := raw.authkitJWK()
		out = append(out, iam.RemoteApplicationKey{JWK: &jwk})
	}
	return out
}

// configureMerchantRemoteApplication registers or updates the merchant's
// manifest remote_application under its group, holding the merchant owner role:
// full `merchant:*` authority over this merchant alone. The system registers
// it (trust root manual). opts places it in the merchant's creation
// transaction.
func configureMerchantRemoteApplication(ctx context.Context, cp *controlplane.ControlPlane, group iam.GroupRef, app *RemoteApplicationConfig, opts ...authkit.Option) error {
	if app == nil {
		return nil
	}
	ra, err := manifestRemoteApplication(app)
	if err != nil {
		return fmt.Errorf("merchant bootstrap: remote_application for group %s: %w", group.ID(), err)
	}
	core := cp.Core()
	stored, err := core.UpsertRemoteApplication(ctx, iam.SystemActor(), group, ra, opts...)
	if err != nil {
		return fmt.Errorf("merchant bootstrap: register remote_application for group %s: %w", group.ID(), err)
	}
	if _, err := core.SetGroupRole(ctx, iam.SystemActor(), group, iam.RemoteApplicationSubject(stored.ID), controlplane.MerchantOwner, opts...); err != nil {
		return fmt.Errorf("merchant bootstrap: grant remote_application owner role for group %s: %w", group.ID(), err)
	}
	return nil
}

// manifestRemoteApplication maps a merchant's manifest remote_application onto
// an AuthKit registration.
func manifestRemoteApplication(app *RemoteApplicationConfig) (iam.RemoteApplication, error) {
	mode := iam.RemoteApplicationModeJWKS
	publicKeys := app.PublicKeys
	if len(app.JWKS.Keys) > 0 {
		publicKeys = remoteApplicationStaticPublicKeys(app)
	}
	if len(publicKeys) > 0 {
		mode = iam.RemoteApplicationModeStatic
	}
	return iam.RemoteApplication{
		Issuer:     strings.TrimSpace(app.Issuer),
		JWKSURI:    strings.TrimSpace(app.JWKSURI),
		Mode:       mode,
		PublicKeys: publicKeys,
		Enabled:    true,
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
	if err := gen.New(conn).LockMerchantManifestBootstrap(ctx, merchantManifestAdvisoryLock); err != nil {
		release()
		return nil, fmt.Errorf("merchant bootstrap: acquire advisory lock: %w", err)
	}
	return release, nil
}

const merchantManifestAdvisoryLock = int64(734252042137424)
const DefaultMerchantConfigManifestPath = merchantbootstrap.DefaultMerchantConfigManifestPath

var validateMerchantSecretOverlay = merchantbootstrap.ValidateMerchantSecretOverlay
var rejectMisplacedMerchantConfigKeys = merchantbootstrap.RejectMisplacedMerchantConfigKeys

type PSPConfig = config.PSPConfig
type CustodianConfig = config.CustodianConfig
type PSPSignerConfig = config.PSPSignerConfig
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
var pruneManifestSecrets = merchantbootstrap.PruneManifestSecrets

type resolvedManifestRailAccount = merchantbootstrap.ResolvedManifestRailAccount

var resolveManifestRailAccount = merchantbootstrap.ResolveManifestRailAccount

func SeedMerchantManifestSecretPlane(ctx context.Context, cfg *config.Config, id billing.MerchantID, mt MerchantConfig, store merchants.MerchantSecretStore, transit solana.TransitClient) error {
	return merchantbootstrap.SeedMerchantManifestSecretPlane(ctx, cfg, id, mt.MerchantDeclaration, store, transit)
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
