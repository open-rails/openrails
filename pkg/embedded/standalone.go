package embedded

import (
	"context"
	"fmt"
	"net/http"

	server "github.com/open-rails/openrails/internal/http"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

// StandaloneHandler returns the full standalone HTTP surface for the embedded
// app: health + user + self/customer + merchant + webhooks + API-key service
// routes, served on the framework-neutral net/http stack (#670). It is intended
// for the standalone server entrypoint (cmd/openrails).
//
// This is the standalone surface, so it wires the OpenRails-owned control plane
// (#284/#469) and uses that control plane's AuthKit verifier. There is no
// config-declared verifier-only mode.
func StandaloneHandler(e *Embedded) (http.Handler, error) {
	srv, err := StandaloneServer(e)
	if err != nil {
		return nil, err
	}
	return srv.Handler(), nil
}

// StandaloneServer constructs the standalone *server.Server graph from the
// embedded app (StandaloneHandler minus the Handler() projection; the route
// table is reachable via Server.RouteTable for surface tests).
func StandaloneServer(e *Embedded) (*server.Server, error) {
	if e == nil {
		return nil, fmt.Errorf("embedded billing: not initialized")
	}
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
	authenticator := embcp.Get(a).UserAuthenticator()
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
