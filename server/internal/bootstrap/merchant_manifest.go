package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/merchantdocs"

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
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/signeridentity"
	"github.com/open-rails/openrails/server/internal/controlplane"
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

// ReconcileOptions are how a manifest provisions its merchants.
type ReconcileOptions struct {
	// Insert registers a merchant the manifest names that does not exist.
	Insert bool
	// Merchants is the merchants service over the configuration the manifest
	// fills (a file) or reads (Vault); nil opens one for this call.
	Merchants *merchants.Service
	// SolanaTransit reads a vault_transit Solana signer's public key.
	SolanaTransit solana.TransitClient
	// WrapTransit wraps SolanaTransit per merchant (signer-change detection).
	WrapTransit func(slug string, transit solana.TransitClient) solana.TransitClient
	// DeferPSP skips a PSP whose account cannot be derived now.
	DeferPSP func(rail string, err error) bool
}

// ProvisionMerchantParams provisions one manifest merchant (#527).
type ProvisionMerchantParams struct {
	// MerchantID is an already resolved, explicit host binding. The outer name
	// boundary must verify the supplied name before passing this immutable scope.
	MerchantID    billing.MerchantID
	Config        *config.Config
	ControlPlane  *controlplane.ControlPlane
	Database      *db.DB
	Merchants     *merchants.Service
	SolanaTransit solana.TransitClient
	DeferPSP      func(rail string, err error) bool
	Slug          string
	Merchant      MerchantConfig
	Insert        bool
}

// ReconcileMerchantManifestData provisions the merchants a manifest declares:
// it registers missing ones (with Insert) and, with the manifest as the
// configuration, puts each declaration in place. With Vault as the source a
// manifest names merchants only (slug, api_host, remote_application) and is
// refused when it declares more. Issuer registration is declarative and does
// not fetch the JWKS.
func ReconcileMerchantManifestData(ctx context.Context, cfg *config.Config, cp *controlplane.ControlPlane, manifest *BillingConfig, opts ReconcileOptions) error {
	if cp == nil || cp.Core() == nil || cp.Pool() == nil {
		return fmt.Errorf("merchant bootstrap manifest configured but control plane is not enabled")
	}
	if manifest == nil {
		return fmt.Errorf("merchant bootstrap manifest is required")
	}
	if manifest.Version != BootstrapManifestVersion {
		return fmt.Errorf("merchant bootstrap: manifest version must be %d", BootstrapManifestVersion)
	}
	for _, slug := range sortedMerchantKeys(manifest.Merchants) {
		if err := config.RefuseDeclarationBesideVault(cfg, slug, manifest.Merchants[slug].MerchantDeclaration); err != nil {
			return fmt.Errorf("merchant bootstrap: %w", err)
		}
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
	database, err := db.NewWithPGXPool(cp.Pool().Raw(), cp.Pool().Schema())
	if err != nil {
		return fmt.Errorf("wrap control-plane db: %w", err)
	}
	directory, transit := opts.Merchants, opts.SolanaTransit
	if directory == nil {
		opened, err := merchantbootstrap.OpenMerchants(ctx, cfg, database)
		if err != nil {
			return fmt.Errorf("merchant bootstrap: %w", err)
		}
		defer opened.Close()
		directory, transit = opened.Service, opened.SolanaTransit
		if _, ok := directory.Config().Source().(*merchantdocs.FileSource); ok {
			log.Info("merchant bootstrap: without Vault the manifest is read by the server that boots with it; only merchant identities persist here")
		}
	}
	for _, slug := range sortedMerchantKeys(manifest.Merchants) {
		signer := transit
		switch {
		case signer == nil:
		case opts.WrapTransit != nil:
			signer = opts.WrapTransit(slug, signer)
		default:
			// One-off tools fail closed on a changed Transit key too.
			signer = &signeridentity.Transit{TransitClient: signer, DB: database, Directory: directory, Slug: slug,
				Environment: config.ExpectedProviderEnvironment(config.IsTestMode(cfg))}
		}
		tn, err := ProvisionMerchant(ctx, ProvisionMerchantParams{
			Config: cfg, ControlPlane: cp, Database: database, Merchants: directory,
			SolanaTransit: signer, DeferPSP: opts.DeferPSP, Slug: slug, Merchant: manifest.Merchants[slug], Insert: opts.Insert,
		})
		if err != nil {
			return err
		}
		log.WithFields(log.Fields{"merchant": tn.Slug, "merchant_id": tn.ID.String()}).Info("merchant bootstrap: merchant ensured")
	}
	return nil
}

// ProvisionMerchant registers a manifest merchant when missing, then puts its
// declaration in place (merchantbootstrap.ProvisionMerchant).
func ProvisionMerchant(ctx context.Context, req ProvisionMerchantParams) (*merchants.Merchant, error) {
	slug := billing.NormalizeMerchantSlug(req.Slug)
	mt := req.Merchant
	if err := merchantbootstrap.ValidateMerchantDeclaration(req.Config, mt.MerchantDeclaration); err != nil {
		return nil, err
	}
	if req.Database == nil || req.Merchants == nil {
		return nil, fmt.Errorf("merchant provisioning requires the database and the merchant configuration")
	}
	id := req.MerchantID
	if id.IsZero() {
		tn, err := req.Merchants.GetBySlug(ctx, slug)
		switch {
		case err == nil:
			id = tn.ID
		case !errors.Is(err, merchants.ErrMerchantNotFound):
			return nil, fmt.Errorf("merchant bootstrap: lookup %q: %w", slug, err)
		case !req.Insert:
			return nil, fmt.Errorf("merchant bootstrap: merchant %q is missing; rerun with --insert to create it", slug)
		default:
			if tn, err = provisionMerchantIdentity(ctx, req.Database, req.ControlPlane, slug, mt); err != nil {
				return nil, err
			}
			id = tn.ID
		}
	}
	tn, err := merchantbootstrap.ProvisionMerchant(ctx, merchantbootstrap.ProvisionMerchantParams{
		MerchantID: id, Config: req.Config, Database: req.Database, Merchants: req.Merchants,
		Slug: slug, Merchant: mt.MerchantDeclaration, SolanaTransit: req.SolanaTransit, DeferPSP: req.DeferPSP,
	})
	if err != nil {
		return nil, err
	}
	return tn, nil
}

func provisionMerchantIdentity(ctx context.Context, database *db.DB, cp *controlplane.ControlPlane, slug string, mt MerchantConfig) (*merchants.Merchant, error) {
	if cp == nil {
		// Embedded: OpenRails runs no AuthKit, so it records no permission-group;
		// permission_group_id stays NULL and the host owns authority.
		id, err := db.RegisterUnboundMerchant(ctx, database.Qx(ctx), db.RegisterUnboundMerchantOptions{Slug: slug})
		if err != nil {
			return nil, err
		}
		return &merchants.Merchant{ID: id, Slug: slug}, nil
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
	stored, err := core.UpsertRemoteApplication(ctx, iam.SystemIdentity(), group, ra, opts...)
	if err != nil {
		return fmt.Errorf("merchant bootstrap: register remote_application for group %s: %w", group.ID(), err)
	}
	if _, err := core.SetGroupRole(ctx, iam.SystemIdentity(), group, iam.RemoteApplicationSubject(stored.ID), controlplane.MerchantOwner, opts...); err != nil {
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
