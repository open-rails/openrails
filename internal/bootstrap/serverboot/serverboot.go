// Package serverboot is the standalone-server composition root (#285/#670):
// NewServer wires the framework-neutral HTTP Server onto the application graph
// built by app.BootstrapWithOptions, attaching the mandatory control plane.
package serverboot

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	hostconfig "github.com/open-rails/openrails/hostauth/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/bootstrap"
	server "github.com/open-rails/openrails/internal/http"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/internal/retry"
	"github.com/open-rails/openrails/internal/signeridentity"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/cache"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Result holds the application graph plus the HTTP server created by the
// composition root.
type Result struct {
	App    *app.App
	Server *server.Server
}

// Options controls optional dependency overrides for the standalone server
// composition root.
type Options struct {
	Auth *hostconfig.AuthConfig

	PGXPool *pgxpool.Pool
	Redis   *redis.Client
	Cache   cache.Cache
	Clock   clockwork.Clock

	// Authenticator protects user routes. When nil, standalone uses the attached
	// control plane verifier.
	Authenticator billingauth.Authenticator
	// DelegatedAuthenticator is the optional host-pluggable identity seam for
	// self-service routes. When nil, those routes use control-plane delegation.
	DelegatedAuthenticator billingauth.DelegatedAuthenticator

	// MerchantManifestPath overrides where the MODE-1 boot merchant manifest is
	// read from (#723). Empty uses the conventional
	// bootstrap.DefaultMerchantConfigManifestPath when that file exists.
	MerchantManifestPath string

	// ConsoleAssets is the built admin console SPA (#754); nil = absent.
	// admin_console.enabled without assets is a boot error.
	ConsoleAssets fs.FS

	// NMIProbeV5BaseURL is a test-only seam: overrides the v5 base URL the
	// startup sandbox posture probe hits. Empty in production.
	NMIProbeV5BaseURL string

	ConfiguredMerchant merchant.ID
}

// NewServer constructs the application runtime and the HTTP server graph
// together. ctx is the boot context (see app.BootstrapWithOptions).
func NewServer(ctx context.Context, cfg *config.Config, opts *Options) (*Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// #711: the bootstrap.Options relay layer is gone — call the app
	// composition root directly.
	application, err := app.BootstrapWithOptions(ctx, cfg, &app.BootstrapOptions{
		PGXPool:            optsValue(opts, func(o *Options) *pgxpool.Pool { return o.PGXPool }),
		Redis:              optsValue(opts, func(o *Options) *redis.Client { return o.Redis }),
		Cache:              optsValue(opts, func(o *Options) cache.Cache { return o.Cache }),
		Clock:              optsValue(opts, func(o *Options) clockwork.Clock { return o.Clock }),
		ConfiguredMerchant: optsValue(opts, func(o *Options) merchant.ID { return o.ConfiguredMerchant }),
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap application: %w", err)
	}

	cleanupOnError := true
	defer func() {
		if cleanupOnError {
			_ = application.Close(context.Background())
		}
	}()

	// Standalone always attaches the OpenRails-owned AuthKit control plane
	// (#284/#469), reusing an injected pool when present. Failure is fatal.
	var injectedPool = func() *pgxpool.Pool {
		if opts != nil {
			return opts.PGXPool
		}
		return nil
	}()
	if cperr := embcp.Attach(context.Background(), application, cfg, optsValue(opts, func(o *Options) *hostconfig.AuthConfig { return o.Auth }), injectedPool); cperr != nil {
		return nil, fmt.Errorf("attach control plane: %w", cperr)
	}
	authenticator := optsValue(opts, func(o *Options) billingauth.Authenticator { return o.Authenticator })
	if authenticator == nil {
		authenticator = embcp.Get(application).UserAuthenticator()
		if authenticator == nil {
			return nil, fmt.Errorf("control plane verifier unavailable")
		}
	}

	// MODE 1 (#723): the standalone server loads the merchant manifest at boot —
	// DB rows converge as projections, secrets seed the in-memory plane. A
	// declared-but-unloadable manifest refuses boot; an absent conventional file
	// boots control-plane-only (merchants can be bound later; there is no
	// implied truth to miss). MODE 2 refuses a present manifest (two truths).
	if err := ReconcileBootMerchantManifest(context.Background(), cfg, application,
		optsValue(opts, func(o *Options) string { return o.MerchantManifestPath }),
		optsValue(opts, func(o *Options) string { return o.NMIProbeV5BaseURL })); err != nil {
		return nil, err
	}
	// Request handlers need durable producers even when another process runs
	// the workers. Compose after all components attach and before publishing HTTP.
	if err := application.Runtime.InitRiver(ctx); err != nil {
		return nil, fmt.Errorf("bind standalone job producers: %w", err)
	}

	billingServer, err := server.New(server.Dependencies{
		Config:                 application.Config,
		Cache:                  application.Cache,
		Runtime:                application.Runtime,
		Redis:                  application.RedisClient,
		Authenticator:          authenticator,
		DelegatedAuthenticator: optsValue(opts, func(o *Options) billingauth.DelegatedAuthenticator { return o.DelegatedAuthenticator }),
		ControlPlane:           embcp.Get(application),
		ConsoleAssets:          optsValue(opts, func(o *Options) fs.FS { return o.ConsoleAssets }),
	})
	if err != nil {
		return nil, fmt.Errorf("create billing server: %w", err)
	}

	cleanupOnError = false
	return &Result{App: application, Server: billingServer}, nil
}

func optsValue[T any](opts *Options, pick func(*Options) T) T {
	var zero T
	if opts == nil {
		return zero
	}
	return pick(opts)
}

// ReconcileBootMerchantManifest implements the standalone rows of the #723
// boot matrix, shared by NewServer and cmd/openrails runServer (#847): every
// boot ensures missing identities and reloads host-owned snapshot credentials.
// Existing metadata and archive decisions survive restart. The conventional
// path is optional; an explicitly supplied path must exist.
//
// Only Postgres can block it. A Vault Transit signer uses the runtime's own
// client (logged in in the background): while Vault is down its PSP keeps the
// stored identity, or on a first boot is deferred and provisioned in the
// background once Vault answers; a changed Transit key fails closed until an
// operator approves it (see ApproveSolanaSigner). Posture is verified in the
// background; nmiProbeV5BaseURL is the test-only posture probe seam.
func ReconcileBootMerchantManifest(ctx context.Context, cfg *config.Config, application *app.App, path, nmiProbeV5BaseURL string) error {
	rt := application.Runtime
	if rt == nil {
		return fmt.Errorf("merchant startup requires a runtime")
	}
	// The merchant credential plane (and its Transit client) is built first so
	// the manifest reconcile signs with it rather than a Vault login of its own.
	if err := rt.EnsureMerchantsService(ctx); err != nil {
		log.WithError(err).Warn("merchant credentials unavailable at startup; sandbox PSPs verify on first use")
	}
	slugs, pending, err := reconcileBootMerchantManifest(ctx, cfg, application, path, true)
	if err != nil {
		return err
	}
	rt.ApproveSolanaSigner = func(ctx context.Context, mid merchant.ID, key string) error {
		return approveSolanaSigner(ctx, cfg, application, path, mid, key)
	}
	if pending {
		rt.Go("solana transit signer", func(ctx context.Context) {
			err := retry.Forever(ctx, func(ctx context.Context) error {
				if err := rt.MerchantSecretBackend.State(); err != nil {
					return err
				}
				_, _, err := reconcileBootMerchantManifest(ctx, cfg, application, path, false)
				return err
			}, func(attempt int, err error) {
				if attempt == 0 {
					log.WithError(err).Warn("merchant startup: Solana Transit signer awaits Vault; retrying in the background")
				}
			})
			if err == nil {
				log.Info("merchant startup: Solana Transit signer confirmed by Vault")
			}
		})
	}
	if rt.Merchants == nil {
		return nil
	}
	var declared []merchant.ID
	for _, slug := range slugs {
		if m, err := rt.Merchants.GetBySlug(ctx, slug); err == nil {
			declared = append(declared, m.ID)
		}
	}
	rt.NMIPostureV5BaseURL = nmiProbeV5BaseURL
	rt.StartProviderPosture(declared...)
	return nil
}

// approveSolanaSigner accepts the identity Vault now reports for key and
// re-applies the boot manifest, provisioning it.
func approveSolanaSigner(ctx context.Context, cfg *config.Config, application *app.App, path string, mid merchant.ID, key string) error {
	rt := application.Runtime
	if rt.MerchantSecretBackend == nil || rt.Merchants == nil {
		return fmt.Errorf("no Vault Transit signer is configured")
	}
	if _, err := signeridentity.Approve(ctx, rt.DB, rt.Merchants, rt.MerchantSecretBackend.SolanaTransit, mid, config.ExpectedProviderEnvironment(cfg.IsTestMode()), key); err != nil {
		return err
	}
	if _, _, err := reconcileBootMerchantManifest(ctx, cfg, application, path, false); err != nil {
		return err
	}
	rt.ReportSignerKeyChange(nil)
	return nil
}

// reconcileBootMerchantManifest reports whether a Solana Transit signer fell
// back to its stored identity (or was deferred) because Vault did not answer.
func reconcileBootMerchantManifest(ctx context.Context, cfg *config.Config, application *app.App, path string, tolerateVault bool) ([]string, bool, error) {
	explicit := strings.TrimSpace(path) != ""
	if !explicit {
		path = bootstrap.DefaultMerchantConfigManifestPath
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- path is a boot-time CLI/config value, not request input
	if os.IsNotExist(err) {
		if explicit {
			return nil, false, fmt.Errorf("merchant manifest %s: %w (explicit startup manifest is required)", path, err)
		}
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read merchant manifest %s: %w", path, err)
	}

	overlays, err := bootstrap.ReadMerchantManifestOverlays(cfg.MerchantManifestOverlays)
	if err != nil {
		return nil, false, err
	}
	manifest, err := bootstrap.LoadMerchantConfigManifestWithOverlays(raw, overlays...)
	if err != nil {
		return nil, false, fmt.Errorf("merchant manifest %s: %w", path, err)
	}
	rt := application.Runtime
	if rt == nil {
		return nil, false, fmt.Errorf("merchant startup requires a runtime")
	}
	opts := bootstrap.MerchantManifestReconcileOptions{StripeClients: rt.StripeClients, Insert: true}
	if cfg.SecretStoreBackend() == config.SecretBackendSnapshot {
		if rt.ManifestSecrets == nil {
			return nil, false, fmt.Errorf("snapshot credentials require the runtime snapshot plane")
		}
		opts.SecretStore = rt.ManifestSecrets.Seeder()
	}
	var signers []*signeridentity.Transit
	if backend := rt.MerchantSecretBackend; backend != nil && backend.SolanaTransit != nil && rt.Merchants != nil {
		opts.SolanaTransit = backend.SolanaTransit
		environment := config.ExpectedProviderEnvironment(cfg.IsTestMode())
		opts.WrapTransit = func(slug string, transit solanaint.TransitClient) solanaint.TransitClient {
			signer := &signeridentity.Transit{TransitClient: transit, DB: rt.DB, Directory: rt.Merchants, Slug: slug,
				Environment: environment, Tolerate: tolerateVault, OnChange: rt.ReportSignerKeyChange}
			signers = append(signers, signer)
			return signer
		}
		opts.DeferPSP = (&signeridentity.Transit{Tolerate: tolerateVault}).Defers
	}
	if err := bootstrap.ReconcileMerchantManifestData(ctx, cfg, embcp.Get(application), manifest, opts); err != nil {
		return nil, false, fmt.Errorf("merchant manifest %s: %w", path, err)
	}
	log.WithField("file", path).Info("merchant startup initialized; existing metadata preserved")
	slugs := make([]string, 0, len(manifest.Merchants))
	for slug := range manifest.Merchants {
		slugs = append(slugs, slug)
	}
	pending := false
	for _, signer := range signers {
		pending = pending || signer.Unavailable()
	}
	return slugs, pending, nil
}
