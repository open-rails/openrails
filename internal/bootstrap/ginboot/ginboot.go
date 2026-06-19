// Package ginboot holds the gin-coupled composition root (#285): NewServer wires
// the gin HTTP Server onto the gin-free application graph built by
// bootstrap.NewApp. Keeping it out of internal/bootstrap is what lets the
// gin-free composition root (and pkg/embedded through it) avoid importing gin.
package ginboot

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	internalauth "github.com/open-rails/openrails/internal/auth"
	"github.com/open-rails/openrails/internal/bootstrap"
	server "github.com/open-rails/openrails/internal/http"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

// Result holds the application graph plus the gin HTTP server created by the
// composition root.
type Result struct {
	App    *app.App
	Server *server.Server
}

// NewServer constructs the application runtime and the gin HTTP server graph
// together. It is the gin-coupled counterpart of bootstrap.NewApp.
func NewServer(cfg *config.Config, opts *bootstrap.Options) (*Result, error) {
	application, err := bootstrap.NewApp(cfg, opts)
	if err != nil {
		return nil, err
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
	if application.Authenticator == nil {
		cp := embcp.Get(application)
		if cp == nil || cp.AuthService() == nil || cp.AuthService().Verifier() == nil {
			return nil, fmt.Errorf("control plane verifier unavailable")
		}
		application.Authenticator = internalauth.NewAuthenticator(cp.AuthService().Verifier())
	}

	billingServer, err := server.New(server.Dependencies{
		Config:                 application.Config,
		Cache:                  application.Cache,
		Runtime:                application.Runtime,
		Redis:                  application.RedisClient,
		Authenticator:          application.Authenticator,
		DelegatedAuthenticator: application.DelegatedAuthenticator,
		ConfiguredMerchant:     application.Runtime.ConfiguredMerchant,
		ControlPlane:           embcp.Get(application),
	})
	if err != nil {
		return nil, fmt.Errorf("create billing server: %w", err)
	}

	cleanupOnError = false
	return &Result{App: application, Server: billingServer}, nil
}
