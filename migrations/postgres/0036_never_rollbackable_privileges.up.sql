-- or#859 phase 1: close the doctrine-only Class A gaps.
--
-- The never-rollbackable register (internal/reconcile/never_rollbackable.go,
-- docs/invariants.md §11) names the tables no rollback may restore at any scope.
-- For most of them the rule is already enforced by GRANT: ledger_transfers,
-- ledger_accounts, grants and subscription_status_transitions are
-- SELECT,INSERT-only, so openrails_app physically cannot rewrite history.
--
-- Two were doctrine only. Both are the record of what we did — the manifest a
-- rollback READS to report irreversible divergence — and a table an undo reads
-- for truth must not be editable by the same role that performs the undo.
--
--   rail_mutation_logs  had UPDATE. Nothing in the application updates it: the
--                       only writes are the INSERT in rail_mutation_logs.sql and
--                       the merchant purge's whole-merchant DELETE, which stays.
--   reconciliation_runs had DELETE. Nothing in the application deletes it: the
--                       run header is INSERTed at start and UPDATEd at finish,
--                       and it is not in the merchant purge inventory.
--
-- Deliberately NOT revoked: DELETE on webhook_events. It looks like the same
-- shape but the retention sweep (DeleteCompletedWebhookEventsBefore) needs it,
-- and that is a legitimate, bounded expiry rather than an edit of history. The
-- consequence is recorded instead: a destructive run stops being safely
-- reversible once its window reaches past webhook_events retention, because a
-- rollback that outlives the dedup record makes re-processing possible again.
SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

REVOKE UPDATE ON TABLE openrails.rail_mutation_logs FROM openrails_app;
REVOKE DELETE ON TABLE openrails.reconciliation_runs FROM openrails_app;

COMMENT ON TABLE openrails.rail_mutation_logs IS
    'Append-only operator history for external provider mutations executed from provider intents/convergence (#533). or#859 Class A: the record of what we did to the outside world — INSERT plus the whole-merchant purge DELETE only, never UPDATE, and never rolled back.';
COMMENT ON TABLE openrails.reconciliation_runs IS
    'One row per manual reconcile run (#107): advisory diffs or enforce convergence against the payment rails. Summary jsonb carries per-rail counts and the dunning-forensics report. or#859 Class A forensics: INSERT at start, UPDATE at finish, never DELETE — a rollback that erases the evidence of what went wrong defeats itself.';

-- The reversal is one verb now (`openrails undo-run`), kind-dispatched over this
-- ledger, so the schema comments that named the per-kind verbs are corrected
-- here rather than left to drift into the generated models.
COMMENT ON TABLE openrails.destructive_runs IS 'or#858/or#859 tier 1: every destructive operation is an attributable, scoped, stamped unit of damage with a single-command undo. kind=prune stamps rows it soft-deleted (destructive_run_id on the row); kind=converge_enforce captures before-images of the rows it OVERWROTE plus the provider intents it queued. Both reverse with `openrails undo-run --run <id>`, which dispatches on kind, plans before it applies, and refuses a kind it cannot reverse. declared_import / plan_migration / catalog_push are declared and not yet converted; merchant_delete is registered as unrecoverable (it hard-DELETEs Class A rows).';
COMMENT ON COLUMN openrails.subscriptions.deleted_at IS 'or#858 soft delete: set, the row is invisible to every live read. Only `pull-provider --prune` sets it, and `openrails undo-run` clears it.';
COMMENT ON COLUMN openrails.payments.deleted_at IS 'or#858 soft delete: set, the row is invisible to every live read. Only `pull-provider --prune` sets it, and `openrails undo-run` clears it.';
COMMENT ON COLUMN openrails.checkout_sessions.deleted_at IS 'or#858 soft delete: set, the row is invisible to every live read. Only `pull-provider --prune` sets it, and `openrails undo-run` clears it.';
