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

func mintCustomerServiceToken(cmd *cobra.Command, _ []string) error {
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

	bootstrapOrg, err := cmd.Flags().GetString("org")
	if err != nil {
		return fmt.Errorf("read org flag: %w", err)
	}
	bootstrapOrg = strings.ToLower(strings.TrimSpace(bootstrapOrg))
	if bootstrapOrg == "" {
		// No default merchant (#336): the authority org must be named explicitly.
		return fmt.Errorf("--org is required (the AuthKit org slug hosting the admin authority)")
	}
	if _, err := embcp.RunBootstrap(ctx, embeddedApp.App(), controlplane.BootstrapOptions{
		BootstrapOrgSlug:        bootstrapOrg,
		MintInitialServiceToken: false,
	}); err != nil {
		return fmt.Errorf("bootstrap authority/role: %w", err)
	}

	merchantRef, err := cmd.Flags().GetString("merchant")
	if err != nil {
		return fmt.Errorf("read merchant flag: %w", err)
	}
	merchantID, merchantSlug, merchantResource, err := cp.MerchantScope(ctx, merchantRef)
	if err != nil {
		return fmt.Errorf("resolve merchant %q: %w", merchantRef, err)
	}

	customerRef, err := cmd.Flags().GetString("customer")
	if err != nil {
		return fmt.Errorf("read customer flag: %w", err)
	}
	customerID, err := uuid.Parse(strings.TrimSpace(customerRef))
	if err != nil || customerID == uuid.Nil {
		return fmt.Errorf("--customer must be a customer UUID")
	}

	name, err := cmd.Flags().GetString("name")
	if err != nil {
		return fmt.Errorf("read name flag: %w", err)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "openrails-customer-" + customerID.String()
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
		merchantResource,
		controlplane.CustomerResource(customerID),
	}
	serviceToken, token, err := cp.Core().MintServiceTokenWithOptions(ctx, bootstrapOrg, authcore.ServiceTokenMintOptions{
		Name:        name,
		Permissions: permissions,
		Resources:   resources,
	})
	if err != nil {
		return fmt.Errorf("mint customer service token: %w", err)
	}

	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"org":                  bootstrapOrg,
		"name":                 name,
		"merchant":             merchantSlug,
		"merchant_id":          merchantID.String(),
		"customer_id":          customerID.String(),
		"service_token_key_id": serviceToken.KeyID,
		"service_token_secret": token,
		"permissions":          serviceToken.Permissions,
		"resources":            serviceToken.Resources,
	})
}
