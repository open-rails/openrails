package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/core"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/pkg/embedded"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
	"github.com/open-rails/openrails/pkg/tenant"
)

func mintTenantSubjectServiceToken(cmd *cobra.Command, _ []string) error {
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

	bootstrapTenant, err := cmd.Flags().GetString("org")
	if err != nil {
		return fmt.Errorf("read org flag: %w", err)
	}
	bootstrapTenant = strings.ToLower(strings.TrimSpace(bootstrapTenant))
	if bootstrapTenant == "" {
		// HARDCUT (#312): service tokens are minted under the default tenant's own
		// AuthKit org — there is no separate "operator" tenant.
		bootstrapTenant = tenant.DefaultSlug
	}
	if _, err := embcp.RunBootstrap(ctx, embeddedApp.App(), controlplane.BootstrapOptions{
		BootstrapTenantSlug:     bootstrapTenant,
		MintInitialServiceToken: false,
	}); err != nil {
		return fmt.Errorf("bootstrap authority/role: %w", err)
	}

	tenantRef, err := cmd.Flags().GetString("tenant")
	if err != nil {
		return fmt.Errorf("read tenant flag: %w", err)
	}
	tenantID, tenantSlug, tenantResource, err := cp.TenantScope(ctx, tenantRef)
	if err != nil {
		return fmt.Errorf("resolve tenant %q: %w", tenantRef, err)
	}

	tenantSubjectRef, err := cmd.Flags().GetString("tenant-subject")
	if err != nil {
		return fmt.Errorf("read tenant-subject flag: %w", err)
	}
	tenantSubjectID, err := uuid.Parse(strings.TrimSpace(tenantSubjectRef))
	if err != nil || tenantSubjectID == uuid.Nil {
		return fmt.Errorf("--tenant-subject must be a tenant subject UUID")
	}

	name, err := cmd.Flags().GetString("name")
	if err != nil {
		return fmt.Errorf("read name flag: %w", err)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "openrails-tenant-subject-" + tenantSubjectID.String()
	}

	permissions, err := cmd.Flags().GetStringSlice("permission")
	if err != nil {
		return fmt.Errorf("read permission flags: %w", err)
	}
	for i := range permissions {
		permissions[i] = strings.TrimSpace(permissions[i])
	}
	if len(permissions) == 0 {
		permissions = []string{controlplane.PermCreditsSpend}
	}

	resources := []authcore.ServiceTokenResource{
		tenantResource,
		controlplane.TenantSubjectResource(tenantSubjectID),
	}
	serviceToken, token, err := cp.Core().MintServiceTokenWithOptions(ctx, bootstrapTenant, authcore.ServiceTokenMintOptions{
		Name:        name,
		Permissions: permissions,
		Resources:   resources,
	})
	if err != nil {
		return fmt.Errorf("mint tenant subject service token: %w", err)
	}

	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"org":                  bootstrapTenant,
		"name":                 name,
		"tenant":               tenantSlug,
		"tenant_id":            tenantID.String(),
		"tenant_subject_id":    tenantSubjectID.String(),
		"service_token_key_id": serviceToken.KeyID,
		"service_token_secret": token,
		"permissions":          serviceToken.Permissions,
		"resources":            serviceToken.Resources,
	})
}
