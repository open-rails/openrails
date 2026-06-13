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
	"github.com/open-rails/openrails/internal/bootstrap"
	server "github.com/open-rails/openrails/internal/http"
	embauthkit "github.com/open-rails/openrails/pkg/embedded/authkit"
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

	// Standalone wires the AuthKit first-party JWT verifier (#284): the embedded
	// core no longer builds a default verifier, so when no Authenticator was
	// injected we construct the AuthKit-backed one from the config-declared
	// first-party issuers (auth.issuers) here. This is an input to the user/admin
	// JWT surface, not an auth mode (#469).
	if application.Authenticator == nil && cfg.Auth != nil && len(cfg.Auth.Issuers) > 0 {
		authn, aerr := embauthkit.NewVerifierAuthenticatorFromConfig(cfg.Auth)
		if aerr != nil {
			return nil, fmt.Errorf("build authkit verifier: %w", aerr)
		}
		application.Authenticator = authn
	}

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

	billingServer, err := server.New(server.Dependencies{
		Config:                 application.Config,
		Cache:                  application.Cache,
		Runtime:                application.Runtime,
		Redis:                  application.RedisClient,
		Authenticator:          application.Authenticator,
		DelegatedAuthenticator: application.DelegatedAuthenticator,
		ControlPlane:           embcp.Get(application),
	})
	if err != nil {
		return nil, fmt.Errorf("create billing server: %w", err)
	}

	cleanupOnError = false
	return &Result{App: application, Server: billingServer}, nil
}
