package engine

import (
	"context"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/routes"
)

func (e *Engine) ResolveCustomer(ctx context.Context, authority auth.Authenticator, verified auth.Verified) (config.CustomerBinding, error) {
	if err := e.CheckOpen(); err != nil {
		return config.CustomerBinding{}, err
	}
	binding, err := routes.ResolveExistingCustomer(ctx, e.App.Runtime, authority, verified)
	if err != nil {
		return config.CustomerBinding{}, err
	}
	if err := e.CheckOpen(); err != nil {
		return config.CustomerBinding{}, err
	}
	return binding, nil
}
