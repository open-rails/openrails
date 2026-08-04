package reconcile

import "fmt"

// or#859 §4: the never-rollbackable register, in code.
//
// The spec's Class A is a list of tables no rollback may restore at any scope by
// any tier — the money spine, the grant log that every derived effect is
// recomputed from, the lifecycle audit trail, the record of what we did to the
// outside world, and the dedup truth that stops a webhook being processed twice.
// Prose in a spec is not a guard, so the register lives here, three enforcement
// points read it, and the reasons travel with the names:
//
//   - TestNeverRollbackableRegisterIsEnforced scans the SQL of every query the
//     undo paths issue and fails if one writes a registered table;
//   - migration 0036 revokes the privileges the doctrine says nothing
//     legitimately uses, so the rule survives a query that never met this file;
//   - UndoRun refuses a run KIND whose damage is registered as unrecoverable
//     rather than reporting a reversal it cannot perform.
//
// The asymmetry that governs all of it: a rollback restores state wrongly
// DESTROYED; it can never retract value wrongly GRANTED. Retraction is a revoke
// event plus a compensating transfer plus an operator-authorised refund.
var NeverRollbackableTables = map[string]string{
	"ledger_transfers": "money. Reversal is a compensating transfer, never a deletion — and any row-level write bypassing the SECURITY DEFINER trigger silently corrupts every balance read (LED-5)",
	"ledger_accounts":  "trigger-maintained balance projection; restoring a row desynchronises it from its transfers",
	"grants": "the authority every grant effect is RE-DERIVED from. Roll it back and derived state becomes unrecoverable — " +
		"this is the table that makes the whole design work (ID-8)",
	"subscription_status_transitions": "lifecycle audit trail; a reversal appends to it, never rewinds it",
	"rail_mutation_logs":              "the record of what we did to the outside world — the divergence manifest an undo READS, never edits",
	"webhook_events": "dedup truth (IDEM-11). Roll it back and every webhook after T becomes re-processable: duplicate grants, " +
		"duplicate charges, duplicate cancels",
	"reconciliation_findings": "forensics: a rollback that erases the evidence of what went wrong defeats itself",
	"reconciliation_runs":     "forensics: the run ledger a finding is attributed to",
	"catalog_drift_events":    "forensics: provider-drift evidence",
	"merchant_exports":        "forensics: the export record merchant deletion is gated on",
	"price_key_movements":     "forensics: price-identity history",
}

// Two tables an undo legitimately writes and which are nonetheless append-only
// in substance — `destructive_runs` and `destructive_run_before_images` — are
// absent from the register on purpose. They are the undo's own bookkeeping
// (status/reversed_at, restored_at), and the schema already holds the line
// harder than a name list could: both carry COLUMN-level UPDATE grants, so
// openrails_app cannot rewrite a run's identity or a captured image even by
// accident. See migration 0018 and 0030.

// rail_intents is deliberately NOT in the register even though the spec lists it
// as Class A, and the distinction is the single most valuable thing tier 1 does:
// moving a queued row to `status='superseded'` is a FORWARD lifecycle transition
// on an existing status value, not a rollback of an append-only log. That is how
// an undo neutralises a provider write that has not fired yet. What stays
// forbidden is deleting the row or rewriting one that already executed, and the
// supersede query's `status IN ('pending','failed_retryable')` predicate is what
// holds that line.
const railIntentsForwardOnlyReason = "forward lifecycle transition only: pending/failed_retryable -> superseded. Never deleted, never rewritten once it has executed"

// UnrecoverableRunKinds are destructive-run kinds whose damage no local undo can
// reverse, mapped to what the operator must reach for instead. Refusing them by
// name beats attempting a reversal that would silently restore nothing.
var UnrecoverableRunKinds = map[string]string{
	"merchant_delete": "a merchant purge hard-DELETEs append-only Class A rows (grants, the ledger, the intent and mutation logs). " +
		"Nothing local restores them: recovery is tier 0 (cluster PITR) or a tier 2 snapshot taken before the purge",
}

// ReversibleRunKinds are the kinds an undo actually knows how to reverse. A kind
// outside both maps is declared in the schema but not yet converted, and saying
// so is more useful than a no-op that marks the run reversed.
var ReversibleRunKinds = map[string]string{
	DestructiveRunKindPrune:           "rows were soft-deleted with the run's stamp; the undo clears the tombstones",
	DestructiveRunKindConvergeEnforce: "row VALUES were overwritten; the undo re-asserts them from the captured before-images and supersedes the provider writes the run queued but has not sent",
}

// classifyRunKind decides what an undo may do with a run, by kind.
func classifyRunKind(kind string) error {
	if _, ok := ReversibleRunKinds[kind]; ok {
		return nil
	}
	if why, ok := UnrecoverableRunKinds[kind]; ok {
		return fmt.Errorf("destructive run kind %q is NOT reversible: %s", kind, why)
	}
	return fmt.Errorf("destructive run kind %q has no undo: it is declared in the run ledger but not yet converted to a reversible operation (or#859 phase 1). "+
		"Reversing it would restore nothing and still mark the run reversed", kind)
}
