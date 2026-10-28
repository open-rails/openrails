package hosttools

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchant"
	boot "github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/reconcile"
)

// providerRuntime is a one-off provider read's world. Rail clients arm per
// merchant from Merchants, so it holds no process-wide rail client.
type providerRuntime struct {
	StripeClients *stripeapi.Factory
	DB            *db.DB
	Config        *config.Config
	Merchants     *merchants.Service
}

// providerRuntimeOptions is what a one-off provider read arms from.
type providerRuntimeOptions struct {
	Config     *config.Config
	PGXPool    *pgxpool.Pool
	MerchantID billing.MerchantID
	// MerchantManifest is the parsed MODE-1 merchant manifest; nil reads
	// MerchantManifestPath, and an empty path tries the conventional one.
	MerchantManifest     *boot.BillingConfig
	MerchantManifestPath string
}

func newProviderRuntime(ctx context.Context, opts providerRuntimeOptions) (*providerRuntime, func(), error) {
	cfg := opts.Config
	if cfg == nil || cfg.DB == nil {
		return nil, nil, fmt.Errorf("config not loaded")
	}
	database, err := openEmbeddedDB(ctx, cfg, opts.PGXPool)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = database.Close() }
	bindParkWakes(ctx, cfg, database)
	if err := database.RequireMerchantID(ctx, opts.MerchantID); err != nil {
		cleanup()
		return nil, nil, err
	}
	// Rails arm as the server's pulls do: a declared account with a missing
	// credential is a rail not armed, loudly.
	opened, err := boot.OneOffMerchants(ctx, cfg, database, opts.MerchantID, opts.MerchantManifest, opts.MerchantManifestPath, nil)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("merchant configuration unavailable, so no rail can be armed: %w", err)
	}
	cleanup = func() { opened.Close(); _ = database.Close() }
	stripeClients := stripeapi.NewFactory(nil)
	if api := config.SandboxStripeAPIURL(cfg); api != "" {
		stripeClients = stripeapi.NewFactory(stripeapi.HostRewriteTransport(api))
	}
	if opened.Service != nil {
		opened.Service.StripeClients = stripeClients
	}
	return &providerRuntime{
		StripeClients: stripeClients,
		DB:            database,
		Config:        cfg,
		Merchants:     opened.Service,
	}, cleanup, nil
}

// resolvePSPTarget reads a --psp value: a PSP id (psp_… or a bare UUID),
// which also names an archived or superseded PSP, or the key of a current one.
// Must run merchant-scoped.
func resolvePSPTarget(ctx context.Context, database *db.DB, raw string) (reconcile.Provider, reconcile.PSPBinding, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", reconcile.PSPBinding{}, err
	}
	raw = strings.TrimSpace(raw)
	var account gen.BillingPsp
	if id, ok := parsePSPIDArg(raw); ok {
		if account, err = database.Gen(ctx).GetPSP(ctx, gen.GetPSPParams{ID: id, MerchantID: mid.UUID()}); err != nil {
			return "", reconcile.PSPBinding{}, fmt.Errorf("load PSP %s: %w", raw, err)
		}
	} else {
		psps, err := database.Gen(ctx).ListPSPsForMerchant(ctx, mid.UUID())
		if err != nil {
			return "", reconcile.PSPBinding{}, err
		}
		found := 0
		for _, p := range psps {
			if p.Key == raw && p.SupersededAt == nil {
				account, found = p, found+1
			}
		}
		switch found {
		case 0:
			return "", reconcile.PSPBinding{}, fmt.Errorf("no current PSP with key %q for this merchant; name a superseded one by its id", raw)
		case 1:
		default:
			return "", reconcile.PSPBinding{}, fmt.Errorf("%d current PSPs hold key %q: name one by its id", found, raw)
		}
	}
	return reconcile.Provider(account.Rail), reconcile.PSPBinding{ID: account.ID, Rail: account.Rail, AccountID: account.AccountID}, nil
}

func parsePSPIDArg(raw string) (uuid.UUID, bool) {
	if id, err := uuid.Parse(raw); err == nil {
		return id, true
	}
	if id, err := billing.ParsePSPID(raw); err == nil && !id.IsZero() {
		return id.UUID(), true
	}
	return uuid.Nil, false
}

func parseProviderTime(s, name string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --%s %q (want RFC3339 or YYYY-MM-DD)", name, s)
	}
	return t, nil
}

func nmiClients(transport http.RoundTripper) *railresolve.NMIFactory {
	if transport == nil {
		return nil
	}
	return &railresolve.NMIFactory{Transport: transport}
}
