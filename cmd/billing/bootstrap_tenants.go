package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	tenantbootstrap "github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/pkg/embedded"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

func bootstrapTenants(cmd *cobra.Command, _ []string) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	ctx := context.Background()

	embeddedApp, err := embedded.New(embedded.Options{Config: cfg})
	if err != nil {
		return fmt.Errorf("bootstrap application: %w", err)
	}
	defer func() { _ = embeddedApp.Close(ctx) }()

	if err := embcp.Attach(ctx, embeddedApp.App(), cfg, nil); err != nil {
		return fmt.Errorf("attach control plane: %w", err)
	}
	if err := tenantbootstrap.ReconcileTenantManifest(ctx, cfg, embcp.Get(embeddedApp.App())); err != nil {
		return fmt.Errorf("tenant manifest bootstrap: %w", err)
	}
	return nil
}
