package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

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
		Short: "Read or explicitly apply merchant metadata through the Client",
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
			var params billing.ApplyMerchantConfigurationParams
			if apply {
				if file == "" {
					return fmt.Errorf("--file is required for an application document with application_id and expected_revision")
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
				token, readErr := os.ReadFile(tokenFile)
				if readErr != nil {
					return readErr
				}
				client, err = openrails.NewRemote(serverURL, openrails.WithAPIKey(strings.TrimSpace(string(token))), openrails.WithDefaultMerchant(slug))
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
				result, err = client.ApplyMerchantConfiguration(cmd.Context(), params)
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
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "file containing the remote API bearer credential")
	if apply {
		cmd.Flags().StringVarP(&file, "file", "f", "", "YAML or JSON application with stable application_id and expected_revision")
	}
	return cmd
}

// Local metadata administration needs database access, not credential custody,
// payment-provider clients, workers, or standalone authentication infrastructure.
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
	rt := &app.Runtime{DB: database, Config: cfg, MoneyService: money.NewMoneyService(database), EntitlementService: entitlements.NewEntitlementService(database)}
	table := &router.Table{}
	httproutes.RegisterStaffRoutesUnder(router.NewMux(table, "/v1", rt), rt, httproutes.HostOptions(), "/v1/admin/configuration")
	transport, capability := inprocess.NewTransport(table.Handler(), func() billing.MerchantID { return selected.ID })
	client, err := openrails.NewRemote("http://openrails.invalid", openrails.WithHTTPClient(&http.Client{Transport: transport}), openrails.WithMerchantID(selected.ID), openrails.WithTokenProvider(func(context.Context) (string, error) { return capability, nil }))
	return client, cleanup, err
}
