package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/pkg/embedded"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

func mintOperatorOAT(cmd *cobra.Command, _ []string) error {
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

	org, err := cmd.Flags().GetString("org")
	if err != nil {
		return fmt.Errorf("read org flag: %w", err)
	}
	org = strings.ToLower(strings.TrimSpace(org))
	if org == "" && cfg != nil && cfg.Auth != nil {
		org = strings.ToLower(strings.TrimSpace(cfg.Auth.OperatorOrgSlug))
	}
	if org == "" {
		org = "operator"
	}

	name, err := cmd.Flags().GetString("name")
	if err != nil {
		return fmt.Errorf("read name flag: %w", err)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "openrails-operator-manual"
	}

	oat, token, err := cp.Core().MintOrgAccessToken(ctx, org, name, controlplane.OperatorRolePermissions(), "", nil)
	if err != nil {
		return fmt.Errorf("mint operator oat: %w", err)
	}

	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"org":         org,
		"name":        name,
		"oat_key_id":  oat.KeyID,
		"oat_secret":  token,
		"permissions": oat.Permissions,
	})
}
