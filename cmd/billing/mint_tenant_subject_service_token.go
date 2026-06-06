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
		return fmt.Errorf("control plane is not enabled")
	}

	operatorOrg, err := cmd.Flags().GetString("org")
	if err != nil {
		return fmt.Errorf("read org flag: %w", err)
	}
	operatorOrg = strings.ToLower(strings.TrimSpace(operatorOrg))
	if operatorOrg == "" && cfg != nil && cfg.Auth != nil {
		operatorOrg = strings.ToLower(strings.TrimSpace(cfg.Auth.OperatorTenantSlug))
	}
	if operatorOrg == "" {
		operatorOrg = "operator"
	}
	if _, err := embcp.RunBootstrap(ctx, embeddedApp.App(), controlplane.BootstrapOptions{
		OperatorTenantSlug:      operatorOrg,
		MintInitialServiceToken: false,
	}); err != nil {
		return fmt.Errorf("bootstrap operator tenant/role: %w", err)
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
	serviceToken, token, err := cp.Core().MintServiceTokenWithOptions(ctx, operatorOrg, authcore.ServiceTokenMintOptions{
		Name:        name,
		Permissions: permissions,
		Resources:   resources,
	})
	if err != nil {
		return fmt.Errorf("mint tenant subject serviceToken: %w", err)
	}

	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"org":                  operatorOrg,
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
