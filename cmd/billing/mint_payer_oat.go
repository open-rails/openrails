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

func mintTenantSubjectOAT(cmd *cobra.Command, _ []string) error {
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
		operatorOrg = strings.ToLower(strings.TrimSpace(cfg.Auth.OperatorOrgSlug))
	}
	if operatorOrg == "" {
		operatorOrg = "operator"
	}
	if _, err := embcp.RunBootstrap(ctx, embeddedApp.App(), controlplane.BootstrapOptions{
		OperatorOrgSlug: operatorOrg,
		MintInitialOAT:  false,
	}); err != nil {
		return fmt.Errorf("bootstrap operator org/role: %w", err)
	}

	tenantRef, err := cmd.Flags().GetString("tenant")
	if err != nil {
		return fmt.Errorf("read tenant flag: %w", err)
	}
	tenantID, tenantSlug, tenantResource, err := cp.TenantScope(ctx, tenantRef)
	if err != nil {
		return fmt.Errorf("resolve tenant %q: %w", tenantRef, err)
	}

	payerSlug, err := cmd.Flags().GetString("payer")
	if err != nil {
		return fmt.Errorf("read payer flag: %w", err)
	}
	payerSlug = strings.ToLower(strings.TrimSpace(payerSlug))
	if payerSlug == "" {
		return fmt.Errorf("--payer is required")
	}
	payerOrg, err := cp.Core().ResolveOrgBySlug(ctx, payerSlug)
	if err != nil {
		return fmt.Errorf("resolve payer org %q: %w", payerSlug, err)
	}

	name, err := cmd.Flags().GetString("name")
	if err != nil {
		return fmt.Errorf("read name flag: %w", err)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "openrails-payer-" + payerSlug
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

	payerID, err := uuid.Parse(payerOrg.ID)
	if err != nil {
		return fmt.Errorf("payer org %q has invalid id %q: %w", payerSlug, payerOrg.ID, err)
	}
	resources := []authcore.OrgAccessTokenResource{
		tenantResource,
		controlplane.TenantSubjectResource(payerID),
	}
	oat, token, err := cp.Core().MintOrgAccessTokenWithOptions(ctx, operatorOrg, authcore.OrgAccessTokenMintOptions{
		Name:        name,
		Permissions: permissions,
		Resources:   resources,
	})
	if err != nil {
		return fmt.Errorf("mint payer oat: %w", err)
	}

	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"org":               operatorOrg,
		"name":              name,
		"tenant":            tenantSlug,
		"tenant_id":         tenantID.String(),
		"payer":             payerSlug,
		"tenant_subject_id": payerOrg.ID,
		"oat_key_id":        oat.KeyID,
		"oat_secret":        token,
		"permissions":       oat.Permissions,
		"resources":         oat.Resources,
	})
}
