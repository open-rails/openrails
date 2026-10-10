package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	boot "github.com/open-rails/openrails/internal/merchantbootstrap"

	"net/http"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/inprocess"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/server/internal/hostconfig"
	"github.com/spf13/cobra"
)

// Both modes use the public Client. Remote mode never loads local infrastructure.
func newMerchantConfigurationCmd(apply bool) *cobra.Command {
	var serverURL, tokenFile, slug, file string
	name := "get-merchant-config"
	if apply {
		name = "apply-merchant-config"
	}
	cmd := &cobra.Command{
		Use:   name,
		Short: "Read or update merchant configuration through the Client",
		Args:  cobra.NoArgs,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(slug) == "" {
				return fmt.Errorf("--merchant is required")
			}
			if serverURL != "" {
				for _, flag := range []string{"config", "provider-write-mode", "test-mode"} {
					if cmd.Flags().Changed(flag) {
						return fmt.Errorf("--%s is local-only and cannot be used with --server-url", flag)
					}
				}
				if tokenFile == "" {
					return fmt.Errorf("remote mode requires --token-file")
				}
				return nil
			}
			if tokenFile != "" {
				return fmt.Errorf("--token-file requires --server-url")
			}
			path, _ := cmd.Flags().GetString("config")
			cfg, err := hostconfig.LoadDatabase(path)
			if err != nil {
				return err
			}
			cmd.SetContext(context.WithValue(cmd.Context(), config.ConfigContextKey, cfg.Config))
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			var params billing.UpdateMerchantConfigurationParams
			if apply {
				if file == "" {
					return fmt.Errorf("--file is required: the configuration to merge, with the expected_revision it was read at")
				}
				input, err := os.Open(file)
				if err != nil {
					return err
				}
				defer input.Close()
				raw, err := io.ReadAll(io.LimitReader(input, billing.MaxMerchantConfigurationBytes+1))
				if err != nil {
					return err
				}
				parsed, err := billing.ParseMerchantConfigurationYAML(raw)
				if err != nil {
					return err
				}
				params = *parsed
			}
			var client *openrails.Client
			var err error
			if serverURL != "" {
				if _, err := readTokenFile(tokenFile); err != nil {
					return err
				}
				client, err = openrails.NewRemote(serverURL, tokenFileCredential(tokenFile), openrails.WithDefaultMerchant(slug))
			} else {
				cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
				var cleanup func()
				client, cleanup, err = localMerchantConfigurationClient(cmd.Context(), cfg, slug)
				if cleanup != nil {
					defer cleanup()
				}
			}
			if err != nil {
				return err
			}
			var result any
			if apply {
				result, err = client.UpdateMerchantConfiguration(cmd.Context(), params)
			} else {
				result, err = client.GetMerchantConfiguration(cmd.Context())
			}
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		},
	}
	cmd.Flags().StringVar(&slug, "merchant", "", "existing merchant slug (selector, not authority)")
	cmd.Flags().StringVar(&serverURL, "server-url", "", "remote OpenRails base URL; omit for trusted local operator execution")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "file holding an access token from the merchant's trusted issuer (client credentials), read on every call")
	if apply {
		cmd.Flags().StringVarP(&file, "file", "f", "", "YAML or JSON configuration to merge, with an optional expected_revision")
	}
	return cmd
}

// Local administration needs the database and the merchant's configuration
// (Vault, or the manifest at its conventional path), not payment-provider
// clients, workers, or standalone authentication infrastructure.
func localMerchantConfigurationClient(ctx context.Context, cfg *config.Config, slug string) (*openrails.Client, func(), error) {
	database, err := openCLIDB(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { database.Close() }
	directory, err := merchants.NewDirectoryService(database.DataPool())
	if err != nil {
		return nil, cleanup, err
	}
	selected, err := directory.GetBySlug(ctx, slug)
	if err != nil {
		return nil, cleanup, err
	}
	merchantsSvc, closeConfig, err := boot.OneOffMerchants(ctx, cfg, database, selected.ID, nil, "", hostconfig.FromContext(ctx).MerchantManifestOverlays)
	if err != nil {
		return nil, cleanup, err
	}
	cleanup = func() { closeConfig(); database.Close() }
	rt := &app.Runtime{DB: database, Config: cfg, Merchants: merchantsSvc, MerchantConfig: merchantsSvc.Config(), MoneyService: money.NewMoneyService(database), EntitlementService: entitlements.NewEntitlementService(database)}
	table := &router.Table{}
	httproutes.RegisterStaffRoutesUnder(router.NewMux(table, "/v1", rt), rt, httproutes.HostOptions(), "/v1/admin/configuration")
	transport, capability := inprocess.NewTransport(table.Handler(), func() billing.MerchantID { return selected.ID })
	client, err := openrails.NewRemote("http://openrails.invalid", openrails.WithHTTPClient(&http.Client{Transport: transport}), openrails.WithMerchantID(selected.ID), openrails.WithTokenProvider(func(context.Context) (string, error) { return capability, nil }))
	return client, cleanup, err
}
