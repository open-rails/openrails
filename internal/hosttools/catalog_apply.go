package hosttools

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/service"
)

// CatalogApplyOptions is local operator authority. The file never selects its
// own merchant.
type CatalogApplyOptions struct {
	Config  *config.Config
	PGXPool *pgxpool.Pool
	App     *app.App
	// MerchantManifestPath optionally supplies the host-owned credential snapshot.
	// Managed DB/Vault deployments always use their configured backend instead.
	MerchantManifestPath string
	Merchant             string
	File                 string
	Manifest             []byte
	// Force overwrites the fields an edit set and takes them.
	Force bool
	Out   io.Writer
}

func ApplyMerchantCatalog(ctx context.Context, opts CatalogApplyOptions) (*billing.CatalogApplicationReceipt, error) {
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
	receipt, err := svc.ApplyCatalog(ctx, *params, billing.ApplyCatalogParams{Force: opts.Force})
	if err != nil {
		return nil, err
	}
	if opts.Out != nil {
		if err := WriteCatalogReceipt(opts.Out, receipt); err != nil {
			return nil, fmt.Errorf("write catalog receipt (application committed): %w", err)
		}
	}
	return receipt, nil
}

// WriteCatalogReceipt writes a catalog application's result for a person:
// each object it changed, and each it skipped with readable values.
func WriteCatalogReceipt(w io.Writer, r *billing.CatalogApplicationReceipt) error {
	var b strings.Builder
	switch {
	case r.Replayed:
		fmt.Fprintf(&b, "%s was already applied; nothing changed\n", r.ApplicationID)
	default:
		fmt.Fprintf(&b, "%s: catalog revision %d -> %d\n", r.ApplicationID, r.BaseRevision, r.AppliedRevision)
	}
	for _, c := range r.Changes {
		name := fmt.Sprintf("%s %s", c.Object, c.Key)
		if c.ProductKey != nil {
			name = fmt.Sprintf("price %s of product %s", c.Key, *c.ProductKey)
		}
		fmt.Fprintf(&b, "  changed %s (%s), now revision %d\n", name, strings.Join(c.Fields, ", "), c.Revision)
	}
	for _, line := range service.ReadableCatalogConflicts(r.Conflicts) {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	if len(r.Conflicts) > 0 {
		fmt.Fprintf(&b, "%d object(s) skipped: an edit set the fields above. Change the file to agree, remove those fields, or apply with --force-conflicts.\n", len(r.Conflicts))
	}
	_, err := io.WriteString(w, b.String())
	return err
}
