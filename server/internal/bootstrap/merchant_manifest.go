package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/merchantdocs"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/server/internal/controlplane"
)

// BillingConfig is the manifest as the server reads it.
type BillingConfig = merchantbootstrap.Manifest[MerchantConfig]

// Declarations is the manifest as the engine reads it: each merchant's
// declaration, without the keys only the server reads (remote_application).
func Declarations(m *BillingConfig) *merchantbootstrap.BillingConfig {
	if m == nil {
		return nil
	}
	out := &merchantbootstrap.BillingConfig{Version: m.Version, Merchants: make(map[string]billing.MerchantDocument, len(m.Merchants))}
	for slug, merchant := range m.Merchants {
		out.Merchants[slug] = merchant.MerchantDocument
	}
	return out
}

// LoadMerchantConfigManifestFile loads and validates the merchant config
// manifest at path.
func LoadMerchantConfigManifestFile(path string) (*BillingConfig, error) {
	return merchantbootstrap.LoadManifestFile(path, checkManifestEntry)
}

// LoadMerchantConfigManifest validates a merchant config manifest.
func LoadMerchantConfigManifest(raw []byte) (*BillingConfig, error) {
	return merchantbootstrap.LoadManifest(raw, checkManifestEntry)
}

type MerchantConfig struct {
	billing.MerchantDocument `yaml:",inline" koanf:",squash"`
	RemoteApplication        *RemoteApplicationConfig `yaml:"remote_application,omitempty" koanf:"remote_application"`
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

// ErrNotProvisioned marks a merchant a serving process could not provision
// (ReconcileOptions.Refuse): it is retried.
var ErrNotProvisioned = errors.New("the merchant is not provisioned")

// ReconcileOptions are how a manifest provisions its merchants.
type ReconcileOptions struct {
	// Insert registers a merchant the manifest names that does not exist.
	Insert bool
	// Merchants is the merchants service over the configuration the manifest
	// fills (a file) or reads (Vault); nil opens one for this call, over
	// Vault.
	Merchants *merchants.Service
	// Vault is the authenticated client Merchants is opened over.
	Vault *vaultapi.Client
	// Refuse, set by a serving process, takes what cannot be served: a
	// merchant that cannot be provisioned is reported to it and the others
	// proceed, and a document the merchant does not serve is refused alone.
	// Unset (an operator command), the first of either fails the call.
	Refuse func(subject string, err error)
}

// ProvisionMerchantParams provisions one manifest merchant (#527).
type ProvisionMerchantParams struct {
	// MerchantID is an already resolved, explicit host binding. The outer name
	// boundary must verify the supplied name before passing this immutable scope.
	MerchantID   billing.MerchantID
	Config       *billing.Config
	ControlPlane *controlplane.ControlPlane
	Database     *db.DB
	Merchants    *merchants.Service
	Slug         string
	Merchant     MerchantConfig
	Insert       bool
	// Tolerate refuses a document the merchant cannot serve alone, instead
	// of failing.
	Tolerate bool
}

// ReconcileMerchantManifestData provisions the merchants a manifest declares:
// it registers missing ones (with Insert) and, with the manifest as the
// configuration, puts each declaration in place. With Vault as the source a
// manifest names merchants only (slug, remote_application) and is
// refused when it declares more. Issuer registration is declarative and does
// not fetch the JWKS.
func ReconcileMerchantManifestData(ctx context.Context, logger *slog.Logger, cfg *billing.Config, cp *controlplane.ControlPlane, manifest *BillingConfig, opts ReconcileOptions) error {
	if cp == nil || cp.Core() == nil || cp.Pool() == nil {
		return fmt.Errorf("merchant bootstrap manifest configured but control plane is not enabled")
	}
	if manifest == nil {
		return fmt.Errorf("merchant bootstrap manifest is required")
	}
	if manifest.Version != merchantbootstrap.BootstrapManifestVersion {
		return fmt.Errorf("merchant bootstrap: manifest version must be %d", merchantbootstrap.BootstrapManifestVersion)
	}
	merchantsDeclared := maps.Clone(manifest.Merchants)
	for _, slug := range slices.Sorted(maps.Keys(merchantsDeclared)) {
		mt := merchantsDeclared[slug]
		if err := config.RefuseDeclarationBesideVault(cfg, slug, mt.MerchantDocument); err != nil {
			if opts.Refuse == nil {
				return fmt.Errorf("merchant bootstrap: %w", err)
			}
			opts.Refuse("merchants."+slug+".declaration", fmt.Errorf("%w; Vault's configuration serves", err))
			merchantsDeclared[slug] = MerchantConfig{MerchantDocument: billing.MerchantDocument{Slug: mt.Slug}, RemoteApplication: mt.RemoteApplication}
		}
	}
	release, err := lockMerchantManifestBootstrap(ctx, cp)
	if err != nil {
		return err
	}
	defer release()

	if len(manifest.Merchants) == 0 {
		logger.InfoContext(ctx, "merchant bootstrap manifest has no merchants")
		return nil
	}
	database, err := db.NewWithPGXPool(logger, cp.Pool().Raw(), cp.Pool().Schema())
	if err != nil {
		return fmt.Errorf("wrap control-plane db: %w", err)
	}
	directory := opts.Merchants
	if directory == nil {
		opened, err := merchantbootstrap.OpenMerchants(ctx, cfg, database, opts.Vault)
		if err != nil {
			return fmt.Errorf("merchant bootstrap: %w", err)
		}
		defer opened.Close()
		directory = opened.Service
		if _, ok := directory.Config().Source().(*merchantdocs.FileSource); ok {
			logger.InfoContext(ctx, "merchant bootstrap: without Vault the manifest is read by the server that boots with it; only merchant identities persist here")
		}
	}
	for _, slug := range slices.Sorted(maps.Keys(manifest.Merchants)) {
		tn, err := ProvisionMerchant(ctx, ProvisionMerchantParams{
			Config: cfg, ControlPlane: cp, Database: database, Merchants: directory,
			Slug: slug, Merchant: merchantsDeclared[slug], Insert: opts.Insert,
			Tolerate: opts.Refuse != nil,
		})
		if err != nil && opts.Refuse != nil {
			opts.Refuse("merchants."+slug, fmt.Errorf("%w; %w", err, ErrNotProvisioned))
			continue
		}
		if err != nil {
			return err
		}
		logger.InfoContext(ctx, "merchant bootstrap: merchant ensured", "merchant", tn.Slug, "merchant_id", tn.ID.String())
	}
	return nil
}

// ProvisionMerchant registers a manifest merchant when missing, then puts its
// declaration in place (merchantbootstrap.ProvisionMerchant).
func ProvisionMerchant(ctx context.Context, req ProvisionMerchantParams) (*merchants.Merchant, error) {
	slug := billing.NormalizeMerchantSlug(req.Slug)
	mt := req.Merchant
	if !req.Tolerate {
		if err := merchantbootstrap.ValidateMerchantDeclaration(req.Config, mt.MerchantDocument); err != nil {
			return nil, err
		}
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
		case !errors.Is(err, billing.ErrMerchantNotFound):
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
	tn, refused, err := merchantbootstrap.ProvisionMerchant(ctx, merchantbootstrap.ProvisionMerchantParams{
		MerchantID: id, Config: req.Config, Database: req.Database, Merchants: req.Merchants,
		Slug: slug, Merchant: mt.MerchantDocument,
	})
	if err != nil {
		return nil, err
	}
	if len(refused) > 0 && !req.Tolerate {
		return nil, fmt.Errorf("merchant bootstrap: configure %q: %w", slug, merchantbootstrap.Rejected(refused))
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

// configureMerchantRemoteApplication declares the merchant's manifest
// remote_application in its group, holding the merchant owner role: full
// `merchant:*` authority over this merchant alone. The operator declares it,
// so its keys may be on a private network. opts places it in the merchant's
// creation transaction.
func configureMerchantRemoteApplication(ctx context.Context, cp *controlplane.ControlPlane, group iam.GroupRef, app *RemoteApplicationConfig, opts ...authkit.Option) error {
	if app == nil {
		return nil
	}
	ra, err := manifestRemoteApplication(app)
	if err != nil {
		return fmt.Errorf("merchant bootstrap: remote_application for group %s: %w", group.ID(), err)
	}
	ra.Role = controlplane.MerchantOwner
	if err := cp.Core().DeclareRemoteApplications(ctx, group, []iam.RemoteApplication{ra}, opts...); err != nil {
		return fmt.Errorf("merchant bootstrap: declare remote_application for group %s: %w", group.ID(), err)
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
			cp.Log.WarnContext(ctx, "merchant bootstrap: close advisory-lock session failed", logx.Err(err))
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
