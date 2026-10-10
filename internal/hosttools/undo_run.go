package hosttools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/converge"
)

// UndoRunOptions mirrors `openrails undo-run`.
type UndoRunOptions struct {
	Config     *config.Config
	PGXPool    *pgxpool.Pool
	MerchantID billing.MerchantID
	RunID      string
	Actor      string
	// Apply is false by default: an undo prints its plan and changes nothing
	// until an operator asks for it AND types the row count back.
	Apply bool
	// ExpectRows is that typed confirmation. Required with Apply.
	ExpectRows int64
	Format     string
	Out        io.Writer
	// MerchantConfig is a running app's merchant configuration; nil opens it.
	MerchantConfig db.MerchantConfig
}

// UndoRun reverses one destructive run of any reversible kind or, by default,
// prints what reversing it would do. One verb covers the run ledger: a prune
// reverses by clearing tombstones, a converge-enforce pass from captured
// before-images, and an operator should not need to know which.
//
// A complete rollback is rollback → pull → converge. The derive half runs
// here; the provider pull is the operator's next step and is advisory until
// enforcement is re-armed by hand.
func UndoRun(ctx context.Context, opts UndoRunOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	runID, err := uuid.Parse(strings.TrimSpace(opts.RunID))
	if err != nil {
		return fmt.Errorf("undo-run: --run must be a destructive-run UUID (see `openrails prune list` / `openrails converge list`): %w", err)
	}
	database, err := openEmbeddedDB(ctx, opts.Config, opts.PGXPool)
	if err != nil {
		return err
	}
	defer database.Close()

	merchantID := opts.MerchantID
	if err := database.RequireMerchantID(ctx, merchantID); err != nil {
		return err
	}
	closeConfig, err := bindMerchantConfig(ctx, opts.Config, database, merchantID, opts.MerchantConfig)
	if err != nil {
		return err
	}
	defer closeConfig()
	ctx = merchant.WithID(ctx, merchantID)
	jsonOut := strings.EqualFold(strings.TrimSpace(opts.Format), "json")

	if !opts.Apply {
		var plan reconcile.UndoPlan
		if err := database.RunInMerchantConn(ctx, func(ctx context.Context) error {
			var e error
			plan, e = reconcile.PlanUndoRun(ctx, database, runID)
			return e
		}); err != nil {
			return err
		}
		if jsonOut {
			return encodeJSON(opts.Out, plan)
		}
		printUndoPlan(opts.Out, plan, merchantID)
		return nil
	}

	var res reconcile.UndoResult
	if err := database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		// A converge reversal invalidates Class D, so the re-derivation runs in
		// this command: until it does, restored subscriptions carry no access.
		recompute := func(ctx context.Context) error {
			_, cerr := converge.NewConvergeEngine(database).Converge(ctx, converge.Scope{Merchant: merchantID})
			return cerr
		}
		var e error
		res, e = reconcile.UndoRun(ctx, database, runID, opts.Actor, opts.ExpectRows, recompute)
		return e
	}); err != nil {
		return err
	}
	if jsonOut {
		return encodeJSON(opts.Out, res)
	}
	printUndoResult(opts.Out, res, merchantID)
	return nil
}

func encodeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// merchantFlag is the --merchant value every CLI command resolves exactly.
func merchantFlag(id billing.MerchantID) string { return "id:" + id.String() }

func printUndoPlan(w io.Writer, plan reconcile.UndoPlan, merchantID billing.MerchantID) {
	fmt.Fprintf(w, "DRY RUN — nothing has been changed.\n\n")
	fmt.Fprintf(w, "run %s\n", plan.RunID)
	fmt.Fprintf(w, "  kind    : %s (%s)\n", plan.Kind, reconcile.ReversibleRunKinds[plan.Kind])
	fmt.Fprintf(w, "  status  : %s, opened by %s at %s\n", plan.Status, plan.Actor, plan.StartedAt.Format("2006-01-02T15:04:05Z"))
	if plan.Scope.PspScoped {
		fmt.Fprintf(w, "  scope   : merchant %s, PSP %s\n", plan.Scope.MerchantID, plan.Scope.PspID)
	} else {
		fmt.Fprintf(w, "  scope   : merchant %s (merchant-wide — the pass was not account-bound)\n", plan.Scope.MerchantID)
	}
	fmt.Fprintln(w, "\nwould restore:")
	for _, table := range []string{"subscriptions", "payments", "checkout_attempts", "product_access"} {
		if n, ok := plan.Restorable[table]; ok {
			fmt.Fprintf(w, "  %-18s %d\n", table, n)
		}
	}
	if plan.AccessToInvalidate > 0 {
		fmt.Fprintf(w, "  %-18s %d (INVALIDATED, not restored — Converge rebuilds them from the grant log)\n",
			"product_access", plan.AccessToInvalidate)
	}
	if plan.SubscriptionsChanged > 0 {
		fmt.Fprintf(w, "  %-18s %d (changed since the run; their newer state is kept)\n", "subscriptions kept", plan.SubscriptionsChanged)
	}
	if plan.SubscriptionsTombstoned > 0 {
		fmt.Fprintf(w, "  %-18s %d before-image(s) whose row a LATER prune tombstoned — those belong to that run's undo, not this one\n",
			"skipped", plan.SubscriptionsTombstoned)
	}
	fmt.Fprintf(w, "\nprovider writes this run queued:\n")
	fmt.Fprintf(w, "  %-18s %d (would be superseded before any row is restored)\n", "unfired", plan.IntentsUnfired)
	for _, d := range plan.IntentsIrreversible {
		fmt.Fprintf(w, "  IRREVERSIBLE  %s %s: %s\n", d.IntentType, d.IntentID, d.Consequence)
	}
	for _, d := range plan.IntentsAmbiguous {
		fmt.Fprintf(w, "  AMBIGUOUS     %s %s: %s\n", d.IntentType, d.IntentID, d.Consequence)
	}
	if !plan.Complete() {
		fmt.Fprintf(w, "  this reversal will NOT be complete: %d provider write(s) already reached the rail.\n",
			len(plan.IntentsIrreversible)+len(plan.IntentsAmbiguous))
	}
	// PlanUndoRun refuses when this is non-zero; printed so the operator sees
	// the coverage proof.
	fmt.Fprintf(w, "  coverage: every live provider row is PSP-attributed (unattributed=%d)\n", plan.Unattributed.Total())
	fmt.Fprintf(w, "\nTo apply, confirm the row count:\n"+
		"  openrails undo-run --merchant %s --run %s --apply --expect-rows %d\n",
		merchantFlag(merchantID), plan.RunID, plan.ExpectedRows())
}

func printUndoResult(w io.Writer, res reconcile.UndoResult, merchantID billing.MerchantID) {
	fmt.Fprintf(w, "reversed run %s (kind %s)\n", res.Plan.RunID, res.Plan.Kind)
	for _, table := range []string{"subscriptions", "payments", "checkout_attempts", "product_access"} {
		if n, ok := res.Restored[table]; ok {
			fmt.Fprintf(w, "  restored %-18s %d\n", table, n)
		}
	}
	if res.AccessInvalidated > 0 {
		fmt.Fprintf(w, "  invalidated for re-derivation: %d product access window(s)\n", res.AccessInvalidated)
	}
	if res.IntentsSuperseded > 0 {
		fmt.Fprintf(w, "  unfired provider writes superseded: %d\n", res.IntentsSuperseded)
	}
	if res.ProvenDomainsReset > 0 {
		fmt.Fprintf(w, "  proven source domains reset to unproven: %d (nothing may retract against a book this incomplete)\n", res.ProvenDomainsReset)
	}
	if res.EnforcementDisarmed {
		fmt.Fprintln(w, "  enforcement disarmed: the next pull for this merchant runs ADVISORY until an operator re-arms it")
	}
	if res.Plan.Kind == reconcile.DestructiveRunKindConvergeEnforce {
		if res.Recomputed {
			fmt.Fprintln(w, "  grant effects re-derived from the (untouched) grant log — access is rebuilt, not restored")
		} else {
			fmt.Fprintln(w, "  WARNING: grant effects were NOT re-derived; the restored subscriptions have no access until Converge runs")
		}
	}
	for _, d := range res.IntentsIrreversible {
		fmt.Fprintf(w, "  IRREVERSIBLE  %s %s (%s): %s\n", d.IntentType, d.IntentID, d.Status, d.Consequence)
	}
	for _, d := range res.IntentsAmbiguous {
		fmt.Fprintf(w, "  AMBIGUOUS     %s %s (%s): %s\n", d.IntentType, d.IntentID, d.Status, d.Consequence)
	}
	if res.Complete() {
		fmt.Fprintln(w, "no provider write escaped: this reversal is complete on the local side.")
	} else {
		fmt.Fprintf(w, "%d provider write(s) reached the rail before this reverse did. They are NOT undone and no automatic\n"+
			"re-write will be attempted — resubscribe / card re-entry is operator and customer work.\n",
			len(res.IntentsIrreversible)+len(res.IntentsAmbiguous))
	}
	fmt.Fprintf(w, "local state is now BEFORE the run while the provider is at now — a rollback is not a complete operation.\n"+
		"Finish it:  openrails pull-provider --merchant %s     (advisory; review findings, then re-arm and re-run with mutation flags)\n",
		merchantFlag(merchantID))
}
