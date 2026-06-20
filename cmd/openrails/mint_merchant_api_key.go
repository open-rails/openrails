package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
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

	// AuthKit v0.43.0 mints API keys against an org ROLE, not a raw permission
	// bundle: the key's effective permissions are resolved from the role at verify
	// time. The CLI supports two UX paths:
	//   --role <slug>     mint against an existing org role as-is.
	//   --permission ...  ensure an org role carrying exactly these permissions
	//                     (defaulting to the full merchant API set) and mint it.
	// The two are mutually exclusive.
	roleFlag, err := cmd.Flags().GetString("role")
	if err != nil {
		return fmt.Errorf("read role flag: %w", err)
	}
	roleFlag = strings.TrimSpace(roleFlag)

	permissions, err := cmd.Flags().GetStringSlice("permission")
	if err != nil {
		return fmt.Errorf("read permission flags: %w", err)
	}
	for i := range permissions {
		permissions[i] = strings.TrimSpace(permissions[i])
	}

	if roleFlag != "" && len(permissions) > 0 {
		return fmt.Errorf("--role and --permission are mutually exclusive")
	}

	merchantRef, err := cmd.Flags().GetString("merchant")
	if err != nil {
		return fmt.Errorf("read merchant flag: %w", err)
	}
	merchantID, merchantSlug, merchantResource, err := cp.MerchantScope(ctx, merchantRef)
	if err != nil {
		return fmt.Errorf("resolve merchant %q: %w", merchantRef, err)
	}

	// Resolve the role to mint against. With --role, use it verbatim (it must
	// already exist in the org). Otherwise ensure a role carrying the requested
	// (or default full) permission set and mint against that.
	role := roleFlag
	if role == "" {
		if len(permissions) == 0 {
			permissions = controlplane.OperatorRolePermissions()
			role = controlplane.OperatorRole
		} else {
			role = permissionRoleSlug(permissions)
		}
		if _, err := controlplane.EnsureRole(ctx, cp.Core(), authorityOrgSlug, role, permissions); err != nil {
			return fmt.Errorf("ensure role %q: %w", role, err)
		}
	}

	resources := []authcore.APIKeyResource{merchantResource}
	apiKey, token, err := cp.Core().MintAPIKeyWithOptions(ctx, authorityOrgSlug, authcore.APIKeyMintOptions{
		Name:      name,
		Role:      role,
		Resources: resources,
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
		"resources":   apiKey.Resources,
	})
}

// permissionRoleSlug derives a deterministic, valid org-role slug for a bespoke
// permission set so re-running the CLI with the same --permission set reuses
// (and re-asserts) the same role rather than accumulating roles. The slug is
// stable under permission ordering.
func permissionRoleSlug(perms []string) string {
	sorted := append([]string(nil), perms...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return "openrails-key-" + hex.EncodeToString(sum[:8])
}
