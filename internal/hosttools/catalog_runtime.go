package hosttools

import (
	"context"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchant"
	boot "github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/merchants"
	catalogmodule "github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/service"
	log "github.com/sirupsen/logrus"
)

func catalogRuntime(ctx context.Context, opts CatalogApplyOptions) (*app.Runtime, *service.Service, func(), error) {
	if opts.App != nil {
		if opts.App.Runtime == nil {
			return nil, nil, nil, fmt.Errorf("catalog runtime is not initialized")
		}
		svc, err := service.New(opts.App.Runtime)
		if err != nil {
			return nil, nil, nil, err
		}
		return opts.App.Runtime, svc, func() {}, nil
	}
	return newCatalogRuntime(ctx, opts)
}

func newCatalogRuntime(ctx context.Context, opts CatalogApplyOptions) (*app.Runtime, *service.Service, func(), error) {
	cfg := opts.Config
	if cfg == nil {
		return nil, nil, nil, fmt.Errorf("catalog runtime configuration is required")
	}
	ctx, cancel := context.WithCancel(ctx)
	database, err := openEmbeddedDB(ctx, cfg, opts.PGXPool)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	rt := &app.Runtime{DB: database, Config: cfg, ProductService: catalogmodule.NewProductService(database), PriceService: catalogmodule.NewPriceService(database), MoneyService: money.NewMoneyService(database), EntitlementService: entitlements.NewEntitlementService(database)}
	rt.StripeClients = stripeapi.NewFactory(nil)
	if api := config.SandboxStripeAPIURL(cfg); api != "" {
		rt.StripeClients = stripeapi.NewFactory(stripeapi.HostRewriteTransport(api))
	}
	cleanup := func() {
		cancel()
		if err := rt.Close(context.Background()); err != nil {
			log.WithError(err).Error("catalog runtime cleanup failed")
		}
	}
	fail := func(err error) (*app.Runtime, *service.Service, func(), error) { cleanup(); return nil, nil, nil, err }
	directory, err := merchants.NewDirectoryService(database.DataPool())
	if err != nil {
		return fail(err)
	}
	selected, err := directory.GetBySlug(ctx, opts.Merchant)
	if err != nil {
		return fail(fmt.Errorf("resolve catalog merchant %q: %w", opts.Merchant, err))
	}
	opened, err := boot.OneOffMerchants(ctx, cfg, database, selected.ID, opts.MerchantManifest, opts.MerchantManifestPath, nil)
	if err != nil {
		return fail(fmt.Errorf("catalog runtime merchant configuration: %w", err))
	}
	previous := cleanup
	cleanup = func() { opened.Close(); previous() }
	rt.Merchants, rt.MerchantConfig = opened.Service, opened.Service.Config()
	rt.Merchants.StripeClients = rt.StripeClients
	rt.RailConfigs = railresolve.NewMerchantsSource(cfg, func() *merchants.Service { return rt.Merchants })
	rt.SolanaRPCResolver = &solanamodule.MerchantRPCBuilder{Config: cfg, MerchantsFn: func() *merchants.Service { return rt.Merchants }, Endpoint: config.SandboxSolanaRPCURL(cfg)}
	// Catalog application publishes recurring plans with the deployment's
	// first cranker only when catalog provider writes are allowed; otherwise
	// it can resolve the crankers but never sign. It never builds a
	// subscription lifecycle, poller or workers.
	crankers, err := recurring.BuildCrankers(config.SolanaCrankers(cfg), opened.SolanaTransit, app.CrankerPins{DB: database})
	if err != nil {
		return fail(err)
	}
	var submitter recurring.Submitter = recurring.NewSignerSubmitter(crankers, rt.SolanaRPCResolver.Resolve)
	if config.IsLimitedMode(cfg) {
		submitter = catalogPlanAddressReader{submitter}
	}
	rt.SolanaPlanService = recurring.NewPlanServiceWithReader(submitter, rt.SolanaRPCResolver.ChainReader(), rt.SolanaRPCResolver.RecurringTokens, rt.SolanaRPCResolver.Treasury)
	if err := rt.WireOneOffDestructiveAdmissions(ctx); err != nil {
		return fail(err)
	}
	svc, err := service.New(rt)
	if err != nil {
		return fail(err)
	}
	return rt, svc, cleanup, nil
}

// catalogPlanAddressReader resolves the crankers and never signs: the catalog
// runtime's submitter while provider writes are limited.
type catalogPlanAddressReader struct{ recurring.Submitter }

func (catalogPlanAddressReader) Submit(context.Context, billing.MerchantID, solanago.PublicKey, []solanago.Instruction) (solanago.Signature, error) {
	return solanago.Signature{}, solana.ErrProviderReadOnly
}

func catalogMerchantContext(ctx context.Context, directory *merchants.Service, name string) (context.Context, string, error) {
	if directory == nil {
		return nil, "", fmt.Errorf("merchant directory is required")
	}
	selected, err := directory.GetBySlug(ctx, name)
	if err != nil {
		return nil, "", fmt.Errorf("resolve catalog merchant %q: %w", name, err)
	}
	return merchant.WithID(ctx, selected.ID), selected.Slug, nil
}
