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
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

func mintMerchantAPIKey(cmd *cobra.Command, _ []string) error {
	authorityOrgSlug, err := cmd.Flags().GetString("org")
	if err != nil {
		return fmt.Errorf("read org flag: %w", err)
	}
	authorityOrgSlug = strings.ToLower(strings.TrimSpace(authorityOrgSlug))
	if authorityOrgSlug == "" {
		// No default merchant (#336): the authority org must be named explicitly.
		return fmt.Errorf("--org is required (the AuthKit org slug hosting the admin authority)")
	}

	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	ctx := context.Background()
	application := &app.App{Config: cfg}
	defer func() { _ = application.Close(ctx) }()

	if err := embcp.Attach(ctx, application, cfg, nil); err != nil {
		return fmt.Errorf("attach control plane: %w", err)
	}
	cp := embcp.Get(application)
	if cp == nil || cp.Core() == nil {
		return fmt.Errorf("control plane unavailable (set auth.issuer)")
	}

	if _, berr := embcp.RunBootstrap(ctx, application, controlplane.BootstrapOptions{
		BootstrapOrgSlug:  authorityOrgSlug,
		MintInitialAPIKey: false,
	}); berr != nil {
		return fmt.Errorf("bootstrap authority/role: %w", berr)
	}

	name, err := cmd.Flags().GetString("name")
	if err != nil {
		return fmt.Errorf("read name flag: %w", err)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "openrails-merchant-api-key"
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

	merchantRef, err := cmd.Flags().GetString("merchant")
	if err != nil {
		return fmt.Errorf("read merchant flag: %w", err)
	}
	merchantID, merchantSlug, merchantResource, err := cp.MerchantScope(ctx, merchantRef)
	if err != nil {
		return fmt.Errorf("resolve merchant %q: %w", merchantRef, err)
	}

	resources := []authcore.APIKeyResource{merchantResource}
	apiKey, token, err := cp.Core().MintAPIKeyWithOptions(ctx, authorityOrgSlug, authcore.APIKeyMintOptions{
		Name:        name,
		Permissions: permissions,
		Resources:   resources,
	})
	if err != nil {
		return fmt.Errorf("mint merchant API key: %w", err)
	}

	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"org":         authorityOrgSlug,
		"name":        name,
		"merchant":    merchantSlug,
		"merchant_id": merchantID.String(),
		"api_key_id":  apiKey.KeyID,
		"api_key":     token,
		"permissions": apiKey.Permissions,
		"resources":   apiKey.Resources,
	})
}
