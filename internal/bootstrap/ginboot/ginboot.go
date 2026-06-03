// Package ginboot holds the gin-coupled composition root (#285): NewServer wires
// the gin HTTP Server onto the gin-free application graph built by
// bootstrap.NewApp. Keeping it out of internal/bootstrap is what lets the
// gin-free composition root (and pkg/embedded through it) avoid importing gin.
package ginboot

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/bootstrap"
	server "github.com/open-rails/openrails/internal/http"
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

	billingServer, err := server.New(server.Dependencies{
		Config:        application.Config,
		Cache:         application.Cache,
		Runtime:       application.Runtime,
		Redis:         application.RedisClient,
		Authenticator: application.Authenticator,
		ControlPlane:  application.ControlPlane,
	})
	if err != nil {
		return nil, fmt.Errorf("create billing server: %w", err)
	}

	cleanupOnError = false
	return &Result{App: application, Server: billingServer}, nil
}
