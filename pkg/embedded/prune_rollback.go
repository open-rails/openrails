package embedded

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PruneRollbackOptions mirrors `openrails prune rollback`.
type PruneRollbackOptions struct {
	Config       *config.Config
	PGXPool      *pgxpool.Pool
	MerchantSlug string
	RunID        string
	Actor        string
	Format       string
	Out          io.Writer
}

// PruneListOptions mirrors `openrails prune list`.
type PruneListOptions struct {
	Config       *config.Config
	PGXPool      *pgxpool.Pool
	MerchantSlug string
	Limit        int
	Format       string
	Out          io.Writer
}

// PruneRollback reverses one destructive run by id: every row it soft-deleted
// comes back, in one transaction (or#858).
//
// A rollback is NOT a complete operation on its own — it restores local state to
// before the run while the provider moved on. `rollback → pull → converge` is
// (or#859 §2.1), and the command says so on its way out.
func PruneRollback(ctx context.Context, opts PruneRollbackOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	runID, err := uuid.Parse(strings.TrimSpace(opts.RunID))
	if err != nil {
		return fmt.Errorf("prune rollback: --run must be a destructive-run UUID (see `openrails prune list`): %w", err)
	}
	database, err := newCatalogPushDB(opts.Config, opts.PGXPool)
	if err != nil {
		return err
	}
	defer database.Close()

	merchantID, err := db.ResolveMerchantSlug(ctx, database.Pool(), strings.TrimSpace(opts.MerchantSlug))
	if err != nil {
		return fmt.Errorf("prune rollback: resolve merchant %q: %w", opts.MerchantSlug, err)
	}
	ctx = merchant.WithID(ctx, merchantID)

	var res reconcile.RollbackResult
	if err := database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var e error
		res, e = reconcile.RollbackDestructiveRun(ctx, database, runID, opts.Actor)
		return e
	}); err != nil {
		return err
	}

	if strings.EqualFold(opts.Format, "json") {
		return json.NewEncoder(opts.Out).Encode(map[string]any{
			"run_id":            res.RunID.String(),
			"subscriptions":     res.Subscriptions,
			"payments":          res.Payments,
			"checkout_sessions": res.CheckoutSessions,
			"entitlements":      res.Entitlements,
		})
	}
	fmt.Fprintf(opts.Out, "restored run %s: subscriptions=%d payments=%d checkout_sessions=%d entitlements=%d\n",
		res.RunID, res.Subscriptions, res.Payments, res.CheckoutSessions, res.Entitlements)
	fmt.Fprintf(opts.Out, "local state is now BEFORE the prune while the provider is at now — a rollback is not a complete operation.\n"+
		"Finish it:  openrails pull-provider --merchant %s --insert --overwrite\n", strings.TrimSpace(opts.MerchantSlug))
	return nil
}

// PruneList shows this merchant's destructive runs, newest first — the ids
// `prune rollback --run` takes.
func PruneList(ctx context.Context, opts PruneListOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	database, err := newCatalogPushDB(opts.Config, opts.PGXPool)
	if err != nil {
		return err
	}
	defer database.Close()

	merchantID, err := db.ResolveMerchantSlug(ctx, database.Pool(), strings.TrimSpace(opts.MerchantSlug))
	if err != nil {
		return fmt.Errorf("prune list: resolve merchant %q: %w", opts.MerchantSlug, err)
	}
	ctx = merchant.WithID(ctx, merchantID)

	var runs []gen.OpenrailsDestructiveRun
	if err := database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var e error
		runs, e = database.Gen(ctx).ListDestructiveRuns(ctx, gen.ListDestructiveRunsParams{
			MerchantID: merchantID.UUID(), Lim: int32(limit),
		})
		return e
	}); err != nil {
		return err
	}

	if strings.EqualFold(opts.Format, "json") {
		return json.NewEncoder(opts.Out).Encode(runs)
	}
	if len(runs) == 0 {
		fmt.Fprintln(opts.Out, "no destructive runs recorded for this merchant")
		return nil
	}
	for i := range runs {
		r := &runs[i]
		fmt.Fprintf(opts.Out, "%s  %-8s  %-9s  actor=%-16s  started=%s  affected=%s\n",
			r.ID, r.Kind, r.Status, r.Actor, r.StartedAt.UTC().Format("2006-01-02T15:04:05Z"), string(r.Affected))
	}
	return nil
}
