// Package serverboot is the standalone-server composition root (#285/#670):
// NewServer wires the framework-neutral HTTP Server onto the application graph
// built by app.BootstrapWithOptions, attaching the mandatory control plane.
package serverboot

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	internalauth "github.com/open-rails/openrails/internal/auth"
	"github.com/open-rails/openrails/internal/bootstrap"
	server "github.com/open-rails/openrails/internal/http"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/cache"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
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

	ConfiguredMerchant merchant.ID
	Rails              config.RailMerchantAccountSet
}

// NewServer constructs the application runtime and the HTTP server graph
// together.
func NewServer(cfg *config.Config, opts *Options) (*Result, error) {
	// #711: the bootstrap.Options relay layer is gone — call the app
	// composition root directly.
	application, err := app.BootstrapWithOptions(cfg, &app.BootstrapOptions{
		PGXPool:            optsValue(opts, func(o *Options) *pgxpool.Pool { return o.PGXPool }),
		Redis:              optsValue(opts, func(o *Options) *redis.Client { return o.Redis }),
		Cache:              optsValue(opts, func(o *Options) cache.Cache { return o.Cache }),
		Clock:              optsValue(opts, func(o *Options) clockwork.Clock { return o.Clock }),
		ConfiguredMerchant: optsValue(opts, func(o *Options) merchant.ID { return o.ConfiguredMerchant }),
		Rails:              optsValue(opts, func(o *Options) config.RailMerchantAccountSet { return o.Rails }),
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
	if cperr := embcp.Attach(context.Background(), application, cfg, injectedPool); cperr != nil {
		return nil, fmt.Errorf("attach control plane: %w", cperr)
	}
	authenticator := optsValue(opts, func(o *Options) billingauth.Authenticator { return o.Authenticator })
	if authenticator == nil {
		cp := embcp.Get(application)
		if cp == nil || cp.AuthService() == nil || cp.AuthService().Verifier() == nil {
			return nil, fmt.Errorf("control plane verifier unavailable")
		}
		authenticator = internalauth.NewAuthenticator(cp.AuthService().Verifier())
	}

	// MODE 1 (#723): the standalone server loads the merchant manifest at boot —
	// DB rows converge as projections, secrets seed the in-memory plane. A
	// declared-but-unloadable manifest refuses boot; an absent conventional file
	// boots control-plane-only (merchants can be bound later; there is no
	// implied truth to miss). MODE 2 refuses a present manifest (two truths).
	if err := reconcileBootMerchantManifest(context.Background(), cfg, application,
		optsValue(opts, func(o *Options) string { return o.MerchantManifestPath })); err != nil {
		return nil, err
	}

	billingServer, err := server.New(server.Dependencies{
		Config:                 application.Config,
		Cache:                  application.Cache,
		Runtime:                application.Runtime,
		Redis:                  application.RedisClient,
		Authenticator:          authenticator,
		DelegatedAuthenticator: optsValue(opts, func(o *Options) billingauth.DelegatedAuthenticator { return o.DelegatedAuthenticator }),
		ConfiguredMerchant:     application.Runtime.ConfiguredMerchant,
		ControlPlane:           embcp.Get(application),
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

// reconcileBootMerchantManifest implements the standalone rows of the #723
// boot matrix. path empty → the conventional manifest location, which is
// optional; an EXPLICIT path must exist.
func reconcileBootMerchantManifest(ctx context.Context, cfg *config.Config, application *app.App, path string) error {
	explicit := strings.TrimSpace(path) != ""
	if !explicit {
		path = bootstrap.DefaultMerchantConfigManifestPath
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if explicit {
			return fmt.Errorf("merchant manifest %s: %w (merchant_source=manifest declared this file as truth, #723)", path, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read merchant manifest %s: %w", path, err)
	}
	if !cfg.IsManifestMerchantSource() {
		return fmt.Errorf("merchant_source=api refuses the merchant manifest at %s: merchant truth lives in the API/store, not a boot file (two truths, #723); delete the file or run merchant_source=manifest", path)
	}
	manifest, err := bootstrap.LoadMerchantConfigManifestBytes(raw)
	if err != nil {
		return fmt.Errorf("merchant manifest %s: %w", path, err)
	}
	rt := application.Runtime
	if rt == nil || rt.ManifestSecrets == nil {
		return fmt.Errorf("merchant_source=manifest requires the runtime manifest secret plane (#723)")
	}
	if err := bootstrap.ReconcileMerchantManifestData(ctx, cfg, embcp.Get(application), manifest, bootstrap.MerchantManifestReconcileOptions{
		Insert:      true,
		Overwrite:   true,
		Prune:       true,
		SecretStore: rt.ManifestSecrets.Seeder(),
	}); err != nil {
		return fmt.Errorf("merchant manifest %s: %w", path, err)
	}
	log.WithField("file", path).Info("merchant_source=manifest: boot manifest converged; credentials held in memory (#723)")
	return nil
}
