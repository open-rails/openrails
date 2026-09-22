package hosttools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	"github.com/open-rails/openrails/pkg/catalog"
	"github.com/open-rails/openrails/pkg/merchant"
)

// CatalogApplyOptions is local operator authority. The file never selects its
// own merchant or enables public catalog mutation routes.
type CatalogApplyOptions struct {
	NameAuthority merchant.NameAuthority
	Config        *config.Config
	PGXPool       *pgxpool.Pool
	App           *app.App
	// MerchantManifestPath optionally supplies the host-owned credential snapshot.
	// Managed DB/Vault deployments always use their configured backend instead.
	MerchantManifestPath string
	Merchant             string
	File                 string
	Manifest             []byte
	Out                  io.Writer
}

func ApplyMerchantCatalog(ctx context.Context, opts CatalogApplyOptions) (*openrails.CatalogApplicationReceipt, error) {
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
	rt, svc, cleanup, err := catalogRuntime(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	ctx, _, err = catalogMerchantContext(ctx, rt.Merchants, opts.Merchant)
	if err != nil {
		return nil, err
	}
	receipt, err := svc.ApplyCatalog(catalogpolicy.OperatorContext(ctx), *params)
	if err != nil {
		return nil, err
	}
	if opts.Out != nil {
		if err := json.NewEncoder(opts.Out).Encode(receipt); err != nil {
			return nil, fmt.Errorf("write catalog receipt (application committed): %w", err)
		}
	}
	return receipt, nil
}
