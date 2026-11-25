package hosttools

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/merchantbootstrap"
)

// CatalogApplyOptions is local operator authority. The file never selects its
// own merchant.
type CatalogApplyOptions struct {
	Config  *billing.Config
	PGXPool *pgxpool.Pool
	Vault   *vaultapi.Client
	App     *app.App
	// MerchantManifest is the manifest the host boots from; nil reads
	// MerchantManifestPath (or the conventional path). Managed Vault
	// deployments always use their configured backend instead.
	MerchantManifest     *merchantbootstrap.BillingConfig
	MerchantManifestPath string
	Merchant             string
	File                 string
	Manifest             []byte
	// Force overwrites the fields an edit set and takes them.
	Force bool
	// EventStore is the deployment's; nil refuses meters, limits and rate
	// cards.
	EventStore *eventstore.Store
}

// ApplyMerchantCatalog applies opts' document to opts.Merchant with local
// operator authority.
func ApplyMerchantCatalog(ctx context.Context, logger *slog.Logger, opts CatalogApplyOptions) (*billing.CatalogApplicationReceipt, error) {
	if strings.TrimSpace(opts.Merchant) == "" {
		return nil, fmt.Errorf("catalog merchant is required")
	}
	raw := opts.Manifest
	if opts.File != "" {
		if len(raw) != 0 {
			return nil, fmt.Errorf("specify a catalog file or bytes, not both")
		}
		file, err := os.Open(opts.File) // #nosec G304 -- explicit operator file, never HTTP input
		if err != nil {
			return nil, err
		}
		defer file.Close()
		raw, err = io.ReadAll(io.LimitReader(file, catalog.MaxApplicationBytes+1))
		if err != nil {
			return nil, err
		}
	}
	params, err := catalog.ParseApplicationYAML(raw)
	if err != nil {
		return nil, err
	}
	rt, svc, cleanup, err := catalogRuntime(ctx, logger, opts)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	ctx, _, err = catalogMerchantContext(ctx, rt.Merchants, opts.Merchant)
	if err != nil {
		return nil, err
	}
	return svc.ApplyOperatorCatalog(ctx, *params, billing.ApplyCatalogParams{Force: opts.Force})
}
