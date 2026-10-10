package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// DestructiveRunKindPrune is this run kind's key in billing.maintenance_runs.
const DestructiveRunKindPrune = "prune"

// PruneParams bounds a pull-provider --prune pass.
type PruneParams struct {
	Since time.Time
	Until time.Time
	// Apply=false is dry-run: discover + count, write nothing.
	Apply bool
	// ExpectedRows is the operator's typed confirmation — how many rows they
	// believe the pass will remove. REQUIRED with Apply: a bare boolean
	// authorises a number the operator never saw. A mismatch refuses before
	// anything is written.
	ExpectedRows *int
	// Actor is who asked for it; recorded on the run.
	Actor string
}

// PruneResult tallies one PSP's prune pass.
type PruneResult struct {
	// RunID is the maintenance_runs id when Apply wrote anything — the handle
	// `openrails undo-run --run <id>` reverses.
	RunID                  uuid.UUID
	Subscriptions          int // soft-deleted (Apply) or would-delete (dry-run)
	SubscriptionsSkipped   int // excess but entangled with the grant ledger
	Payments               int
	PaymentsSkipped        int // excess but with protected dependents
	CheckoutAttempts       int // dependents soft-deleted with their subscription
	ProductAccess          int
	SubscriptionIDs        []uuid.UUID
	SkippedSubscriptionIDs []uuid.UUID
	PaymentIDs             []uuid.UUID
	SkippedPaymentIDs      []uuid.UUID
	SubscriptionSkipReason string
	PaymentSkipReason      string
}

// ErrPruneEmptyRemoteSet: a snapshot claimed prunable coverage but listed
// nothing, while local account-bound rows exist. An empty remote set is
// indistinguishable from a credential pointed at the wrong gateway account or a
// provider incident, so it never means "everything here is excess".
type ErrPruneEmptyRemoteSet struct {
	Domain    string
	LocalRows int
	AccountID string
	Provider  Provider
}

func (e *ErrPruneEmptyRemoteSet) Error() string {
	return fmt.Sprintf(
		"refusing to prune %s for %s account %s: the snapshot claimed complete coverage but listed ZERO %s while %d local rows are attributed to that account. "+
			"An empty remote set is indistinguishable from a misdeclared account_id, a credential rotated onto a sibling sub-account, or a provider incident — it never means 'prune everything'. "+
			"Re-run the pull and confirm the roster before pruning",
		e.Domain, e.Provider, e.AccountID, e.Domain, e.LocalRows)
}

// ErrPruneCountMismatch: --apply's typed expected row count disagrees with what
// the pass discovered. Nothing is written.
type ErrPruneCountMismatch struct {
	Expected int
	Found    int
}

func (e *ErrPruneCountMismatch) Error() string {
	return fmt.Sprintf("refusing to prune: --expect-rows says %d, this pass found %d. Re-run without --apply to review the plan, then confirm the number it reports",
		e.Expected, e.Found)
}

// PrunePSPExcess fetches the bound account's current snapshot and prunes local
// mirror rows attributed to that PSP that are absent from it. It is
// account-bound and fails closed (a row whose PSP this pass did not pull is out
// of scope, never "maybe ours"):
//
//   - Nothing is deleted: eligible rows are soft-deleted (deleted_at) and
//     stamped with a maintenance_runs id, so `openrails undo-run --run <id>`
//     reverses the pass.
//   - An empty remote set refuses, in the SQL and here, rather than matching
//     everything.
//   - --apply requires a typed expected row count that must match.
//   - A subscription that fed the grant ledger is skipped (removing it would
//     orphan an append-only grant; convergence revokes instead), as is a
//     payment with protected dependents (grant, refund, entitlement grant,
//     checkout attempt).
//
// Dry-run writes nothing. Must run merchant-scoped.
func PrunePSPExcess(ctx context.Context, database *db.DB, fetcher RailFetcher, provider Provider, binding PSPBinding, params PruneParams) (PruneResult, error) {
	var res PruneResult
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return res, err
	}
	mid := merchantID.UUID()

	snap, err := fetcher.Fetch(ctx, FetchParams{Since: params.Since, Until: params.Until, PspID: binding.ID.String()})
	if err != nil {
		return res, fmt.Errorf("fetch snapshot: %w", err)
	}
	if snap == nil {
		return res, fmt.Errorf("provider %s returned a nil snapshot", provider)
	}

	presentSubs := make([]string, 0, len(snap.Subscriptions))
	for i := range snap.Subscriptions {
		if id := snap.Subscriptions[i].RailSubscriptionID; id != "" {
			presentSubs = append(presentSubs, id)
		}
	}
	presentTxns := make([]string, 0, len(snap.Transactions))
	for i := range snap.Transactions {
		if id := snap.Transactions[i].TransactionID; id != "" {
			presentTxns = append(presentTxns, id)
		}
	}

	q := database.Gen(ctx)

	var since, until *time.Time
	if !params.Since.IsZero() {
		s := params.Since
		since = &s
	}
	if !params.Until.IsZero() {
		u := params.Until
		until = &u
	}

	// --- discovery (writes nothing) ---
	var excessSubs, excessPays []uuid.UUID

	// Subscriptions: head state — a roster lists every live sub.
	if snap.Coverage.CanPruneSubscriptions() {
		if len(presentSubs) == 0 {
			candidates, cerr := q.ListPSPSubscriptionCandidates(ctx, gen.ListPSPSubscriptionCandidatesParams{MerchantID: mid, PspID: binding.ID})
			if cerr != nil {
				return res, fmt.Errorf("list subscription prune candidates: %w", cerr)
			}
			if len(candidates) > 0 {
				return res, &ErrPruneEmptyRemoteSet{Domain: "subscriptions", LocalRows: len(candidates), AccountID: binding.AccountID, Provider: provider}
			}
		} else if excessSubs, err = q.ListExcessSubscriptionsForPSP(ctx, gen.ListExcessSubscriptionsForPSPParams{
			MerchantID: mid, PspID: binding.ID, PresentIds: presentSubs,
		}); err != nil {
			return res, fmt.Errorf("list excess subscriptions: %w", err)
		}
	} else {
		skipped, serr := q.ListPSPSubscriptionCandidates(ctx, gen.ListPSPSubscriptionCandidatesParams{MerchantID: mid, PspID: binding.ID})
		if serr != nil {
			return res, fmt.Errorf("list subscription prune skips: %w", serr)
		}
		res.SubscriptionsSkipped = len(skipped)
		res.SkippedSubscriptionIDs = append(res.SkippedSubscriptionIDs, skipped...)
		res.SubscriptionSkipReason = "snapshot_not_complete"
	}

	// Payments: windowed — a snapshot only proves absence inside its window.
	if canPruneTransactionWindow(snap.Coverage, params.Since, params.Until) {
		if len(presentTxns) == 0 {
			candidates, cerr := q.ListPSPPaymentCandidates(ctx, gen.ListPSPPaymentCandidatesParams{MerchantID: mid, PspID: binding.ID, Since: since, Until: until})
			if cerr != nil {
				return res, fmt.Errorf("list payment prune candidates: %w", cerr)
			}
			if len(candidates) > 0 {
				return res, &ErrPruneEmptyRemoteSet{Domain: "payments", LocalRows: len(candidates), AccountID: binding.AccountID, Provider: provider}
			}
		} else if excessPays, err = q.ListExcessPaymentsForPSP(ctx, gen.ListExcessPaymentsForPSPParams{
			MerchantID: mid, PspID: binding.ID, PresentTxns: presentTxns, Since: since, Until: until,
		}); err != nil {
			return res, fmt.Errorf("list excess payments: %w", err)
		}
	} else {
		skipped, serr := q.ListPSPPaymentCandidates(ctx, gen.ListPSPPaymentCandidatesParams{MerchantID: mid, PspID: binding.ID, Since: since, Until: until})
		if serr != nil {
			return res, fmt.Errorf("list payment prune skips: %w", serr)
		}
		res.PaymentsSkipped = len(skipped)
		res.SkippedPaymentIDs = append(res.SkippedPaymentIDs, skipped...)
		res.PaymentSkipReason = "snapshot_not_complete"
	}

	// Partition excess into eligible vs entangled BEFORE any write, so the
	// operator's typed count is checked against the number actually at risk.
	var eligibleSubs []uuid.UUID
	for _, subID := range excessSubs {
		entangled, gerr := q.SubscriptionHasGrant(ctx, gen.SubscriptionHasGrantParams{MerchantID: mid, SubscriptionID: subID})
		if gerr != nil {
			return res, fmt.Errorf("subscription grant check %s: %w", subID, gerr)
		}
		if entangled {
			res.SubscriptionsSkipped++
			res.SkippedSubscriptionIDs = append(res.SkippedSubscriptionIDs, subID)
			continue
		}
		eligibleSubs = append(eligibleSubs, subID)
	}
	var eligiblePays []uuid.UUID
	for _, payID := range excessPays {
		protected, perr := q.PaymentHasProtectedDependents(ctx, gen.PaymentHasProtectedDependentsParams{MerchantID: mid, PaymentID: payID})
		if perr != nil {
			return res, fmt.Errorf("payment dependent check %s: %w", payID, perr)
		}
		if protected != nil && *protected {
			res.PaymentsSkipped++
			res.SkippedPaymentIDs = append(res.SkippedPaymentIDs, payID)
			continue
		}
		eligiblePays = append(eligiblePays, payID)
	}

	res.Subscriptions, res.SubscriptionIDs = len(eligibleSubs), eligibleSubs
	res.Payments, res.PaymentIDs = len(eligiblePays), eligiblePays

	if !params.Apply {
		return res, nil
	}

	// --- typed confirmation ---
	found := len(eligibleSubs) + len(eligiblePays)
	if params.ExpectedRows == nil {
		return res, fmt.Errorf("refusing to prune: --apply requires --expect-rows. This pass found %d rows to prune; review the dry-run plan, then re-run with --expect-rows %d", found, found)
	}
	if *params.ExpectedRows != found {
		return res, &ErrPruneCountMismatch{Expected: *params.ExpectedRows, Found: found}
	}
	if found == 0 {
		return res, nil
	}

	// --- open the run BEFORE writing, so a crash mid-pass stays reversible ---
	runID := uuid.New()
	coverage, err := json.Marshal(snap.Coverage)
	if err != nil {
		return res, fmt.Errorf("marshal coverage proof: %w", err)
	}
	actor := params.Actor
	if actor == "" {
		actor = "unknown"
	}
	pspID := binding.ID
	expected := int64(found)
	note := fmt.Sprintf("pull-provider --prune %s account %s", provider, binding.AccountID)
	if _, err := q.CreateDestructiveRun(ctx, gen.CreateDestructiveRunParams{
		ID: runID, MerchantID: mid, PspID: &pspID, Kind: DestructiveRunKindPrune,
		Actor: actor, DryRun: false, Coverage: coverage, ExpectedRows: &expected, Note: &note,
	}); err != nil {
		return res, fmt.Errorf("open destructive run: %w", err)
	}
	res.RunID = runID

	now := time.Now().UTC()
	for _, subID := range eligibleSubs {
		// Soft-delete the dependent mirror rows then the subscription atomically,
		// all stamped with the run so the rollback restores them together.
		if err := database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			tq := gen.New(tx)
			cs, e := tq.PruneSoftDeleteCheckoutAttemptsBySubscription(ctx, gen.PruneSoftDeleteCheckoutAttemptsBySubscriptionParams{MerchantID: mid, SubscriptionID: subID, Now: now, RunID: runID})
			if e != nil {
				return e
			}
			ent, e := tq.PruneSoftDeleteAccessBySubscription(ctx, gen.PruneSoftDeleteAccessBySubscriptionParams{MerchantID: mid, SubscriptionID: subID, Now: now, RunID: runID})
			if e != nil {
				return e
			}
			if _, e := tq.PruneSoftDeleteSubscriptionByID(ctx, gen.PruneSoftDeleteSubscriptionByIDParams{MerchantID: mid, ID: subID, Now: now, RunID: runID}); e != nil {
				return e
			}
			res.CheckoutAttempts += int(cs)
			res.ProductAccess += int(ent)
			return nil
		}); err != nil {
			return res, finishRunFailed(ctx, q, mid, runID, res, fmt.Errorf("prune excess subscription %s: %w", subID, err))
		}
	}
	for _, payID := range eligiblePays {
		if _, e := q.PruneSoftDeletePaymentByID(ctx, gen.PruneSoftDeletePaymentByIDParams{MerchantID: mid, ID: payID, Now: now, RunID: runID}); e != nil {
			return res, finishRunFailed(ctx, q, mid, runID, res, fmt.Errorf("prune excess payment %s: %w", payID, e))
		}
	}

	if _, err := q.FinishDestructiveRun(ctx, gen.FinishDestructiveRunParams{
		MerchantID: mid, ID: runID, Status: "completed", Now: time.Now().UTC(), Affected: affectedJSON(res),
	}); err != nil {
		return res, fmt.Errorf("close destructive run %s: %w", runID, err)
	}
	return res, nil
}

func affectedJSON(res PruneResult) []byte {
	b, _ := json.Marshal(map[string]int{
		"subscriptions":     res.Subscriptions,
		"payments":          res.Payments,
		"checkout_attempts": res.CheckoutAttempts,
		"product_access":    res.ProductAccess,
	})
	return b
}

// finishRunFailed marks a partially-applied run failed and returns the original
// error. Rows it already stamped stay stamped — that is exactly what makes the
// partial damage reversible by run id.
func finishRunFailed(ctx context.Context, q *gen.Queries, merchantID, runID uuid.UUID, res PruneResult, cause error) error {
	if _, err := q.FinishDestructiveRun(ctx, gen.FinishDestructiveRunParams{
		MerchantID: merchantID, ID: runID, Status: "failed", Now: time.Now().UTC(), Affected: affectedJSON(res),
	}); err != nil {
		return fmt.Errorf("%w (and could not mark run %s failed: %v)", cause, runID, err)
	}
	return fmt.Errorf("%w — run %s is reversible with `openrails undo-run --run %s`", cause, runID, runID)
}

// RollbackResult tallies one reversal.
type RollbackResult struct {
	RunID            uuid.UUID
	Subscriptions    int64
	Payments         int64
	CheckoutAttempts int64
	ProductAccess    int64
}

// RollbackDestructiveRun reverses one prune run by id: every row that run
// soft-deleted has its deleted_at and stamp cleared, in one transaction. Only
// rows carrying this run's id are restored, so an unrelated soft delete is
// never resurrected. A unique or exclusion conflict (the provider re-created
// what the prune removed) aborts the whole rollback rather than half-restoring.
// A rollback is not a complete operation: `rollback → pull → converge` is.
// Must run merchant-scoped.
func RollbackDestructiveRun(ctx context.Context, database *db.DB, runID uuid.UUID, actor string) (RollbackResult, error) {
	var res RollbackResult
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return res, err
	}
	mid := merchantID.UUID()
	res.RunID = runID

	run, err := database.Gen(ctx).GetDestructiveRun(ctx, gen.GetDestructiveRunParams{MerchantID: mid, ID: runID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return res, fmt.Errorf("destructive run %s not found for this merchant", runID)
		}
		return res, fmt.Errorf("load destructive run %s: %w", runID, err)
	}
	if run.Status == "reversed" {
		return res, fmt.Errorf("destructive run %s was already reversed", runID)
	}
	// This reverse only clears soft-delete tombstones. A converge-enforce run
	// overwrote row values, so running this against one would restore nothing,
	// leave its queued provider writes free to fire, and still mark the run
	// reversed.
	if run.Kind != DestructiveRunKindPrune {
		return res, fmt.Errorf("destructive run %s is kind %q; this reverse only handles %q runs. Use `openrails undo-run --run %s`, which dispatches on kind",
			runID, run.Kind, DestructiveRunKindPrune, runID)
	}
	if actor == "" {
		actor = "unknown"
	}

	if err := database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tq := gen.New(tx)
		now := time.Now().UTC()
		var e error
		if res.Subscriptions, e = tq.RestoreSubscriptionsByDestructiveRun(ctx, gen.RestoreSubscriptionsByDestructiveRunParams{MerchantID: mid, RunID: runID, Now: now}); e != nil {
			return fmt.Errorf("restore subscriptions: %w", e)
		}
		if res.Payments, e = tq.RestorePaymentsByDestructiveRun(ctx, gen.RestorePaymentsByDestructiveRunParams{MerchantID: mid, RunID: runID}); e != nil {
			return fmt.Errorf("restore payments: %w", e)
		}
		if res.CheckoutAttempts, e = tq.RestoreCheckoutAttemptsByDestructiveRun(ctx, gen.RestoreCheckoutAttemptsByDestructiveRunParams{MerchantID: mid, RunID: runID, Now: now}); e != nil {
			return fmt.Errorf("restore checkout attempts: %w", e)
		}
		if res.ProductAccess, e = tq.RestoreAccessByDestructiveRun(ctx, gen.RestoreAccessByDestructiveRunParams{MerchantID: mid, RunID: runID, Now: now}); e != nil {
			return fmt.Errorf("restore entitlements: %w", e)
		}
		if _, e = tq.MarkDestructiveRunReversed(ctx, gen.MarkDestructiveRunReversedParams{MerchantID: mid, ID: runID, Now: now, ReversedBy: actor}); e != nil {
			return fmt.Errorf("mark run reversed: %w", e)
		}
		return nil
	}); err != nil {
		return RollbackResult{RunID: runID}, err
	}
	return res, nil
}

func canPruneTransactionWindow(coverage SnapshotCoverage, since, until time.Time) bool {
	if !coverage.CanPruneTransactions() {
		return false
	}
	return sameCoverageTime(coverage.TransactionWindowSince, since) &&
		sameCoverageTime(coverage.TransactionWindowUntil, until)
}

func sameCoverageTime(got *time.Time, want time.Time) bool {
	if got == nil || want.IsZero() {
		return false
	}
	return got.UTC().Equal(want.UTC())
}
