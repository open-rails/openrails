package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/goccy/go-yaml"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	catalogmodule "github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/catalog"
	"github.com/open-rails/openrails/pkg/merchant"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

const defaultCatalogManifestPath = "/etc/openrails/catalog.yaml"

// catalogOptions holds the flags for `openrails push-merchant-catalog`.
type catalogOptions struct {
	file      string
	dryRun    bool
	insert    bool
	overwrite bool
	prune     bool
}

type catalogApplyTarget struct {
	Merchant string
	Name     string
	Manifest *catalog.Manifest
}

type catalogFile struct {
	Version  int                       `yaml:"version"`
	Catalogs []catalogFileCatalogEntry `yaml:"catalogs"`
}

type catalogFileCatalogEntry struct {
	Merchant         string              `yaml:"merchant"`
	Name             string              `yaml:"name,omitempty"`
	DefaultProviders []string            `yaml:"default_providers,omitempty"`
	TierGroups       []catalog.TierGroup `yaml:"tier_groups"`
}

// newPushCatalogCmd builds the `openrails push-merchant-catalog` command — a terraform-style
// declarative apply of a YAML catalog manifest
// (issue #162). It mirrors cozy-art's sync-product-catalog pipeline:
// load -> validate -> plan -> print -> (dry-run? stop : apply).
//
// The command runs in-process through a catalog-sized runtime: Postgres plus the
// catalog/provider facades, without starting the server runtime.
func newPushCatalogCmd() *cobra.Command {
	opts := catalogOptions{file: defaultCatalogManifestPath}
	cmd := &cobra.Command{
		Use:     "push-merchant-catalog",
		Aliases: []string{"push-catalog"},
		Short:   "Push a YAML merchant catalog manifest into OpenRails and configured providers",
		Long: "Loads a declarative catalog manifest (catalogs[] > tier_groups > products > prices), " +
			"computes a terraform-style plan per merchant, and prints it. A bare command is plan-only. " +
			"Mutation classes are explicit and compose: --insert creates missing products/prices/provider objects; " +
			"--overwrite updates existing OpenRails-owned catalog rows; --prune archives OpenRails-owned extras. " +
			"Products are identified by slug within their merchant; prices by financial substance (currency, amount, interval).",
		Args: validateCatalogArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPushCatalog(cmd, opts)
		},
	}
	flags := cmd.Flags()
	flags.StringVarP(&opts.file, "file", "f", defaultCatalogManifestPath, "catalog manifest YAML file")
	flags.BoolVar(&opts.dryRun, "dry-run", false, "deprecated alias for the default plan-only behavior")
	_ = flags.MarkHidden("dry-run")
	flags.BoolVar(&opts.insert, "insert", false, "Create missing OpenRails/provider catalog objects from the manifest")
	flags.BoolVar(&opts.overwrite, "overwrite", false, "Update existing OpenRails-owned catalog objects from the manifest")
	flags.BoolVar(&opts.prune, "prune", false, "archive OpenRails-owned provider objects absent from the local catalog; foreign provider objects are never touched")
	return cmd
}

func validateCatalogArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return err
	}
	file, err := cmd.Flags().GetString("file")
	if err != nil {
		return err
	}
	file = strings.TrimSpace(file)
	if file == "" {
		file = defaultCatalogManifestPath
	}
	if _, err := os.Stat(file); err != nil {
		return fmt.Errorf("read catalog manifest: %w", err)
	}
	return nil
}

func runPushCatalog(cmd *cobra.Command, opts catalogOptions) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	if strings.TrimSpace(opts.file) == "" {
		opts.file = defaultCatalogManifestPath
	}

	targets, err := loadCatalogTargets(opts.file)
	if err != nil {
		return err
	}

	cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	if cfg == nil {
		return fmt.Errorf("config not loaded; in-process mode requires --config")
	}
	manifests := make([]*catalog.Manifest, 0, len(targets))
	for _, target := range targets {
		manifests = append(manifests, target.Manifest)
	}
	applier, svc, rt, cleanup, err := buildApplier(ctx, cmd, manifests...)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	for _, target := range targets {
		label := strings.TrimSpace(target.Name)
		if label == "" {
			label = target.Merchant
		}
		catalogCtx, err := contextForCatalogTarget(ctx, rt.DB, target.Merchant)
		if err != nil {
			return err
		}
		if err := rt.DB.RunInMerchantConn(catalogCtx, func(ctx context.Context) error {
			plan, err := catalog.Plan(ctx, applier, target.Manifest)
			if err != nil {
				return fmt.Errorf("plan %s: %w", label, err)
			}
			if len(targets) > 1 {
				fmt.Fprintf(out, "\n%s plan:\n", label)
			}
			planOnly := opts.planOnly()
			plan.Print(out, planOnly)

			if planOnly {
				if !plan.HasChanges() {
					fmt.Fprintln(out, "\nno changes — catalog is up to date")
				}
				return reportCatalogExtras(ctx, svc, out, true, opts.prune)
			}
			if !plan.HasChanges() {
				fmt.Fprintln(out, "\nno changes — catalog is up to date")
				return reportCatalogExtras(ctx, svc, out, false, opts.prune)
			}

			result, err := catalog.ApplyWithOptions(ctx, applier, plan, catalog.ApplyOptions{
				Insert:    opts.insert,
				Overwrite: opts.overwrite,
				Prune:     opts.prune,
			})
			if err != nil {
				return fmt.Errorf("apply %s: %w", label, err)
			}
			fmt.Fprintln(out)
			result.Print(out)
			return reportCatalogExtras(ctx, svc, out, false, opts.prune)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (o catalogOptions) planOnly() bool {
	return o.dryRun || (!o.insert && !o.overwrite && !o.prune)
}

func loadCatalogTargets(path string) ([]catalogApplyTarget, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read catalog manifest: %w", err)
	}
	var file catalogFile
	if err := yaml.UnmarshalWithOptions(raw, &file, yaml.DisallowUnknownField()); err != nil {
		return nil, fmt.Errorf("parse catalog manifest: %w", err)
	}
	if file.Version != catalog.SupportedVersion {
		return nil, fmt.Errorf("unsupported catalog manifest version %d (want %d)", file.Version, catalog.SupportedVersion)
	}
	if len(file.Catalogs) == 0 {
		return nil, fmt.Errorf("catalog manifest must define at least one catalogs[] entry")
	}
	targets := make([]catalogApplyTarget, 0, len(file.Catalogs))
	for i, entry := range file.Catalogs {
		merchantSlug := strings.ToLower(strings.TrimSpace(entry.Merchant))
		if merchantSlug == "" {
			return nil, fmt.Errorf("catalog #%d merchant is required", i+1)
		}
		manifest := &catalog.Manifest{
			Version:          catalog.SupportedVersion,
			DefaultProviders: append([]string(nil), entry.DefaultProviders...),
			TierGroups:       append([]catalog.TierGroup(nil), entry.TierGroups...),
		}
		if err := manifest.Validate(); err != nil {
			label := strings.TrimSpace(entry.Name)
			if label == "" {
				label = merchantSlug
			}
			return nil, fmt.Errorf("catalog %s: %w", label, err)
		}
		targets = append(targets, catalogApplyTarget{
			Merchant: merchantSlug,
			Name:     strings.TrimSpace(entry.Name),
			Manifest: manifest,
		})
	}
	return targets, nil
}

// buildApplier returns an in-process Applier plus a cleanup func that closes the
// minimal catalog runtime. It deliberately does not call app.Bootstrap: catalog
// apply needs Postgres plus catalog/provider facades, not Redis, ClickHouse,
// health checks, workers, or HTTP/server startup.
func buildApplier(_ context.Context, cmd *cobra.Command, manifests ...*catalog.Manifest) (catalog.Applier, *billingservice.Service, *app.Runtime, func(), error) {
	cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	if cfg == nil {
		return nil, nil, nil, nil, fmt.Errorf("config not loaded; in-process mode requires --config")
	}
	rt, err := newCatalogRuntime(cfg, manifests...)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	cleanup := func() {
		if closeErr := rt.Close(context.Background()); closeErr != nil {
			log.WithError(closeErr).Error("catalog runtime cleanup failed")
		}
	}
	svc, err := billingservice.New(rt)
	if err != nil {
		cleanup()
		return nil, nil, nil, nil, fmt.Errorf("construct OpenRails service: %w", err)
	}
	return catalog.NewServiceApplier(svc), svc, rt, cleanup, nil
}

func reportCatalogExtras(ctx context.Context, svc *billingservice.Service, out io.Writer, dryRun bool, prune bool) error {
	report, err := svc.DetectCatalogExtras(ctx)
	if err != nil {
		if prune {
			return fmt.Errorf("catalog extras detection: %w", err)
		}
		log.WithError(err).Warn("push-merchant-catalog: catalog extras detection failed")
		fmt.Fprintf(out, "\ncatalog extras: detection failed (apply unaffected): %v\n", err)
		return nil
	}
	fmt.Fprintf(out, "\ncatalog extras: %d provider-side object(s) not in the local catalog (scanned %d stripe products, %d stripe prices, %d nmi plans, %d solana plans)\n",
		len(report.Extras),
		report.ScannedStripeProducts,
		report.ScannedStripePrices,
		report.ScannedNMIPlans,
		report.ScannedSolanaPlans,
	)
	for _, extra := range report.Extras {
		marker := "foreign — never touched"
		if extra.Owned {
			marker = "openrails-marked"
		}
		active := "inactive"
		if extra.Active {
			active = "active"
		}
		fmt.Fprintf(out, "  - %s %s %s label=%q marker=%s state=%s\n", extra.Provider, extra.ObjectType, extra.ExternalID, extra.Label, marker, active)
		log.WithFields(log.Fields{
			"provider":    extra.Provider,
			"object_type": extra.ObjectType,
			"external_id": extra.ExternalID,
			"owned":       extra.Owned,
			"active":      extra.Active,
			"marker_key":  extra.MarkerKey,
		}).Warn("push-merchant-catalog: provider-side catalog extra not present in local manifest")
	}
	for _, note := range report.Notes {
		fmt.Fprintf(out, "  note[%s]: %s\n", note.Provider, note.Note)
	}
	if !prune || len(report.Extras) == 0 {
		return nil
	}
	if dryRun {
		fmt.Fprintf(out, "\nprune (dry-run): would archive OpenRails-owned active extras and would never touch foreign extras\n")
		return nil
	}
	outcomes, archiveErr := svc.ArchiveCatalogExtras(ctx, report.Extras)
	fmt.Fprintf(out, "\nprune: archive outcomes\n")
	for _, outcome := range outcomes {
		line := fmt.Sprintf("  - %s %s %s: %s", outcome.Extra.Provider, outcome.Extra.ObjectType, outcome.Extra.ExternalID, outcome.Action)
		if outcome.Detail != "" {
			line += " (" + outcome.Detail + ")"
		}
		if outcome.IntentID != "" {
			line += " intent=" + outcome.IntentID
		}
		fmt.Fprintln(out, line)
		log.WithFields(log.Fields{
			"provider":    outcome.Extra.Provider,
			"object_type": outcome.Extra.ObjectType,
			"external_id": outcome.Extra.ExternalID,
			"action":      outcome.Action,
			"intent_id":   outcome.IntentID,
		}).Info("push-merchant-catalog --prune: catalog extra archive outcome")
	}
	if archiveErr != nil {
		return fmt.Errorf("archive catalog extras: %w", archiveErr)
	}
	return nil
}

func contextForCatalogTarget(ctx context.Context, database *db.DB, merchantSlug string) (context.Context, error) {
	tid, err := resolveCLIMerchant(ctx, database, merchantSlug)
	if err != nil {
		if strings.HasPrefix(err.Error(), "--merchant is required") {
			return nil, fmt.Errorf("catalog merchant is required")
		}
		return nil, err
	}
	return merchant.WithID(ctx, tid), nil
}

func newCatalogRuntime(cfg *config.Config, manifests ...*catalog.Manifest) (*app.Runtime, error) {
	database, err := db.NewDB(cfg.DB)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	usesMobius := false
	for _, manifest := range manifests {
		if catalogManifestUsesProvider(manifest, "mobius") {
			usesMobius = true
			break
		}
	}
	return &app.Runtime{
		DB:                 database,
		Config:             cfg,
		NMIClients:         catalogNMIClients(cfg, config.ProcessorSet{}, usesMobius),
		ProductService:     catalogmodule.NewProductService(database),
		PriceService:       catalogmodule.NewPriceService(database),
		MoneyService:       money.NewMoneyService(database),
		EntitlementService: entitlements.NewEntitlementService(database),
	}, nil
}

func catalogNMIClients(cfg *config.Config, processors config.ProcessorSet, enabled bool) map[string]*nmi.NMIClient {
	clients := make(map[string]*nmi.NMIClient)
	if cfg == nil || !enabled {
		return clients
	}
	for name, procConfig := range processors.GetNMIProcessors() {
		providerKey := strings.TrimSpace(strings.ToLower(name))
		if providerKey == "" || procConfig == nil || strings.TrimSpace(procConfig.SecurityKey) == "" {
			continue
		}
		client, err := nmi.NewClient(providerKey, procConfig.ToNMIProviderSettings(providerKey), cfg.IsTestEnv())
		if err != nil {
			log.WithError(err).WithField("provider", providerKey).Warn("NMI catalog client unavailable")
			continue
		}
		client.ReadOnly = cfg.IsProviderReadOnly()
		clients[providerKey] = client
	}
	return clients
}

func catalogManifestUsesProvider(manifest *catalog.Manifest, provider string) bool {
	if manifest == nil {
		return false
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return false
	}
	hasProvider := func(providers []string) bool {
		for _, p := range providers {
			if strings.EqualFold(strings.TrimSpace(p), provider) {
				return true
			}
		}
		return false
	}
	hasLink := func(links map[string]map[string]string) bool {
		for p := range links {
			if strings.EqualFold(strings.TrimSpace(p), provider) {
				return true
			}
		}
		return false
	}
	if hasProvider(manifest.DefaultProviders) {
		return true
	}
	for _, group := range manifest.TierGroups {
		for _, product := range group.Products {
			if hasProvider(product.Providers) {
				return true
			}
			for _, price := range product.Prices {
				if hasProvider(price.Providers) || hasLink(price.ProviderLinks) {
					return true
				}
			}
		}
	}
	return false
}
