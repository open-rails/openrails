package hosttools

import (
	"context"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	catalogmodule "github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/merchant"
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
	directory.WithNameAuthority(opts.NameAuthority)
	selected, err := directory.GetBySlug(ctx, opts.Merchant)
	if err != nil {
		return fail(fmt.Errorf("resolve catalog merchant %q: %w", opts.Merchant, err))
	}
	rt.SetConfiguredMerchant(selected.ID)
	if cfg.IsManifestMerchantConfigSource() {
		rt.Merchants, err = pullProviderManifestPlane(ctx, cfg, database, PullProviderOptions{
			MerchantID: selected.ID, NameAuthority: opts.NameAuthority, MerchantManifestPath: opts.MerchantManifestPath,
		})
		if err != nil {
			return fail(fmt.Errorf("catalog runtime credential plane: %w", err))
		}
		if rt.Merchants == nil {
			// No conventional snapshot is legal for a local-only catalog. It
			// supplies no credential fallback: any required provider secret
			// remains absent and reference validation must fail closed.
			rt.ManifestSecrets = merchants.NewManifestSecretStore()
			rt.Merchants, err = merchants.NewService(database.DataPool(), rt.ManifestSecrets, config.ExpectedProviderEnvironment(cfg.IsTestMode()))
			if err != nil {
				return fail(err)
			}
		}
	} else {
		if err := rt.EnsureMerchantsService(ctx); err != nil {
			return fail(fmt.Errorf("catalog runtime credential plane unavailable: %w", err))
		}
	}
	rt.Merchants.WithNameAuthority(opts.NameAuthority)
	rt.RailConfigs = railresolve.NewMerchantsSource(cfg, func() *merchants.Service { return rt.Merchants })
	// Catalog application verifies existing plans. This graph never constructs
	// a signer, signing submitter, subscription lifecycle, poller, or worker fleet.
	readConfig := *cfg
	readConfig.ProviderWriteMode = config.ProviderWriteModeReadOnly
	rt.SolanaRPCResolver = &solanamodule.MerchantRPCBuilder{Config: &readConfig, MerchantsFn: func() *merchants.Service { return rt.Merchants }}
	network := "mainnet"
	if cfg.IsTestMode() {
		network = "devnet"
	}
	rt.SolanaPlanService = recurring.NewPlanServiceWithReader(catalogPlanAddressReader{merchants: rt.Merchants, environment: config.ExpectedProviderEnvironment(cfg.IsTestMode())}, rt.SolanaRPCResolver.ChainReader(), network, solanatokens.ForNetwork(network))
	svc, err := service.New(rt)
	if err != nil {
		return fail(err)
	}
	return rt, svc, cleanup, nil
}

// PlanService also serves publishing workflows, so its constructor accepts a
// Submitter. This catalog-only adapter can resolve public account identity but
// deliberately cannot sign or submit instructions, irrespective of write mode.
type catalogPlanAddressReader struct {
	merchants   *merchants.Service
	environment string
}

func (r catalogPlanAddressReader) MerchantAddress(ctx context.Context, id merchant.ID) (solanago.PublicKey, error) {
	bound, err := merchant.Require(ctx)
	if err != nil || bound != id {
		return solanago.PublicKey{}, fmt.Errorf("Solana catalog merchant scope mismatch")
	}
	scope, ok, err := r.merchants.ActivePSPScope(ctx, id, "solana", r.environment)
	if err != nil {
		return solanago.PublicKey{}, err
	}
	if !ok {
		return solanago.PublicKey{}, fmt.Errorf("Solana catalog reader has no active account for merchant %s", id)
	}
	key, err := solanago.PublicKeyFromBase58(scope.AccountID)
	if err != nil || key.IsZero() {
		return solanago.PublicKey{}, fmt.Errorf("Solana catalog account has an invalid public address")
	}
	return key, nil
}
func (catalogPlanAddressReader) Submit(context.Context, merchant.ID, []solanago.Instruction) (solanago.Signature, error) {
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
