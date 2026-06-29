package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-rails/authkit"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

func mintMerchantAPIKey(cmd *cobra.Command, _ []string) error {
	merchantRef, err := cmd.Flags().GetString("merchant")
	if err != nil {
		return fmt.Errorf("read merchant flag: %w", err)
	}
	merchantRef = strings.TrimSpace(merchantRef)
	if merchantRef == "" {
		return fmt.Errorf("--merchant is required")
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

	name, err := cmd.Flags().GetString("name")
	if err != nil {
		return fmt.Errorf("read name flag: %w", err)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "openrails-merchant-api-key"
	}

	roleFlag, err := cmd.Flags().GetString("role")
	if err != nil {
		return fmt.Errorf("read role flag: %w", err)
	}
	roleFlag = strings.TrimSpace(roleFlag)

	merchantID, merchantSlug, err := cp.MerchantScope(ctx, merchantRef)
	if err != nil {
		return fmt.Errorf("resolve merchant %q: %w", merchantRef, err)
	}

	if _, err := cp.Bootstrap(ctx, controlplane.BootstrapOptions{
		BootstrapMerchantSlug: merchantSlug,
		MintInitialAPIKey:     false,
	}); err != nil {
		return fmt.Errorf("ensure merchant permission-group: %w", err)
	}

	role := roleFlag
	if role == "" {
		role = controlplane.MerchantRoleOwner
	}
	createdBy, err := cp.EnsureMerchantAPIKeyActor(ctx, merchantSlug)
	if err != nil {
		return fmt.Errorf("ensure API-key actor: %w", err)
	}

	apiKey, token, err := cp.Core().MintAPIKeyWithOptions(ctx, controlplane.MerchantType, merchantSlug, authkit.APIKeyMintOptions{
		Name:      name,
		Role:      role,
		CreatedBy: createdBy,
	})
	if err != nil {
		return fmt.Errorf("mint merchant API key: %w", err)
	}

	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"name":        name,
		"merchant":    merchantSlug,
		"merchant_id": merchantID.String(),
		"api_key_id":  apiKey.KeyID,
		"api_key":     token,
		"role":        role,
		"permissions": apiKey.Permissions,
	})
}
