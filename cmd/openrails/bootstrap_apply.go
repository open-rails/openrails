package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	authcore "github.com/open-rails/authkit/core"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/pkg/catalog"
	"github.com/open-rails/openrails/pkg/embedded"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
	"github.com/open-rails/openrails/pkg/merchant"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

type bootstrapApplyOptions struct {
	file   string
	dryRun bool
	prune  bool
}

func newBootstrapCmd() *cobra.Command {
	opts := bootstrapApplyOptions{file: bootstrap.DefaultBootstrapManifestPath}
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Apply declarative OpenRails provisioning manifests",
	}
	apply := &cobra.Command{
		Use:   "apply",
		Short: "Apply merchants and catalog from a unified bootstrap YAML file",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBootstrapApply(cmd, opts)
		},
	}
	apply.Flags().StringVarP(&opts.file, "file", "f", bootstrap.DefaultBootstrapManifestPath, "bootstrap manifest YAML file")
	apply.Flags().BoolVar(&opts.dryRun, "dry-run", false, "validate and print plans without mutating state")
	apply.Flags().BoolVar(&opts.prune, "prune", false, "treat the local catalog as complete and archive OpenRails-owned provider objects that are absent from it; foreign provider objects are never touched")
	apply.Flags().BoolVar(&opts.prune, "exhaustive", false, "deprecated alias for --prune")
	_ = apply.Flags().MarkHidden("exhaustive")
	cmd.AddCommand(apply)
	return cmd
}

func runBootstrapApply(cmd *cobra.Command, opts bootstrapApplyOptions) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	path := strings.TrimSpace(opts.file)
	if path == "" {
		path = bootstrap.DefaultBootstrapManifestPath
	}

	manifest, err := bootstrap.LoadBootstrapManifest(path)
	if err != nil {
		return err
	}

	cfg, _ := ctx.Value(config.ConfigContextKey).(*config.Config)
	if cfg == nil {
		return fmt.Errorf("config not loaded; bootstrap apply requires --config")
	}

	// --prune archives provider-side catalog objects — provider WRITES
	// that flow through the provider intent ledger (#358) as ADMIN-origin
	// intents (the flag is an explicit human request). No up-front mode
	// refusal: under mode=limited the intents execute; under readonly they
	// PARK durably and the server's scheduled executor drains them when the
	// mode lifts. Default extras detection/logging is read-only in every mode.
	embeddedApp, err := embedded.New(embedded.Options{Config: cfg})
	if err != nil {
		return fmt.Errorf("bootstrap application: %w", err)
	}
	defer func() {
		if closeErr := embeddedApp.Close(context.Background()); closeErr != nil {
			log.WithError(closeErr).Error("application cleanup failed")
		}
	}()

	if err := embcp.Attach(ctx, embeddedApp.App(), cfg, nil); err != nil {
		return fmt.Errorf("attach control plane: %w", err)
	}

	return applyBootstrapManifest(ctx, cfg, embeddedApp.App(), manifest, out, opts.dryRun, opts.prune)
}

// startupBootstrapLockKey is the pg_advisory_lock key serializing concurrent
// startup bootstraps (#342). The apply is plan-then-execute with no internal
// transaction, so simultaneous replica cold starts against an empty control
// plane would each plan the same creates and race the inserts (23505 on
// unique_prices_product_amount_cycle). Holding this lock across plan+apply
// makes the second replica plan against the converged state instead.
const startupBootstrapLockKey = int64(0x6f72_626f_6f74) // "orboot"

// applyStartupBootstrap applies the unified bootstrap manifest on EVERY server
// start (#327). The apply is idempotent + additive — merchant data is reconciled
// (upsert; issuers/principals are registered if missing) and catalogs plan with
// ArchiveMissingProducts:false, so nothing is ever removed — so reapplying on
// every boot is safe and lets a mounted bootstrap file converge an existing
// control plane (e.g. register a delegated issuer that was added after the DB
// was first provisioned). No-op when no manifest is configured/present.
// Concurrent replicas serialize on a session-level advisory lock (#342); the
// lock auto-releases if the holder dies.
func applyStartupBootstrap(ctx context.Context, cfg *config.Config, a *app.App) error {
	path := resolveBootstrapManifestPath(cfg)
	if path == "" {
		return nil
	}
	cp := embcp.Get(a)
	if cp == nil {
		return fmt.Errorf("startup bootstrap: control plane not attached (#469: it is mandatory in standalone mode)")
	}
	manifest, err := bootstrap.LoadBootstrapManifest(path)
	if err != nil {
		return err
	}

	// Dedicated connection: pg_advisory_lock is session-scoped, so the lock
	// must be taken and released on the same conn, held for the whole apply.
	conn, err := cp.Pool().Raw().Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire bootstrap lock connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, startupBootstrapLockKey); err != nil {
		return fmt.Errorf("acquire bootstrap advisory lock: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, startupBootstrapLockKey); err != nil {
			log.WithError(err).Warn("release bootstrap advisory lock")
		}
	}()

	log.WithField("file", path).Info("startup bootstrap: applying unified manifest (idempotent)")
	// prune=false: the startup re-apply is additive-only; archiving extras
	// is reserved for the explicit `bootstrap apply --prune` CLI (#357).
	return applyBootstrapManifest(ctx, cfg, a, manifest, log.StandardLogger().Out, false, false)
}

// resolveBootstrapManifestPath returns the conventional bootstrap manifest
// location when that file exists, else "". There is deliberately no config
// knob for this path (#350 hard cut of the legacy tenant_bootstrap.file): the
// manifest lives at the one conventional path, or is passed explicitly to the
// `bootstrap apply` CLI.
func resolveBootstrapManifestPath(_ *config.Config) string {
	if _, err := os.Stat(bootstrap.DefaultBootstrapManifestPath); err == nil {
		return bootstrap.DefaultBootstrapManifestPath
	}
	return ""
}

// applyBootstrapManifest provisions the merchants and catalog declared by a
// unified bootstrap manifest. It is the single apply
// path shared by the `bootstrap apply` CLI and the server's first-run startup
// bootstrap (#327), so both consume the same one manifest schema.
//
// After the catalogs converge it additionally detects provider-side catalog
// EXTRAS — products/prices/plans on the provider account that are NOT in the
// local catalog (#357). By default extras are only logged (read-only; they are
// ignorable and never touched). Under prune=true the OpenRails-marked
// extras are ARCHIVED (never deleted) through the provider intent ledger
// (#358): Stripe active=false; Solana plans sunset on-chain; NMI/CCBill
// surface pending manual actions. Foreign provider objects are never touched.
func applyBootstrapManifest(ctx context.Context, cfg *config.Config, a *app.App, manifest *bootstrap.BootstrapManifest, out io.Writer, dryRun bool, prune bool) error {
	if manifest.HasAuthBootstrap() {
		cp := embcp.Get(a)
		if cp == nil || cp.Core() == nil {
			return fmt.Errorf("auth bootstrap: control plane not attached")
		}
		result, err := cp.Core().ReconcileBootstrapManifest(ctx, *manifest.Auth, authcore.FileBootstrapTokenStore{}, authcore.BootstrapReconcileOptions{DryRun: dryRun})
		if err != nil {
			return fmt.Errorf("auth bootstrap: %w", err)
		}
		printAuthBootstrapResult(out, result)
	}

	if len(manifest.Merchants) > 0 {
		if dryRun {
			fmt.Fprintf(out, "merchants: %d declared (dry-run: no merchant mutations)\n", len(manifest.Merchants))
		} else {
			cp := embcp.Get(a)
			if cp == nil {
				return fmt.Errorf("merchant bootstrap: control plane not attached")
			}
			if err := bootstrap.ReconcileMerchantManifestData(ctx, cfg, cp, manifest.MerchantManifest(), bootstrap.MerchantManifestReconcileOptions{}); err != nil {
				return fmt.Errorf("merchant bootstrap: %w", err)
			}
			fmt.Fprintf(out, "merchants: %d reconciled\n", len(manifest.Merchants))
		}
	}

	if len(manifest.Catalogs) == 0 {
		return nil
	}

	svc, err := billingservice.New(a.Runtime)
	if err != nil {
		return fmt.Errorf("construct OpenRails service: %w", err)
	}
	applier := catalog.NewServiceApplier(svc)

	for i := range manifest.Catalogs {
		cat, err := manifest.CatalogManifest(i)
		if err != nil {
			return err
		}
		label := strings.TrimSpace(manifest.Catalogs[i].Name)
		if label == "" {
			label = manifest.Catalogs[i].Merchant
		}
		catalogCtx, err := contextForBootstrapCatalog(ctx, a, manifest.Catalogs[i].Merchant)
		if err != nil {
			return err
		}
		plan, err := catalog.PlanWithOptions(catalogCtx, applier, cat, catalog.PlanOptions{ArchiveMissingProducts: false})
		if err != nil {
			return fmt.Errorf("plan %s: %w", label, err)
		}
		fmt.Fprintf(out, "\n%s plan:\n", label)
		plan.Print(out, dryRun)
		if dryRun {
			continue
		}
		if !plan.HasChanges() {
			fmt.Fprintf(out, "\n%s: no changes\n", label)
			continue
		}
		result, err := catalog.Apply(catalogCtx, applier, plan)
		if err != nil {
			return fmt.Errorf("apply %s: %w", label, err)
		}
		fmt.Fprintln(out)
		result.Print(out)
	}

	// #357: extras pass. Gated on the manifest actually declaring catalogs —
	// an empty/merchants-only manifest makes no prune claim about the
	// provider accounts, so nothing is scanned (and nothing could ever be
	// archived) for it.
	return reportCatalogExtras(ctx, svc, out, dryRun, prune)
}

func printAuthBootstrapResult(out io.Writer, result authcore.BootstrapManifestResult) {
	mode := "reconciled"
	if result.DryRun {
		mode = "planned (dry-run)"
	}
	fmt.Fprintf(out, "auth: %s users=%d updated=%d passwords_set=%d passwords_kept=%d global_roles=%d global_role_assignments=%d orgs=%d issuers=%d roles=%d memberships=%d service_tokens_minted=%d service_tokens_kept=%d\n",
		mode,
		result.UsersCreated,
		result.UsersUpdated,
		result.PasswordsSet,
		result.PasswordsKept,
		result.GlobalRoles,
		result.GlobalRoleAssignments,
		result.OrgManifest.Orgs,
		result.OrgManifest.Issuers,
		result.OrgManifest.Roles,
		result.OrgManifest.Memberships,
		result.OrgManifest.TokensMinted,
		result.OrgManifest.TokensKept,
	)
}

// reportCatalogExtras runs the #357 extras pass after a catalog apply: detect
// + log provider-side objects not in the local catalog (always; read-only),
// and archive the OpenRails-marked ones when prune is set. Under dry-run
// the archive half only prints what it WOULD archive.
//
// Detection failure is non-fatal on a default apply (the apply itself
// succeeded; extras logging is advisory) but fatal under --prune, where
// the operator explicitly asked for the provider accounts to be converged.
func reportCatalogExtras(ctx context.Context, svc *billingservice.Service, out io.Writer, dryRun bool, prune bool) error {
	report, err := svc.DetectCatalogExtras(ctx)
	if err != nil {
		if prune {
			return fmt.Errorf("catalog extras detection: %w", err)
		}
		log.WithError(err).Warn("catalog extras detection failed (apply succeeded; provider extras were not checked)")
		fmt.Fprintf(out, "\ncatalog extras: detection failed (apply unaffected): %v\n", err)
		return nil
	}

	fmt.Fprintf(out, "\ncatalog extras: %d provider-side object(s) not in the local catalog (scanned %d stripe products, %d stripe prices, %d nmi plans, %d solana plans)\n",
		len(report.Extras), report.ScannedStripeProducts, report.ScannedStripePrices, report.ScannedNMIPlans, report.ScannedSolanaPlans)
	for _, e := range report.Extras {
		marker := "foreign — never touched"
		if e.Owned {
			marker = "openrails-marked"
		}
		state := ""
		if !e.Active {
			state = ", inactive"
		}
		fmt.Fprintf(out, "  - %s %s %s (%s) [%s%s]\n", e.Provider, e.ObjectType, e.ExternalID, e.Label, marker, state)
		log.WithFields(log.Fields{
			"event":       "catalog_extra",
			"provider":    e.Provider,
			"object_type": e.ObjectType,
			"external_id": e.ExternalID,
			"label":       e.Label,
			"owned":       e.Owned,
			"active":      e.Active,
		}).Info("bootstrap apply: provider-side catalog extra (ignorable; not in local catalog)")
	}
	for _, n := range report.Notes {
		fmt.Fprintf(out, "  note (%s): %s\n", n.Provider, n.Note)
	}

	if !prune || len(report.Extras) == 0 {
		return nil
	}
	if dryRun {
		fmt.Fprintf(out, "\nprune (dry-run): would archive the openrails-marked extras above; foreign objects would not be touched\n")
		return nil
	}
	outcomes, archiveErr := svc.ArchiveCatalogExtras(ctx, report.Extras)
	var archived, parked, failedN int
	fmt.Fprintf(out, "\nprune archive outcomes:\n")
	for _, o := range outcomes {
		switch o.Action {
		case billingservice.CatalogExtraArchived:
			archived++
		case billingservice.CatalogExtraArchiveParked:
			parked++
		case billingservice.CatalogExtraArchiveFailed:
			failedN++
		}
		fmt.Fprintf(out, "  - %s %s %s: %s (%s)\n", o.Extra.Provider, o.Extra.ObjectType, o.Extra.ExternalID, o.Action, o.Detail)
		log.WithFields(log.Fields{
			"event":       "catalog_extra_archive",
			"provider":    o.Extra.Provider,
			"object_type": o.Extra.ObjectType,
			"external_id": o.Extra.ExternalID,
			"action":      string(o.Action),
			"detail":      o.Detail,
			"intent_id":   o.IntentID,
		}).Info("bootstrap apply --prune: catalog extra archive outcome")
	}
	fmt.Fprintf(out, "prune summary: %d archived, %d parked (durable — the intent executor drains them; NOT an error), %d failed\n",
		archived, parked, failedN)
	return archiveErr
}

func contextForBootstrapCatalog(ctx context.Context, a *app.App, merchantSlug string) (context.Context, error) {
	slug := strings.ToLower(strings.TrimSpace(merchantSlug))
	if slug == "" {
		return nil, fmt.Errorf("catalog merchant is required")
	}
	cp := embcp.Get(a)
	if cp == nil || cp.Pool() == nil {
		return nil, fmt.Errorf("catalog declares merchant %q but the control plane is not attached", merchantSlug)
	}
	var id string
	if err := cp.Pool().QueryRow(ctx, `SELECT id::text FROM openrails.merchants WHERE lower(slug) = lower($1)`, slug).Scan(&id); err != nil {
		return nil, fmt.Errorf("resolve catalog merchant %q: %w", slug, err)
	}
	tid, err := merchant.ParseID(id)
	if err != nil {
		return nil, fmt.Errorf("parse catalog merchant %q id: %w", slug, err)
	}
	return merchant.WithID(ctx, tid), nil
}
