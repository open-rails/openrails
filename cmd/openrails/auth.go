package main

import (
	"context"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/internal/controlplane"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
	"github.com/open-rails/openrails/pkg/merchant"
)

// newAuthCmd wires the embedded→standalone auth-reconstruction CLI (#544).
//
//	openrails auth recreate [--mint-keys]
//
// After `data import` loads the portable OpenRails billing data into a STANDALONE
// target, the AuthKit (`profiles`) side is empty — it was never exported (the
// dump is billing data only) and embedded never created it (#544). `auth recreate`
// rebuilds the standalone auth scaffolding for every imported merchant: it creates
// each merchant's backing AuthKit org (slug-derived), records the NEW org id on
// `merchants.owner_org_id` (replacing any stale embedded host-org id), and
// optionally mints a fresh deployment admin API key per merchant. It reuses the
// idempotent control-plane Bootstrap path, so re-running is safe.
func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Standalone auth-plane operations (#544)",
	}
	cmd.AddCommand(newAuthRecreateCmd())
	return cmd
}

func newAuthRecreateCmd() *cobra.Command {
	var (
		mintKeys     bool
		manifestPath string
	)
	cmd := &cobra.Command{
		Use:   "recreate",
		Short: "Rebuild standalone AuthKit orgs for every imported merchant (embedded→standalone)",
		RunE: func(c *cobra.Command, _ []string) error {
			return runAuthRecreate(c, mintKeys, manifestPath)
		},
	}
	cmd.Flags().BoolVar(&mintKeys, "mint-keys", false, "Mint a fresh deployment admin API key per merchant without a manifest issuer (printed once)")
	cmd.Flags().StringVar(&manifestPath, "issuers", "", "Merchant-config manifest (YAML) declaring per-merchant issuers; those merchants get their backing org + the issuer registered as remote-application owner (federated embedded→standalone in one step, #547)")
	return cmd
}

func runAuthRecreate(c *cobra.Command, mintKeys bool, manifestPath string) error {
	ctx := c.Context()
	out := c.OutOrStdout()
	cfg, _ := ctx.Value(config.ConfigContextKey).(*config.Config)
	if cfg == nil {
		return fmt.Errorf("config not loaded; auth recreate requires --config")
	}

	application := &app.App{Config: cfg}
	defer func() {
		if err := application.Close(context.Background()); err != nil {
			log.WithError(err).Error("auth recreate cleanup failed")
		}
	}()
	if err := embcp.Attach(ctx, application, cfg, nil); err != nil {
		return fmt.Errorf("attach control plane: %w", err)
	}
	cp := embcp.Get(application)
	if cp == nil {
		return fmt.Errorf("auth recreate: control plane not attached (standalone AuthKit is required)")
	}

	// #547: merchants declared (with an issuer) in the --issuers manifest get their
	// backing org + the issuer registered as the org `owner` remote-application, via
	// the same tested merchant-config reconcile path (provisionMerchantOrg). This is
	// the federated embedded→standalone case in one command.
	withIssuer := map[string]bool{}
	if strings.TrimSpace(manifestPath) != "" {
		manifest, err := bootstrap.LoadMerchantConfigManifest(manifestPath)
		if err != nil {
			return fmt.Errorf("auth recreate: load issuers manifest: %w", err)
		}
		if err := bootstrap.ReconcileMerchantManifestData(ctx, cfg, cp, manifest, bootstrap.MerchantManifestReconcileOptions{Insert: true, Overwrite: true}); err != nil {
			return fmt.Errorf("auth recreate: reconcile issuers manifest: %w", err)
		}
		for _, m := range manifest.Merchants {
			withIssuer[merchant.NormalizeSlug(m.Slug)] = true
		}
		fmt.Fprintf(out, "auth recreate: reconciled %d manifest merchant(s) (backing org + issuer-as-owner)\n", len(manifest.Merchants))
	}

	slugs, err := listMerchantSlugs(ctx, cp)
	if err != nil {
		return err
	}
	if len(slugs) == 0 && len(withIssuer) == 0 {
		fmt.Fprintln(out, "auth recreate: no merchants found; nothing to do")
		return nil
	}

	bootstrapped := 0
	for _, slug := range slugs {
		// Manifest merchants already got org + issuer-as-owner above; don't
		// re-bootstrap them with a user/key owner.
		if withIssuer[merchant.NormalizeSlug(slug)] {
			continue
		}
		res, berr := cp.Bootstrap(ctx, controlplane.BootstrapOptions{
			BootstrapOrgSlug:  slug,
			MintInitialAPIKey: mintKeys,
		})
		if berr != nil {
			return fmt.Errorf("auth recreate: merchant %q: %w", slug, berr)
		}
		bootstrapped++
		fmt.Fprintf(out, "auth recreate: merchant %q -> org %s (created=%t, api_key_minted=%t)\n",
			res.BootstrapOrgSlug, res.BootstrapOrgID, res.OrgCreated, res.APIKeyMinted)
		if res.APIKeyMinted && res.APIKeySecret != "" {
			fmt.Fprintf(out, "  admin API key (shown once): %s\n", res.APIKeySecret)
		}
	}
	fmt.Fprintf(out, "auth recreate: %d merchant(s) via manifest issuer, %d via bootstrap\n", len(withIssuer), bootstrapped)
	return nil
}

// listMerchantSlugs returns every non-deleted merchant slug from the control
// plane's OpenRails schema. The schema name is a validated SQL identifier
// (config.validateSchema), so direct interpolation is injection-free.
func listMerchantSlugs(ctx context.Context, cp *controlplane.ControlPlane) ([]string, error) {
	schema := cp.Pool().Schema()
	rows, err := cp.Pool().Raw().Query(ctx,
		fmt.Sprintf(`SELECT slug FROM %s.merchants WHERE status <> 'deleted' ORDER BY slug`, schema))
	if err != nil {
		return nil, fmt.Errorf("auth recreate: list merchants: %w", err)
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("auth recreate: scan merchant slug: %w", err)
		}
		slugs = append(slugs, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth recreate: iterate merchants: %w", err)
	}
	return slugs, nil
}
