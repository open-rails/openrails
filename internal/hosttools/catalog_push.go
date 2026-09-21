package hosttools

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/jackc/pgx/v5/pgxpool"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/catalogpublish"
	"github.com/open-rails/openrails/internal/merchants"
	catalogmodule "github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/railresolve"
	billingservice "github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/catalog"
	"github.com/open-rails/openrails/pkg/merchant"
)

// CatalogPushOptions configures PushMerchantCatalog.
type CatalogPushOptions struct {
	// NameAuthority is required for bound names when constructing an isolated
	// catalog runtime. A supplied Runtime uses its already configured authority.
	NameAuthority merchant.NameAuthority
	Config        *config.Config
	PGXPool       *pgxpool.Pool
	// App applies the catalog through an already-bootstrapped engine graph,
	// preserving its armed per-merchant PSPs and signers; without it the helper
	// builds an isolated runtime from Config. The supplied graph's config is
	// authoritative.
	App      *app.App
	File     string
	Manifest []byte
	Out      io.Writer

	// Insert/Overwrite/Prune are the mutation classes; they compose. Declaring
	// NONE is plan-only. or#893 deleted the DryRun field: it could override an
	// explicitly requested mutation, so the same call meant two different things
	// depending on a second field.
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
	Merchant   string              `yaml:"merchant"`
	TierGroups []catalog.TierGroup `yaml:"tier_groups,omitempty"`
	Products   []catalog.Product   `yaml:"products,omitempty"`
	Meters     []catalog.Meter     `yaml:"meters,omitempty"`
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
	cfg := catalogPushConfig(opts)
	if cfg == nil {
		return fmt.Errorf("config not loaded; in-process mode requires config")
	}
	// API-owned catalogs are DB data —
	// a mutating YAML push is a second truth and refuses (plan-only stays legal
	// as a read-only diff). For manifest catalogs the YAML wins unconditionally —
	// a mutating push always runs the full converge (Insert+Overwrite+Prune),
	// so partial flags cannot leave the DB projection drifted from the file.
	if !opts.planOnly() {
		if !cfg.IsManifestCatalogSource() {
			return fmt.Errorf("catalog_source=api refuses a mutating catalog push: mutate the catalog via the API, or select catalog_source=manifest; plan-only remains available")
		}
		if !opts.Insert || !opts.Overwrite || !opts.Prune {
			log.Info("catalog_source=manifest: catalog push upgraded to full converge (insert+overwrite+prune)")
			opts.Insert, opts.Overwrite, opts.Prune = true, true, true
		}
	}

	manifests := make([]*catalog.Manifest, 0, len(targets))
	for _, target := range targets {
		manifests = append(manifests, target.Manifest)
	}
	rt, svc, cleanup, err := catalogPushRuntime(ctx, opts, manifests...)
	if err != nil {
		return err
	}
	defer cleanup()

	for _, target := range targets {
		catalogCtx, canonicalName, err := contextForCatalogPushTarget(ctx, rt.Merchants, target.Merchant)
		if err != nil {
			return err
		}
		target.Merchant = canonicalName
		if err := rt.DB.RunInMerchantConn(catalogCtx, func(ctx context.Context) error {
			response, err := catalogpublish.Publish(ctx, svc, openrails.CatalogPublishRequest{
				Catalog: *target.Manifest, Insert: opts.Insert, Overwrite: opts.Overwrite, Prune: opts.Prune,
			})
			if err != nil {
				return fmt.Errorf("publish %s: %w", target.Merchant, err)
			}
			if len(targets) > 1 {
				fmt.Fprintf(out, "\n%s plan:\n", target.Merchant)
			}
			catalogpublish.PrintPlan(response.Plan, out, opts.planOnly())
			if !response.Plan.HasChanges() {
				fmt.Fprintln(out, "\nno changes — catalog is up to date")
			}
			if response.Result != nil {
				fmt.Fprintln(out)
				catalogpublish.PrintResult(response.Result, out)
			}
			return reportCatalogExtras(ctx, svc, out, opts.planOnly(), opts.Prune)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (o CatalogPushOptions) planOnly() bool {
	return !o.Insert && !o.Overwrite && !o.Prune
}

func catalogPushConfig(opts CatalogPushOptions) *config.Config {
	if opts.App != nil {
		return opts.App.Config
	}
	return opts.Config
}

func catalogPushRuntime(
	ctx context.Context,
	opts CatalogPushOptions,
	manifests ...*catalog.Manifest,
) (*app.Runtime, *billingservice.Service, func(), error) {
	if opts.App != nil {
		if opts.App.Runtime == nil {
			return nil, nil, nil, fmt.Errorf("catalog runtime is not initialized")
		}
		svc, err := billingservice.New(opts.App.Runtime)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("construct catalog service from embedded runtime: %w", err)
		}
		return opts.App.Runtime, svc, func() {}, nil
	}
	rt, svc, cleanup, err := newCatalogPushRuntime(
		ctx,
		catalogPushConfig(opts),
		opts.PGXPool,
		manifests...,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	if opts.NameAuthority != nil {
		rt.Merchants.WithNameAuthority(opts.NameAuthority)
	}
	return rt, svc, cleanup, nil
}

func loadCatalogPushTargets(opts CatalogPushOptions) ([]catalogPushTarget, error) {
	raw := opts.Manifest
	if len(raw) == 0 {
		path := strings.TrimSpace(opts.File)
		if path == "" {
			return nil, fmt.Errorf("catalog manifest file or manifest bytes are required")
		}
		var err error
		raw, err = os.ReadFile(path) // #nosec G304 -- path is opts.File, an operator CLI flag, not request input
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
			Version:    catalog.SupportedVersion,
			TierGroups: append([]catalog.TierGroup(nil), entry.TierGroups...),
			Products:   append([]catalog.Product(nil), entry.Products...),
			Meters:     append([]catalog.Meter(nil), entry.Meters...),
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

func newCatalogPushRuntime(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, manifests ...*catalog.Manifest) (*app.Runtime, *billingservice.Service, func(), error) {
	database, err := openEmbeddedDB(ctx, cfg, pool)
	if err != nil {
		return nil, nil, nil, err
	}
	rt := &app.Runtime{
		DB:                 database,
		Config:             cfg,
		ProductService:     catalogmodule.NewProductService(database),
		PriceService:       catalogmodule.NewPriceService(database),
		MoneyService:       money.NewMoneyService(database),
		EntitlementService: entitlements.NewEntitlementService(database),
	}
	// #788: catalog-push provider verification resolves the armed rail state
	// through the ONE Layer-C seam; arming rides EnsureMerchantsService below
	// (a host with no armed rails degrades to manual provider links).
	rt.RailConfigs = railresolve.NewMerchantsSource(cfg, func() *merchants.Service { return rt.Merchants })
	if err := rt.EnsureMerchantsService(context.Background()); err != nil {
		log.WithError(err).Warn("catalog push: merchants service arming failed; provider links degrade to manual")
	}
	if rt.Merchants == nil {
		// Catalog identity does not depend on provider credentials. Unbound
		// host merchants can still resolve local names; bound groups continue
		// to refuse canonical-name projection without their AuthKit authority.
		rt.Merchants, err = merchants.NewDirectoryService(database.DataPool())
		if err != nil {
			return nil, nil, nil, err
		}
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

func contextForCatalogPushTarget(ctx context.Context, directory *merchants.Service, name string) (context.Context, string, error) {
	if directory == nil {
		return nil, "", fmt.Errorf("merchant directory is required")
	}
	selected, err := directory.GetBySlug(ctx, name)
	if err != nil {
		return nil, "", fmt.Errorf("resolve catalog merchant %q: %w", name, err)
	}
	return merchant.WithID(ctx, selected.ID), selected.Slug, nil
}
