package reconcile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

// One undo verb covers the whole destructive-run ledger: a prune reverses by
// clearing tombstones, a converge-enforce pass from captured before-images. The
// kind is read from the ledger and dispatched on; a kind with no undo is
// refused by name. Dry-run is the default, and applying needs a typed row count
// matching the plan, because an undo is itself a mass mutation of the live book.

// UndoScope is the scope a run's reversal is confined to. It is descriptive, not
// a filter: the scope is a PROPERTY of the run (the ledger row carries the
// merchant and, when the pass was account-bound, the PSP), and every restore
// predicate is keyed on the run id inside a merchant-scoped connection. There is
// no widening knob, by construction.
type UndoScope struct {
	MerchantID uuid.UUID  `json:"merchant_id"`
	PspID      *uuid.UUID `json:"psp_id,omitempty"`
	// PspScoped is false for a merchant-wide run (an import, a catalog edit).
	PspScoped bool `json:"psp_scoped"`
}

// UnattributedRows counts live provider-bound rows carrying no PSP. psp_id is
// NOT NULL, so every count is zero; a non-zero total means the schema changed
// underneath this code, and the undo refuses rather than under-cover.
type UnattributedRows struct {
	Subscriptions    int64 `json:"subscriptions"`
	Payments         int64 `json:"payments"`
	CheckoutAttempts int64 `json:"checkout_attempts"`
	PaymentMethods   int64 `json:"payment_methods"`
	UnfiredIntents   int64 `json:"unfired_intents"`
}

// Total is how many live rows this merchant holds that no PSP-scoped predicate
// can reach. Always zero while psp_id is NOT NULL.
func (b UnattributedRows) Total() int64 {
	return b.Subscriptions + b.Payments + b.CheckoutAttempts + b.PaymentMethods + b.UnfiredIntents
}

// ErrUnattributedRows is the undo's refusal when the invariant does not hold.
var ErrUnattributedRows = errors.New("provider rows carry no PSP: a PSP-scoped rollback cannot cover them")

// UndoPlan is what an undo WOULD do, computed without mutating anything.
type UndoPlan struct {
	RunID uuid.UUID `json:"run_id"`
	Kind  string    `json:"kind"`
	// Status is the run's ledger status: a `reversed` run is refused.
	Status    string    `json:"status"`
	Actor     string    `json:"actor"`
	StartedAt time.Time `json:"started_at"`
	Scope     UndoScope `json:"scope"`
	// Restorable is the per-table count of rows the apply would bring back or
	// re-assert. Its sum is what `--expect-rows` must match.
	Restorable map[string]int64 `json:"restorable"`
	// AccessToInvalidate are Class D rows the reverse will soft-delete so
	// Converge REBUILDS them from the grant log. Not restorations, so not part
	// of the expected count — but the operator is told.
	AccessToInvalidate int64 `json:"access_to_invalidate"`
	// SubscriptionsChanged are before-images whose row a renewal, cancel or
	// payment moved after the run; the reverse leaves that newer state.
	SubscriptionsChanged int64 `json:"subscriptions_changed"`
	// SubscriptionsTombstoned are before-images whose row a LATER prune has since
	// soft-deleted. They belong to that run's reverse, not this one, so this undo
	// skips them — loudly, because a silent skip is how a partial recovery gets
	// reported as a complete one.
	SubscriptionsTombstoned int64 `json:"subscriptions_tombstoned"`
	// IntentsUnfired would be superseded; Irreversible already reached the
	// provider; Ambiguous may have.
	IntentsUnfired      int                `json:"intents_unfired"`
	IntentsIrreversible []IntentDivergence `json:"intents_irreversible,omitempty"`
	IntentsAmbiguous    []IntentDivergence `json:"intents_ambiguous,omitempty"`
	// Unattributed is the every-row-has-a-PSP invariant, carried in the plan
	// so the operator sees the zero rather than trusting it.
	Unattributed UnattributedRows `json:"unattributed_rows"`
}

// ExpectedRows is the typed confirmation `--apply` must be given.
func (p UndoPlan) ExpectedRows() int64 {
	var n int64
	for _, v := range p.Restorable {
		n += v
	}
	return n
}

// Complete reports whether every provider write this run queued is still ours to
// neutralise. False means part of the damage escaped to the rail and no undo can
// recall it.
func (p UndoPlan) Complete() bool {
	return len(p.IntentsIrreversible) == 0 && len(p.IntentsAmbiguous) == 0
}

// UndoResult is a reversal that ran. It carries the plan it was authorised
// against, so the report shows what was promised next to what happened.
type UndoResult struct {
	Plan UndoPlan `json:"plan"`
	// Restored is the actual per-table count.
	Restored            map[string]int64   `json:"restored"`
	AccessInvalidated   int64              `json:"access_invalidated"`
	IntentsSuperseded   int                `json:"intents_superseded"`
	IntentsIrreversible []IntentDivergence `json:"intents_irreversible,omitempty"`
	IntentsAmbiguous    []IntentDivergence `json:"intents_ambiguous,omitempty"`
	ProvenDomainsReset  int64              `json:"proven_domains_reset"`
	EnforcementDisarmed bool               `json:"enforcement_disarmed"`
	Recomputed          bool               `json:"recomputed"`
}

// Complete mirrors UndoPlan.Complete for the executed reversal.
func (r UndoResult) Complete() bool {
	return len(r.IntentsIrreversible) == 0 && len(r.IntentsAmbiguous) == 0
}

// ErrExpectedRowsMismatch is returned when the typed confirmation disagrees with
// the plan. It is deliberately not wrapped in anything retryable: the operator
// re-reads the plan.
type ErrExpectedRowsMismatch struct {
	Expected, Planned int64
}

func (e ErrExpectedRowsMismatch) Error() string {
	return fmt.Sprintf("undo refused: --expect-rows=%d does not match the %d row(s) this run would restore. "+
		"Re-read the plan (`openrails undo-run --merchant … --run …` with no --apply) and confirm the number it prints",
		e.Expected, e.Planned)
}

// PlanUndoRun computes the reversal WITHOUT mutating anything. Must run
// merchant-scoped; the run's own merchant is the only one visible.
func PlanUndoRun(ctx context.Context, database *db.DB, runID uuid.UUID) (UndoPlan, error) {
	mid, err := requireMerchantUUID(ctx)
	if err != nil {
		return UndoPlan{}, err
	}
	q := database.Gen(ctx)

	run, err := q.GetDestructiveRun(ctx, gen.GetDestructiveRunParams{MerchantID: mid, ID: runID})
	if err != nil {
		if isNoRows(err) {
			return UndoPlan{}, fmt.Errorf("destructive run %s not found for this merchant", runID)
		}
		return UndoPlan{}, fmt.Errorf("load destructive run %s: %w", runID, err)
	}
	plan := UndoPlan{
		RunID: runID, Kind: run.Kind, Status: run.Status, Actor: models.DerefStr(run.Actor),
		StartedAt:  run.StartedAt.UTC(),
		Scope:      UndoScope{MerchantID: mid, PspID: run.PspID, PspScoped: run.PspID != nil},
		Restorable: map[string]int64{},
	}
	if err := classifyRunKind(run.Kind); err != nil {
		return plan, err
	}
	if run.Status == "reversed" {
		return plan, fmt.Errorf("destructive run %s was already reversed", runID)
	}

	switch run.Kind {
	case DestructiveRunKindPrune:
		c, err := q.CountPruneRestorableForRun(ctx, gen.CountPruneRestorableForRunParams{MerchantID: mid, RunID: runID})
		if err != nil {
			return plan, fmt.Errorf("count prune-restorable rows: %w", err)
		}
		plan.Restorable["subscriptions"] = c.Subscriptions
		plan.Restorable["payments"] = c.Payments
		plan.Restorable["checkout_attempts"] = c.CheckoutAttempts
		plan.Restorable["product_access"] = c.ProductAccess
	case DestructiveRunKindConvergeEnforce:
		c, err := q.CountConvergeRestorableForRun(ctx, gen.CountConvergeRestorableForRunParams{MerchantID: mid, RunID: runID})
		if err != nil {
			return plan, fmt.Errorf("count converge-restorable rows: %w", err)
		}
		plan.Restorable["subscriptions"] = c.Subscriptions
		plan.AccessToInvalidate = c.AccessToInvalidate
		plan.SubscriptionsTombstoned, plan.SubscriptionsChanged = c.SubscriptionsTombstoned, c.SubscriptionsChanged
	}

	manifest, err := q.ListProviderIntentsForRun(ctx, gen.ListProviderIntentsForRunParams{MerchantID: mid, RunID: runID})
	if err != nil {
		return plan, fmt.Errorf("read intent manifest: %w", err)
	}
	for i := range manifest {
		m := &manifest[i]
		switch m.Status {
		case "pending", "failed_retryable":
			plan.IntentsUnfired++
		case "succeeded":
			plan.IntentsIrreversible = append(plan.IntentsIrreversible, divergenceOf(m, false))
		case "in_flight", "unknown_needs_verify":
			plan.IntentsAmbiguous = append(plan.IntentsAmbiguous, divergenceOf(m, true))
		}
	}

	unattributed, err := q.CountUnattributedProviderRows(ctx, mid)
	if err != nil {
		return plan, fmt.Errorf("assert PSP attribution invariant: %w", err)
	}
	plan.Unattributed = UnattributedRows{
		Subscriptions: unattributed.Subscriptions, Payments: unattributed.Payments,
		CheckoutAttempts: unattributed.CheckoutAttempts, PaymentMethods: unattributed.PaymentMethods,
		UnfiredIntents: unattributed.UnfiredIntents,
	}
	if n := plan.Unattributed.Total(); n > 0 {
		return plan, fmt.Errorf("%w: %d live row(s) for merchant %s", ErrUnattributedRows, n, mid)
	}
	return plan, nil
}

// UndoRun reverses one destructive run of any reversible kind. expectRows is
// the operator's typed confirmation and must equal the plan's restorable total;
// the plan is recomputed here so a stale number cannot satisfy the gate. A
// rollback is not a complete operation (`rollback → pull → converge` is):
// recompute closes the derive half here; the provider pull is the operator's
// next step and runs advisory until enforcement is re-armed by hand.
func UndoRun(ctx context.Context, database *db.DB, runID uuid.UUID, actor string, expectRows int64, recompute Recomputer) (UndoResult, error) {
	plan, err := PlanUndoRun(ctx, database, runID)
	if err != nil {
		return UndoResult{Plan: plan}, err
	}
	if planned := plan.ExpectedRows(); expectRows != planned {
		return UndoResult{Plan: plan}, ErrExpectedRowsMismatch{Expected: expectRows, Planned: planned}
	}

	res := UndoResult{Plan: plan, Restored: map[string]int64{}}
	switch plan.Kind {
	case DestructiveRunKindPrune:
		// Prune's reverse restores ROWS. It has no intents of its own to
		// supersede (a prune queues no provider write) and nothing derived to
		// invalidate: the entitlements it tombstoned come back with their
		// subscriptions.
		r, err := RollbackDestructiveRun(ctx, database, runID, actor)
		if err != nil {
			return res, err
		}
		res.Restored["subscriptions"] = r.Subscriptions
		res.Restored["payments"] = r.Payments
		res.Restored["checkout_attempts"] = r.CheckoutAttempts
		res.Restored["product_access"] = r.ProductAccess
	case DestructiveRunKindConvergeEnforce:
		r, err := RollbackConvergeEnforceRun(ctx, database, runID, actor, recompute)
		if err != nil {
			return res, err
		}
		res.Restored["subscriptions"] = r.SubscriptionsRestored
		res.AccessInvalidated = r.AccessInvalidated
		res.IntentsSuperseded = r.IntentsSuperseded
		res.IntentsIrreversible = r.IntentsIrreversible
		res.IntentsAmbiguous = r.IntentsAmbiguous
		res.ProvenDomainsReset = r.ProvenDomainsReset
		res.EnforcementDisarmed = r.EnforcementDisarmed
		res.Recomputed = r.Recomputed
	}
	return res, nil
}

func divergenceOf(m *gen.ListProviderIntentsForRunRow, ambiguous bool) IntentDivergence {
	d := IntentDivergence{
		IntentID: m.ID, IntentType: m.IntentType, Rail: m.Rail,
		SubscriptionID: m.SubscriptionID, Status: m.Status, ExecutedAt: m.ExecutedAt,
	}
	if ambiguous {
		d.Consequence = "may already have reached the provider (" + irreversibleConsequence(m.IntentType) + "); the verifier resolves it — treat as irreversible until it does"
	} else {
		d.Consequence = irreversibleConsequence(m.IntentType)
	}
	return d
}
