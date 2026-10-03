package main

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
)

// openEngine builds the standalone engine through openrails.New like any host,
// with OpenRails-managed River and, when controlPlane, the control plane.
func openEngine(ctx context.Context, cfg *config.Config, deps openrails.Deps, controlPlane bool) (*openrails.Client, *app.App, error) {
	c := *cfg
	c.River = config.RiverManaged
	if !controlPlane {
		c.ControlPlane = nil
	}
	client, err := openrails.New(ctx, c, deps)
	if err != nil {
		return nil, nil, fmt.Errorf("bootstrap application: %w", err)
	}
	return client, engine.Graph(client), nil
}
