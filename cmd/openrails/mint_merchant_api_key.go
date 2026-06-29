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
		BootstrapMerchantSlug:  authorityOrgSlug,
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

	// #567: the merchant IS a permission-group with a FIXED role catalog
	// (owner/support/viewer; AllowCustomRoles=false). A key is minted under the
	// merchant group against one of those catalog roles; its effective permissions
	// are resolved from the role at verify time. --permission no longer applies
	// (no custom roles); --role selects a catalog role, defaulting to `owner`.
	roleFlag, err := cmd.Flags().GetString("role")
	if err != nil {
		return fmt.Errorf("read role flag: %w", err)
	}
	roleFlag = strings.TrimSpace(roleFlag)

	if perms, perr := cmd.Flags().GetStringSlice("permission"); perr == nil && len(perms) > 0 {
		return fmt.Errorf("--permission is no longer supported (#567: merchant groups have fixed catalog roles); use --role owner|support|viewer")
	}

	merchantRef, err := cmd.Flags().GetString("merchant")
	if err != nil {
		return fmt.Errorf("read merchant flag: %w", err)
	}
	merchantID, merchantSlug, err := cp.MerchantScope(ctx, merchantRef)
	if err != nil {
		return fmt.Errorf("resolve merchant %q: %w", merchantRef, err)
	}

	role := roleFlag
	if role == "" {
		role = controlplane.MerchantRoleOwner
	}

	// Mint under the merchant permission-group (type=merchant, ref=merchant slug).
	// #569 (hard cut): identity is the group; no resource scope.
	apiKey, token, err := cp.Core().MintAPIKeyWithOptions(ctx, controlplane.MerchantType, merchantSlug, authkit.APIKeyMintOptions{
		Name: name,
		Role: role,
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
		"role":        role,
		"permissions": apiKey.Permissions,
	})
}
