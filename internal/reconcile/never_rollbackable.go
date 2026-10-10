package reconcile

import "fmt"

// NeverRollbackableTables lists the tables no rollback may restore, with why:
// the money spine, the grant log every derived effect is recomputed from, the
// lifecycle audit trail, the provider mutation record and webhook dedup. A
// rollback restores state wrongly destroyed; it never retracts value wrongly
// granted (that is a revoke, a compensating transfer and a refund).
var NeverRollbackableTables = map[string]string{
	"ledger_transfers": "money. Reversal is a compensating transfer, never a deletion — and any row-level write bypassing the counter trigger silently corrupts every balance read",
	"ledger_accounts":  "trigger-maintained balance projection; restoring a row desynchronises it from its transfers",
	"grants": "the authority every grant effect is RE-DERIVED from. Roll it back and derived state becomes unrecoverable — " +
		"this is the table that makes the whole design work (ID-8)",
	"subscription_status_transitions": "lifecycle audit trail; a reversal appends to it, never rewinds it",
	"provider_mutation_logs":          "the record of what we did to the outside world — the divergence manifest an undo READS, never edits",
	"webhook_events": "dedup truth (IDEM-11). Roll it back and every webhook after T becomes re-processable: duplicate grants, " +
		"duplicate charges, duplicate cancels",
	"reconciliation_findings": "forensics: a rollback that erases the evidence of what went wrong defeats itself",
	"price_key_movements":     "forensics: price-identity history",
}

// maintenance_runs and destructive_run_before_images are absent on purpose: an
// undo writes its own bookkeeping there, and guard_billing_fact_columns
// triggers refuse any rewrite of a run's identity or a captured image.

// provider_intents is absent too: an undo supersedes a queued intent
// (pending/failed_retryable -> superseded), a forward transition. Deleting one
// or rewriting one that executed stays forbidden; the supersede query's status
// predicate holds that line.
const providerIntentsForwardOnlyReason = "forward lifecycle transition only: pending/failed_retryable -> superseded. Never deleted, never rewritten once it has executed"

// UnrecoverableRunKinds are destructive-run kinds whose damage no local undo can
// reverse, mapped to what the operator must reach for instead. Refusing them by
// name beats attempting a reversal that would silently restore nothing.
var UnrecoverableRunKinds = map[string]string{
	"merchant_purge": "a merchant purge hard-DELETEs append-only Class A rows (grants, the ledger, the intent and mutation logs). " +
		"Nothing local restores them: recovery is tier 0 (cluster PITR) or a tier 2 snapshot taken before the purge",
}

// ReversibleRunKinds are the kinds an undo can reverse. A kind in neither map
// has no undo yet and is refused rather than marked reversed.
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
