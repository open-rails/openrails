// Package gin holds the gin-coupled surface of pkg/embedded (#285). The core
// pkg/embedded package is gin-free and exposes only the net/http NewHTTPHandler;
// hosts that mount billing onto an existing gin engine use RegisterAPI (the
// AuthKit-style front door, #623) or MountHandler (the net/http escape hatch),
// and the standalone entrypoint uses StandaloneHandler.
//
// Everything in this package is built from the gin-free *app.App that the core
// embedded.Embedded carries (accessed via Embedded.App()), so the gin dependency
// stays isolated to this subpackage.
package gin

import (
	"context"
	"fmt"
	"net/http"

	internalauth "github.com/open-rails/openrails/internal/auth"
	server "github.com/open-rails/openrails/internal/http"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

// StandaloneHandler returns the full standalone gin HTTP surface for the embedded app:
// health + debug (dev only) + user + merchant + webhooks + API-key
// service routes. It is built from the gin-free app graph and is intended for
// the standalone server entrypoint (cmd/openrails).
func StandaloneHandler(e *embedded.Embedded) (http.Handler, error) {
	srv, err := newServer(e)
	if err != nil {
		return nil, err
	}
	return srv.Handler(), nil
}

// Handler returns the full standalone gin HTTP surface.
//
// Deprecated: use StandaloneHandler. Embedded hosts should usually use
// RegisterAPI / MountHandler, or pkg/embedded.NewHTTPHandler.
func Handler(e *embedded.Embedded) (http.Handler, error) {
	return StandaloneHandler(e)
}

// newServer constructs the gin billing server graph from the embedded app.
//
// This is the standalone surface, so it wires the OpenRails-owned control plane
// (#284/#469) and uses that control plane's AuthKit verifier when no host
// authenticator was injected. There is no config-declared verifier-only mode.
func newServer(e *embedded.Embedded) (*server.Server, error) {
	a := e.App()
	if a == nil {
		return nil, fmt.Errorf("embedded billing: not initialized")
	}
	cfg := a.Config
	if embcp.Get(a) == nil {
		if err := embcp.Attach(context.Background(), a, cfg, nil); err != nil {
			return nil, fmt.Errorf("attach control plane: %w", err)
		}
	}
	authenticator := func() billingauth.Authenticator {
		cp := embcp.Get(a)
		if cp == nil || cp.AuthService() == nil || cp.AuthService().Verifier() == nil {
			return nil
		}
		return internalauth.NewAuthenticator(cp.AuthService().Verifier())
	}()
	if authenticator == nil {
		return nil, fmt.Errorf("control plane verifier unavailable")
	}
	return server.New(server.Dependencies{
		Config:             a.Config,
		Cache:              a.Cache,
		Runtime:            a.Runtime,
		Redis:              a.RedisClient,
		Authenticator:      authenticator,
		ConfiguredMerchant: a.Runtime.ConfiguredMerchant,
		ControlPlane:       embcp.Get(a),
	})
}
