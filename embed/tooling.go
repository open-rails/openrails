package embed

import (
	"context"
	"fmt"
	"io"

	"github.com/open-rails/openrails/internal/hosttools"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Manifest hosts (merchant_source=manifest) author their catalog with
// PushCatalog; the Client's catalog writes are refused for them. API hosts
// author through the Client.

// PushCatalogOptions declares one catalog push. Exactly one of File or
// Manifest names the catalog YAML. Insert, Overwrite and Prune are the
// mutation classes and compose; declaring none is plan-only.
type PushCatalogOptions struct {
	File      string
	Manifest  []byte
	Out       io.Writer
	Insert    bool
	Overwrite bool
	Prune     bool
}

// PushCatalog converges the declared catalog (products, prices by explicit
// key, meters, tier groups) into the runtime's merchant, using its armed PSPs
// and signers. A mutating push runs the full converge for manifest merchants.
func (r *Runtime) PushCatalog(ctx context.Context, opts PushCatalogOptions) error {
	if err := r.initialized(); err != nil {
		return err
	}
	return hosttools.PushMerchantCatalog(ctx, hosttools.CatalogPushOptions{
		App: r.app, File: opts.File, Manifest: opts.Manifest, Out: opts.Out,
		Insert: opts.Insert, Overwrite: opts.Overwrite, Prune: opts.Prune,
	})
}

// ConvergeResult summarizes one merchant-wide convergence pass.
type ConvergeResult = hosttools.ConvergeMerchantResult

// Converge runs one merchant-wide convergence pass on demand: the same
// idempotent engine the scheduled sweep uses, so grants and entitlements derive
// immediately after an import instead of at the next sweep.
func (r *Runtime) Converge(ctx context.Context, merchantID merchant.ID) (ConvergeResult, error) {
	if err := r.initialized(); err != nil {
		return ConvergeResult{}, err
	}
	return hosttools.ConvergeMerchant(ctx, hosttools.ConvergeMerchantOptions{
		Config: r.app.Config, PGXPool: r.app.Runtime.DB.Pool(), MerchantID: merchantID,
	})
}

// PullProviderOptions configures one provider pull. Bare calls are dry-run;
// local writes require Insert, Overwrite or Prune, and a prune writes only
// with PruneExpectRows matching what the pass found.
type PullProviderOptions struct {
	MerchantID      merchant.ID
	Providers       []string
	PSP             string
	Since           string
	Until           string
	Format          string
	LogDir          string
	Insert          bool
	Overwrite       bool
	Prune           bool
	PruneExpectRows *int
	PruneActor      string
	Out             io.Writer
	// MerchantManifest is the MODE 1 manifest the host boots from; nil reads
	// MerchantManifestPath (or the conventional path). Ignored for API hosts.
	MerchantManifest     *BillingConfig
	MerchantManifestPath string
	// Endpoints overrides provider base URLs (a test seam for fake providers).
	Endpoints reconcile.ProviderEndpoints
}

// PullProvider pulls provider-observed state into the merchant's local mirror.
func (r *Runtime) PullProvider(ctx context.Context, opts PullProviderOptions) error {
	if err := r.initialized(); err != nil {
		return err
	}
	return hosttools.PullProvider(ctx, hosttools.PullProviderOptions{
		PGXPool: r.app.Runtime.DB.Pool(), Config: r.app.Config,
		MerchantID: opts.MerchantID, Providers: opts.Providers, PSP: opts.PSP, Since: opts.Since, Until: opts.Until,
		Format: opts.Format, LogDir: opts.LogDir, Insert: opts.Insert, Overwrite: opts.Overwrite, Prune: opts.Prune,
		Out: opts.Out, PruneExpectRows: opts.PruneExpectRows, PruneActor: opts.PruneActor,
		MerchantManifest: opts.MerchantManifest, MerchantManifestPath: opts.MerchantManifestPath, Endpoints: opts.Endpoints,
	})
}

// PullProviderReport renders one recorded pull run.
func (r *Runtime) PullProviderReport(ctx context.Context, merchantID merchant.ID, runID, format string, out io.Writer) error {
	if err := r.initialized(); err != nil {
		return err
	}
	return hosttools.PullProviderReport(ctx, hosttools.PullProviderReportOptions{
		PGXPool: r.app.Runtime.DB.Pool(), Config: r.app.Config, MerchantID: merchantID, RunID: runID, Format: format, Out: out,
	})
}

// ResolveMerchant captures the immutable merchant id and current public name
// behind a public name. Carry the id, never the name, into later operations.
func (r *Runtime) ResolveMerchant(ctx context.Context, name string) (merchant.ID, string, error) {
	if err := r.initialized(); err != nil {
		return merchant.ID{}, "", err
	}
	directory := r.app.Runtime.Merchants
	if directory == nil {
		return merchant.ID{}, "", fmt.Errorf("openrails embed: merchant directory is not armed")
	}
	selected, err := directory.GetBySlug(ctx, name)
	if err != nil {
		return merchant.ID{}, "", err
	}
	return selected.ID, selected.Slug, nil
}

func (r *Runtime) initialized() error {
	if r == nil || r.app == nil || r.app.Runtime == nil || r.app.Runtime.DB == nil {
		return fmt.Errorf("openrails embed: runtime is not initialized")
	}
	return nil
}
