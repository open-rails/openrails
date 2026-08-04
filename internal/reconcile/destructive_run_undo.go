package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// or#859 §5.2: ONE undo verb over the whole destructive-run ledger.
//
// `--prune` destroys rows and reverses by clearing tombstones; a converge-enforce
// pass destroys row VALUES and reverses from captured before-images. Those are
// different mechanisms, but an operator holding a run id in the middle of an
// incident should not have to know which — nor discover, after typing the wrong
// verb, that the command restored nothing and marked the run reversed anyway.
// So the kind is read from the ledger and dispatched on, and a kind with no undo
// is refused by name.
//
// Dry-run is the DEFAULT. Applying requires a typed row count that matches the
// plan, which is the same shape `--prune --expect-rows` already demands of the
// destructive direction: the confirmation gate belongs on the undo too, because
// an undo IS a mass mutation — of the merchant's live book, at the worst
// possible moment to be wrong.

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

// NullPSPBlindSpot is the coverage hole a PSP-scoped operation cannot see
// (or#859 §3.2, hole 1). Reported, never silently excluded.
type NullPSPBlindSpot struct {
	Subscriptions    int64 `json:"subscriptions"`
	Payments         int64 `json:"payments"`
	CheckoutSessions int64 `json:"checkout_sessions"`
	PaymentMethods   int64 `json:"payment_methods"`
	UnfiredIntents   int64 `json:"unfired_intents"`
}

// Total is how many live rows this merchant holds that no PSP-scoped predicate
// can reach.
func (b NullPSPBlindSpot) Total() int64 {
	return b.Subscriptions + b.Payments + b.CheckoutSessions + b.PaymentMethods + b.UnfiredIntents
}

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
	// EntitlementsToInvalidate are Class D rows the reverse will soft-delete so
	// Converge REBUILDS them from the grant log. Not restorations, so not part
	// of the expected count — but the operator is told.
	EntitlementsToInvalidate int64 `json:"entitlements_to_invalidate"`
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
	BlindSpot           NullPSPBlindSpot   `json:"null_psp_blind_spot"`
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
	Restored                map[string]int64   `json:"restored"`
	EntitlementsInvalidated int64              `json:"entitlements_invalidated"`
	IntentsSuperseded       int                `json:"intents_superseded"`
	IntentsIrreversible     []IntentDivergence `json:"intents_irreversible,omitempty"`
	IntentsAmbiguous        []IntentDivergence `json:"intents_ambiguous,omitempty"`
	ProvenDomainsReset      int64              `json:"proven_domains_reset"`
	EnforcementDisarmed     bool               `json:"enforcement_disarmed"`
	Recomputed              bool               `json:"recomputed"`
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
		RunID: runID, Kind: run.Kind, Status: run.Status, Actor: run.Actor,
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
		plan.Restorable["checkout_sessions"] = c.CheckoutSessions
		plan.Restorable["entitlements"] = c.Entitlements
	case DestructiveRunKindConvergeEnforce:
		c, err := q.CountConvergeRestorableForRun(ctx, gen.CountConvergeRestorableForRunParams{MerchantID: mid, RunID: runID})
		if err != nil {
			return plan, fmt.Errorf("count converge-restorable rows: %w", err)
		}
		plan.Restorable["subscriptions"] = c.Subscriptions
		plan.EntitlementsToInvalidate = c.EntitlementsToInvalidate
		plan.SubscriptionsTombstoned = c.SubscriptionsTombstoned
	}

	manifest, err := q.ListRailIntentsForRun(ctx, gen.ListRailIntentsForRunParams{MerchantID: mid, RunID: runID})
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

	blind, err := q.CountNullPSPBlindSpot(ctx, mid)
	if err != nil {
		return plan, fmt.Errorf("count NULL-psp blind spot: %w", err)
	}
	plan.BlindSpot = NullPSPBlindSpot{
		Subscriptions: blind.Subscriptions, Payments: blind.Payments,
		CheckoutSessions: blind.CheckoutSessions, PaymentMethods: blind.PaymentMethods,
		UnfiredIntents: blind.UnfiredIntents,
	}
	return plan, nil
}

// UndoRun reverses one destructive run of any reversible kind.
//
// expectRows is the operator's typed confirmation and must equal the plan's
// restorable total. The plan is recomputed here rather than passed in: a plan
// the operator read minutes ago is not evidence about the database now, and the
// gate is worth nothing if it can be satisfied by a stale number.
//
// A rollback is not a complete operation — `rollback → pull → converge` is
// (or#859 §2.1). recompute closes the derive half inside this call; the provider
// pull is the operator's next step and runs advisory until enforcement is
// re-armed by hand.
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
		res.Restored["checkout_sessions"] = r.CheckoutSessions
		res.Restored["entitlements"] = r.Entitlements
	case DestructiveRunKindConvergeEnforce:
		r, err := RollbackConvergeEnforceRun(ctx, database, runID, actor, recompute)
		if err != nil {
			return res, err
		}
		res.Restored["subscriptions"] = r.SubscriptionsRestored
		res.EntitlementsInvalidated = r.EntitlementsInvalidated
		res.IntentsSuperseded = r.IntentsSuperseded
		res.IntentsIrreversible = r.IntentsIrreversible
		res.IntentsAmbiguous = r.IntentsAmbiguous
		res.ProvenDomainsReset = r.ProvenDomainsReset
		res.EnforcementDisarmed = r.EnforcementDisarmed
		res.Recomputed = r.Recomputed
	}
	return res, nil
}

func divergenceOf(m *gen.ListRailIntentsForRunRow, ambiguous bool) IntentDivergence {
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

// MarshalCoverage renders a run's stored coverage proof for display. A run is
// only as trustworthy as the proof that authorised it, so the undo surfaces it
// rather than leaving it in the table.
func MarshalCoverage(raw []byte) string {
	if len(raw) == 0 {
		return "(none recorded)"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(out)
}
