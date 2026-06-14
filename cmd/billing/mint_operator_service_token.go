package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	authcore "github.com/open-rails/authkit/core"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/pkg/embedded"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

func mintOperatorServiceToken(cmd *cobra.Command, _ []string) error {
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
	cp := embcp.Get(embeddedApp.App())
	if cp == nil || cp.Core() == nil {
		return fmt.Errorf("control plane unavailable (set auth.control_plane.issuer)")
	}

	authorityTenantSlug, err := cmd.Flags().GetString("org")
	if err != nil {
		return fmt.Errorf("read org flag: %w", err)
	}
	authorityTenantSlug = strings.ToLower(strings.TrimSpace(authorityTenantSlug))
	if authorityTenantSlug == "" {
		// No default tenant (#336): the authority tenant must be named explicitly.
		return fmt.Errorf("--org is required (the AuthKit tenant slug hosting the admin authority)")
	}

	if _, berr := embcp.RunBootstrap(ctx, embeddedApp.App(), controlplane.BootstrapOptions{
		BootstrapTenantSlug:     authorityTenantSlug,
		MintInitialServiceToken: false,
	}); berr != nil {
		return fmt.Errorf("bootstrap authority/role: %w", berr)
	}

	name, err := cmd.Flags().GetString("name")
	if err != nil {
		return fmt.Errorf("read name flag: %w", err)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "openrails-operator-manual"
	}

	permissions, err := cmd.Flags().GetStringSlice("permission")
	if err != nil {
		return fmt.Errorf("read permission flags: %w", err)
	}
	for i := range permissions {
		permissions[i] = strings.TrimSpace(permissions[i])
	}
	if len(permissions) == 0 {
		permissions = controlplane.OperatorRolePermissions()
	}

	tenantRef, err := cmd.Flags().GetString("tenant")
	if err != nil {
		return fmt.Errorf("read tenant flag: %w", err)
	}
	tenantID, tenantSlug, tenantResource, err := cp.MerchantScope(ctx, tenantRef)
	if err != nil {
		return fmt.Errorf("resolve tenant %q: %w", tenantRef, err)
	}

	resources := []authcore.ServiceTokenResource{tenantResource}
	serviceToken, token, err := cp.Core().MintServiceTokenWithOptions(ctx, authorityTenantSlug, authcore.ServiceTokenMintOptions{
		Name:        name,
		Permissions: permissions,
		Resources:   resources,
	})
	if err != nil {
		return fmt.Errorf("mint operator service token: %w", err)
	}

	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"org":                  authorityTenantSlug,
		"name":                 name,
		"tenant":               tenantSlug,
		"tenant_id":            tenantID.String(),
		"service_token_key_id": serviceToken.KeyID,
		"service_token_secret": token,
		"permissions":          serviceToken.Permissions,
		"resources":            serviceToken.Resources,
	})
}
