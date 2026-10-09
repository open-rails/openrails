package hosttools

import (
	"context"
	"fmt"
	"io"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/service"
)

// Local maintenance over an owned engine graph: process ownership is the
// authority, so none of these is reachable through a Client or HTTP route.

func initialized(a *app.App) error {
	if a == nil || a.Runtime == nil || a.Runtime.DB == nil {
		return fmt.Errorf("openrails: engine is not initialized")
	}
	return nil
}

// Converge runs one merchant-wide convergence pass now: the engine the
// scheduled sweep uses, so grants and entitlements derive right after an import.
func Converge(ctx context.Context, a *app.App, merchantID billing.MerchantID) (ConvergeMerchantResult, error) {
	if err := initialized(a); err != nil {
		return ConvergeMerchantResult{}, err
	}
	return ConvergeMerchant(ctx, ConvergeMerchantOptions{
		Config: a.Config, PGXPool: a.Runtime.DB.Pool(), MerchantID: merchantID, Clock: a.Runtime.Clock,
	})
}

// PullProviderRun configures one provider pull. Bare calls are dry-run; local
// writes require Insert, Overwrite or Prune, and a prune writes only with
// PruneExpectRows matching what the pass found.
type PullProviderRun struct {
	MerchantID      billing.MerchantID
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
	// MerchantManifest is the manifest the host boots from; nil reads
	// MerchantManifestPath (or the conventional path).
	MerchantManifest     *merchantbootstrap.BillingConfig
	MerchantManifestPath string
	// Endpoints overrides provider base URLs (a test seam for fake providers).
	Endpoints reconcile.ProviderEndpoints
}

// Pull pulls provider-observed state into the merchant's local mirror.
func Pull(ctx context.Context, a *app.App, run PullProviderRun) error {
	if err := initialized(a); err != nil {
		return err
	}
	return PullProvider(ctx, PullProviderOptions{
		StripeClients: a.Runtime.StripeClients,
		PGXPool:       a.Runtime.DB.Pool(), Config: a.Config,
		MerchantID: run.MerchantID, Providers: run.Providers, PSP: run.PSP, Since: run.Since, Until: run.Until,
		Format: run.Format, LogDir: run.LogDir, Insert: run.Insert, Overwrite: run.Overwrite, Prune: run.Prune,
		Out: run.Out, PruneExpectRows: run.PruneExpectRows, PruneActor: run.PruneActor,
		MerchantManifest: run.MerchantManifest, MerchantManifestPath: run.MerchantManifestPath, Endpoints: run.Endpoints,
	})
}

// ResolveMerchant captures the immutable merchant ID and current name behind a
// public name. Carry the ID, never the name, into later operations.
func ResolveMerchant(ctx context.Context, a *app.App, name string) (billing.MerchantID, string, error) {
	if err := initialized(a); err != nil {
		return billing.MerchantID{}, "", err
	}
	if a.Runtime.Merchants == nil {
		return billing.MerchantID{}, "", fmt.Errorf("openrails: merchant directory is not armed")
	}
	selected, err := a.Runtime.Merchants.GetBySlug(ctx, name)
	if err != nil {
		return billing.MerchantID{}, "", err
	}
	return selected.ID, selected.Slug, nil
}

// ApplyCatalogAsOperator applies a catalog to one explicitly selected merchant
// with operator authority; other callers' writes remain governed by
// Routes.CatalogEdits.
func ApplyCatalogAsOperator(ctx context.Context, a *app.App, merchantID billing.MerchantID, params *catalog.Application) (*billing.CatalogApplicationReceipt, error) {
	if err := initialized(a); err != nil {
		return nil, err
	}
	if merchantID.IsZero() || params == nil {
		return nil, fmt.Errorf("merchant and catalog application are required")
	}
	svc, err := service.New(a.Runtime)
	if err != nil {
		return nil, err
	}
	return svc.ApplyCatalog(catalogpolicy.OperatorContext(merchant.WithID(ctx, merchantID)), *params)
}

// RegisterMerchantForRestore registers a preserved merchant UUID for a host
// without a control plane, then binds the engine to it. Call during startup,
// before serving or starting workers. It registers no PSPs or credentials.
func RegisterMerchantForRestore(ctx context.Context, a *app.App, id billing.MerchantID, slug string) (billing.MerchantID, error) {
	if err := initialized(a); err != nil {
		return billing.MerchantID{}, err
	}
	if a.ControlPlane != nil {
		return billing.MerchantID{}, fmt.Errorf("openrails: an attached control plane restores through ProvisionMerchantForRestore with destination group authority")
	}
	if bound := a.Runtime.ConfiguredMerchant(); !bound.IsZero() && bound != id {
		return billing.MerchantID{}, merchants.ErrMerchantRestoreConflict
	}
	directory, err := merchants.NewDirectoryService(a.Runtime.DB.DataPool())
	if err != nil {
		return billing.MerchantID{}, err
	}
	m, _, err := directory.RegisterForRestore(ctx, id, slug)
	if err != nil {
		return billing.MerchantID{}, err
	}
	a.Runtime.SetConfiguredMerchant(m.ID)
	return m.ID, nil
}

// ResolveSolanaPayReview closes a Solana Pay review receipt (a second, late,
// short or unreadable transfer, or an overpayment's excess) once its money was
// settled outside OpenRails. Unresolved reviews hold the billing archive back.
func ResolveSolanaPayReview(ctx context.Context, a *app.App, merchantID billing.MerchantID, signature, resolution string) error {
	if err := initialized(a); err != nil {
		return err
	}
	if a.Runtime.CheckoutAttemptService == nil {
		return fmt.Errorf("openrails: this engine has no checkout attempts")
	}
	return a.Runtime.CheckoutAttemptService.ResolveSolanaPayReview(merchant.WithID(ctx, merchantID), signature, resolution)
}

// ApproveSolanaSigner accepts the Solana identity a Vault Transit signer key
// now reports after it changed. Until then the Solana rail refuses and the
// openrails_solana_signer_identity probe fails. Verify the new public key
// (in the ERROR log and the probe) before calling.
func ApproveSolanaSigner(ctx context.Context, a *app.App, merchantID billing.MerchantID, key string) error {
	if err := initialized(a); err != nil {
		return err
	}
	if a.Runtime.ApproveSolanaSigner == nil {
		return fmt.Errorf("openrails: this engine cannot approve Solana signers")
	}
	return a.Runtime.ApproveSolanaSigner(ctx, merchantID, key)
}
