package embedded

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/jackc/pgx/v5/pgxpool"
	log "github.com/sirupsen/logrus"

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

// CatalogPushOptions configures PushMerchantCatalog.
type CatalogPushOptions struct {
	Config   *config.Config
	PGXPool  *pgxpool.Pool
	File     string
	Manifest []byte
	Out      io.Writer

	DryRun    bool
	Insert    bool
	Overwrite bool
	Prune     bool
}

type catalogPushTarget struct {
	Merchant string
	Manifest *catalog.Manifest
}

type catalogPushFile struct {
	Version  int                    `yaml:"version"`
	Catalogs []catalogPushFileEntry `yaml:"catalogs"`
}

type catalogPushFileEntry struct {
	Merchant         string               `yaml:"merchant"`
	TierGroups       []catalog.TierGroup  `yaml:"tier_groups,omitempty"`
	Products         []catalog.Product    `yaml:"products,omitempty"`
	Meters           []catalog.Meter      `yaml:"meters,omitempty"`
	UsageLimits      []catalog.UsageLimit `yaml:"usage_limits,omitempty"`
}

// PushMerchantCatalog plans and optionally applies a merchant catalog manifest.
// A zero mutation option set is plan-only; Insert, Overwrite, and Prune compose.
func PushMerchantCatalog(ctx context.Context, opts CatalogPushOptions) error {
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	targets, err := loadCatalogPushTargets(opts)
	if err != nil {
		return err
	}
	if opts.Config == nil {
		return fmt.Errorf("config not loaded; in-process mode requires config")
	}

	manifests := make([]*catalog.Manifest, 0, len(targets))
	for _, target := range targets {
		manifests = append(manifests, target.Manifest)
	}
	rt, svc, cleanup, err := newCatalogPushRuntime(opts.Config, opts.PGXPool, manifests...)
	if err != nil {
		return err
	}
	defer cleanup()
	applier := catalog.NewServiceApplier(svc)

	for _, target := range targets {
		catalogCtx, err := contextForCatalogPushTarget(ctx, rt.DB, target.Merchant)
		if err != nil {
			return err
		}
		if err := rt.DB.RunInMerchantConn(catalogCtx, func(ctx context.Context) error {
			plan, err := catalog.Plan(ctx, applier, target.Manifest)
			if err != nil {
				return fmt.Errorf("plan %s: %w", target.Merchant, err)
			}
			if len(targets) > 1 {
				fmt.Fprintf(out, "\n%s plan:\n", target.Merchant)
			}
			planOnly := opts.planOnly()
			plan.Print(out, planOnly)

			if planOnly {
				if !plan.HasChanges() {
					fmt.Fprintln(out, "\nno changes — catalog is up to date")
				}
				return reportCatalogExtras(ctx, svc, out, true, opts.Prune)
			}
			if !plan.HasChanges() {
				fmt.Fprintln(out, "\nno changes — catalog is up to date")
				return reportCatalogExtras(ctx, svc, out, false, opts.Prune)
			}

			result, err := catalog.ApplyWithOptions(ctx, applier, plan, catalog.ApplyOptions{
				Insert:    opts.Insert,
				Overwrite: opts.Overwrite,
				Prune:     opts.Prune,
			})
			if err != nil {
				return fmt.Errorf("apply %s: %w", target.Merchant, err)
			}
			fmt.Fprintln(out)
			result.Print(out)
			return reportCatalogExtras(ctx, svc, out, false, opts.Prune)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (o CatalogPushOptions) planOnly() bool {
	return o.DryRun || (!o.Insert && !o.Overwrite && !o.Prune)
}

func loadCatalogPushTargets(opts CatalogPushOptions) ([]catalogPushTarget, error) {
	raw := opts.Manifest
	if len(raw) == 0 {
		path := strings.TrimSpace(opts.File)
		if path == "" {
			return nil, fmt.Errorf("catalog manifest file or manifest bytes are required")
		}
		var err error
		raw, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read catalog manifest: %w", err)
		}
	}

	var file catalogPushFile
	if err := yaml.UnmarshalWithOptions(raw, &file, yaml.DisallowUnknownField()); err != nil {
		return nil, fmt.Errorf("parse catalog manifest: %w", err)
	}
	if file.Version != catalog.SupportedVersion {
		return nil, fmt.Errorf("unsupported catalog manifest version %d (want %d)", file.Version, catalog.SupportedVersion)
	}
	if len(file.Catalogs) == 0 {
		return nil, fmt.Errorf("catalog manifest must define at least one catalogs[] entry")
	}
	targets := make([]catalogPushTarget, 0, len(file.Catalogs))
	for i, entry := range file.Catalogs {
		merchantSlug := strings.ToLower(strings.TrimSpace(entry.Merchant))
		if merchantSlug == "" {
			return nil, fmt.Errorf("catalog #%d merchant is required", i+1)
		}
		manifest := &catalog.Manifest{
			Version:     catalog.SupportedVersion,
			TierGroups:  append([]catalog.TierGroup(nil), entry.TierGroups...),
			Products:    append([]catalog.Product(nil), entry.Products...),
			Meters:      append([]catalog.Meter(nil), entry.Meters...),
			UsageLimits: append([]catalog.UsageLimit(nil), entry.UsageLimits...),
		}
		if err := manifest.Validate(); err != nil {
			return nil, fmt.Errorf("catalog %s: %w", merchantSlug, err)
		}
		targets = append(targets, catalogPushTarget{
			Merchant: merchantSlug,
			Manifest: manifest,
		})
	}
	return targets, nil
}

func newCatalogPushRuntime(cfg *config.Config, pool *pgxpool.Pool, manifests ...*catalog.Manifest) (*app.Runtime, *billingservice.Service, func(), error) {
	database, err := newCatalogPushDB(cfg, pool)
	if err != nil {
		return nil, nil, nil, err
	}
	rt := &app.Runtime{
		DB:                 database,
		Config:             cfg,
		NMIClients:         catalogNMIClients(cfg, config.RailSet{}, catalogPushUsesProvider(manifests, "mobius")),
		ProductService:     catalogmodule.NewProductService(database),
		PriceService:       catalogmodule.NewPriceService(database),
		MoneyService:       money.NewMoneyService(database),
		EntitlementService: entitlements.NewEntitlementService(database),
	}
	cleanup := func() {
		if closeErr := rt.Close(context.Background()); closeErr != nil {
			log.WithError(closeErr).Error("catalog runtime cleanup failed")
		}
	}
	svc, err := billingservice.New(rt)
	if err != nil {
		cleanup()
		return nil, nil, nil, fmt.Errorf("construct OpenRails service: %w", err)
	}
	return rt, svc, cleanup, nil
}

func newCatalogPushDB(cfg *config.Config, pool *pgxpool.Pool) (*db.DB, error) {
	if pool != nil {
		schema := config.DefaultSchema
		if cfg != nil && cfg.DB != nil {
			schema = cfg.DB.SchemaName()
		}
		return db.NewWithPGXPool(pool, schema)
	}
	if cfg == nil || cfg.DB == nil {
		return nil, fmt.Errorf("config database is required")
	}
	database, err := db.NewDB(cfg.DB)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	return database, nil
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
		marker := "foreign - never touched"
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

func contextForCatalogPushTarget(ctx context.Context, database *db.DB, merchantSlug string) (context.Context, error) {
	tid, err := resolveMerchantBySlug(ctx, database, merchantSlug)
	if err != nil {
		return nil, err
	}
	return merchant.WithID(ctx, tid), nil
}

func resolveMerchantBySlug(ctx context.Context, database *db.DB, merchantSlug string) (merchant.ID, error) {
	merchantSlug = strings.TrimSpace(merchantSlug)
	if merchantSlug == "" {
		return merchant.ID{}, fmt.Errorf("catalog merchant is required")
	}
	id, err := db.ResolveMerchantSlug(ctx, database.Pool(), merchantSlug)
	if err != nil {
		return merchant.ID{}, err
	}
	return id, nil
}

func catalogNMIClients(cfg *config.Config, rails config.RailSet, enabled bool) map[string]*nmi.NMIClient {
	clients := make(map[string]*nmi.NMIClient)
	if cfg == nil || !enabled {
		return clients
	}
	for name, procConfig := range rails.GetNMIRails() {
		providerKey := strings.TrimSpace(strings.ToLower(name))
		if providerKey == "" || procConfig == nil || strings.TrimSpace(procConfig.SecurityKey) == "" {
			continue
		}
		client, err := nmi.NewClient(providerKey, procConfig.ToNMIProviderSettings(providerKey), cfg.IsTestMode())
		if err != nil {
			log.WithError(err).WithField("provider", providerKey).Warn("NMI catalog client unavailable")
			continue
		}
		client.ReadOnly = cfg.IsProviderReadOnly()
		clients[providerKey] = client
	}
	return clients
}

func catalogPushUsesProvider(manifests []*catalog.Manifest, provider string) bool {
	for _, manifest := range manifests {
		if catalogManifestUsesProvider(manifest, provider) {
			return true
		}
	}
	return false
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
	for _, group := range manifest.TierGroups {
		for _, product := range group.Products {
			for _, price := range product.Prices {
				if hasProvider(price.Providers) {
					return true
				}
			}
		}
	}
	return false
}
