package operator

import (
	"fmt"

	"github.com/open-rails/openrails/internal/app"
	server "github.com/open-rails/openrails/internal/http"
)

// StandaloneServer builds the full standalone surface (billing routes, the
// attached control plane's AuthKit routes and the admin console) over the
// graph. The control plane must already be attached.
func StandaloneServer(a *app.App) (*server.Server, error) {
	cp := Get(a)
	if cp == nil {
		return nil, fmt.Errorf("standalone surface: no control plane attached (call Attach first)")
	}
	authenticator := cp.UserAuthenticator()
	if authenticator == nil {
		return nil, fmt.Errorf("control plane verifier unavailable")
	}
	return server.New(server.Dependencies{
		Config:        a.Config,
		Cache:         a.Cache,
		Runtime:       a.Runtime,
		Redis:         a.RedisClient,
		Authenticator: authenticator,
		ControlPlane:  cp,
		ConsoleAssets: a.ConsoleAssets,
	})
}
