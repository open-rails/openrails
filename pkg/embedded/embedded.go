package embedded

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/cache"
	"github.com/open-rails/openrails/pkg/service"
)

type Options struct {
	Config *config.Config
	// PGXPool is the host-supplied database handle (pgx/v5). The bun-era
	// *sql.DB option was removed with the ORM (#334).
	PGXPool *pgxpool.Pool
	Redis   *redis.Client
	// Authenticator is the framework-neutral auth boundary (gin-free). A host
	// brings its own auth by implementing billingauth.Authenticator. When nil, the
	// default AuthKit-backed authenticator is built from Config. Hosts that have a
	// gin Provider can adapt it with ginauth.AsAuthenticator (#285).
	Authenticator billingauth.Authenticator
	// DelegatedAuthenticator is the OPTIONAL host-pluggable identity seam for
	// the browser-direct self-service surface (/v1/me/* + /v1/admin/*,
	// issue #339). A host that verifies platform credentials itself implements
	// billingauth.DelegatedAuthenticator and returns the EXPLICITLY mapped
	// {tenant, subject, permissions} principal; the standalone/gin handler then
	// mounts the self surface authenticated by it, even without a control
	// plane. When nil, the surface authenticates via the control plane's
	// delegated-token verifier (the default). See
	// billingauth.DelegatedAuthenticatorFunc for a closure example.
	DelegatedAuthenticator billingauth.DelegatedAuthenticator
	Cache                  cache.Cache
	// PaymentProviders lets embedding hosts supply one or more payment-provider
	// credential sets programmatically. Local provider names are optional
	// selectors; durable provider-account identity is resolved from the provider
	// itself.
	PaymentProviders []PaymentProvider
}

type Embedded struct {
	app *app.App
}

func New(opts Options) (*Embedded, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("config is required")
	}
	processors, err := ApplyPaymentProviders(opts.PaymentProviders)
	if err != nil {
		return nil, err
	}

	// Build the gin-free application graph only. The standalone gin HTTP surface
	// (Handler / Register*Routes) lives in the pkg/embedded/gin subpackage, which
	// constructs the gin server from this App on demand (#285). Keeping the gin
	// server out of this core type is what makes pkg/embedded gin-free.
	application, err := bootstrap.NewApp(opts.Config, &bootstrap.Options{
		PGXPool:                opts.PGXPool,
		Redis:                  opts.Redis,
		Authenticator:          opts.Authenticator,
		DelegatedAuthenticator: opts.DelegatedAuthenticator,
		Cache:                  opts.Cache,
		Processors:             processors,
	})
	if err != nil {
		return nil, err
	}

	return &Embedded{app: application}, nil
}

// App returns the gin-free application graph. It is the bridge the
// pkg/embedded/gin subpackage uses to build the gin HTTP surface (#285).
func (e *Embedded) App() *app.App {
	if e == nil {
		return nil
	}
	return e.app
}

// HTTPHandlerOptions controls which billing HTTP route groups are included in the returned handler.
//
// If all fields are false (zero value), the options default to user + admin + webhooks.
//
// Note: billing health endpoints are not exposed in embedded mode.
// If a host wants billing readiness, call IsBillingReady and include it in the host's /readyz.
type HTTPHandlerOptions struct {
	IncludeUser     bool
	IncludeAdmin    bool
	IncludeWebhooks bool
}

// NewHTTPHandler returns a single mountable `http.Handler` for the selected route groups.
//
// Embedded routes live under `/billing/v1/*`.
func (e *Embedded) NewHTTPHandler(opts HTTPHandlerOptions) http.Handler {
	if e == nil || e.app == nil {
		return nil
	}
	return embedhttp.FromApp(e.app).NewHTTPHandler(embedhttp.Options{
		IncludeUser:     opts.IncludeUser,
		IncludeAdmin:    opts.IncludeAdmin,
		IncludeWebhooks: opts.IncludeWebhooks,
	})
}

// Service returns the in-process billing API for embedded hosts.
func (e *Embedded) Service() (*service.Service, error) {
	if e == nil || e.app == nil {
		return nil, fmt.Errorf("embedded billing: app not initialized")
	}
	return service.New(e.app.Runtime)
}

// Control-plane bootstrap + accessor moved to the OPT-IN pkg/embedded/controlplane
// helper (#284): the embedded CORE no longer imports internal/controlplane (or,
// through it, AuthKit). Standalone/AuthKit hosts call
// controlplane.RunBootstrap(ctx, e.App(), opts) and controlplane.Get(e.App()).

func (e *Embedded) RunWorkers(ctx context.Context) error {
	if e == nil || e.app == nil || e.app.Runtime == nil {
		return fmt.Errorf("runtime is not initialized")
	}
	return e.app.Runtime.RunWorkers(ctx)
}

func (e *Embedded) Close(ctx context.Context) error {
	if e == nil || e.app == nil {
		return nil
	}
	return e.app.Close(ctx)
}

func (e *Embedded) Config() *config.Config {
	if e == nil || e.app == nil {
		return nil
	}
	return e.app.Config
}
