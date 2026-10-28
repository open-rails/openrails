-- Repair: none-needed. Existing subscriptions read as provider facts, so a
-- later host book skips them (the safe direction). An older in-flight import
-- of a PSP that a newer one also pulls or applies is failed, so one import per
-- PSP is in flight. The record-state and change-kind checks only widen.

-- Whose facts a subscription keeps: a host's legacy book or the provider's.
ALTER TABLE billing.subscriptions ADD COLUMN facts_authority text NOT NULL DEFAULT 'provider'
    CONSTRAINT subscriptions_facts_authority_check CHECK (facts_authority IN ('host', 'provider'));
COMMENT ON COLUMN billing.subscriptions.facts_authority IS 'host: a host''s legacy book landed the row and may still converge it; provider: provider evidence owns it, and a host book skips it. Moves only from host to provider.';

CREATE FUNCTION billing.subscriptions_facts_authority_one_way() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.facts_authority = 'provider' AND NEW.facts_authority = 'host' THEN
        RAISE EXCEPTION 'subscription % facts authority moves only from host to provider', OLD.id
            USING ERRCODE = '23514', CONSTRAINT = 'subscriptions_facts_authority_one_way';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER subscriptions_facts_authority_one_way BEFORE UPDATE OF facts_authority ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_facts_authority_one_way();

-- Taking the provider's authority moves no fact a proposal's basis names, so
-- it alone keeps the row version.
CREATE OR REPLACE FUNCTION billing.subscriptions_row_version() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.facts_authority IS DISTINCT FROM OLD.facts_authority
       AND to_jsonb(NEW) - 'facts_authority' = to_jsonb(OLD) - 'facts_authority' THEN
        RETURN NEW;
    END IF;
    NEW.row_version := OLD.row_version + 1;
    RETURN NEW;
END;
$$;

-- The import whose pull last raised a proposal.
ALTER TABLE billing.reconciliation_findings
    ADD COLUMN pull_id uuid,
    ADD CONSTRAINT reconciliation_findings_pull_id_fkey FOREIGN KEY (merchant_id, pull_id)
        REFERENCES billing.psp_imports(merchant_id, id) ON DELETE SET NULL (pull_id),
    ADD CONSTRAINT reconciliation_findings_pull_id_check CHECK (pull_id IS NULL OR source = 'pull'),
    DROP CONSTRAINT reconciliation_findings_change_check,
    ADD CONSTRAINT reconciliation_findings_change_check CHECK (change IS NULL OR (
        finding_type LIKE 'pull.%' AND jsonb_typeof(change) = 'object'
        AND change->>'kind' IN ('create_subscription','record_charge','record_refund','adopt_subscription',
            'cancel_subscription','update_card','revoke_access','refund_charge_after_cancel',
            'cancel_provider_schedule','collect_skipped_renewal','correct_subscription')));
CREATE INDEX reconciliation_findings_pull_id_idx ON billing.reconciliation_findings (merchant_id, pull_id)
    WHERE pull_id IS NOT NULL;
COMMENT ON COLUMN billing.reconciliation_findings.pull_id IS 'The PSP import whose pull last raised this pull.* proposal; NULL when none did or the import was deleted.';

-- A record OpenRails already holds: its holder is the customer.
ALTER TABLE billing.psp_import_records
    DROP CONSTRAINT psp_import_records_state_check,
    ADD CONSTRAINT psp_import_records_state_check CHECK (
        state IN ('unmatched', 'conflict') AND customer_id IS NULL AND linked_via IS NULL
        OR state IN ('linked', 'imported', 'blocked') AND customer_id IS NOT NULL AND linked_via IS NOT NULL AND linked_via <> ''
        OR state = 'held' AND customer_id IS NOT NULL AND linked_via = 'held');

UPDATE billing.psp_imports i SET status = 'failed', error = 'superseded by a newer import of the same PSP', updated_at = now()
WHERE i.status IN ('pulling', 'applying') AND EXISTS (
    SELECT 1 FROM billing.psp_imports n WHERE n.merchant_id = i.merchant_id AND n.psp_id = i.psp_id
      AND n.status IN ('pulling', 'applying') AND (n.created_at, n.id) > (i.created_at, i.id));
ALTER TABLE billing.psp_imports
    DROP CONSTRAINT psp_imports_error_code_check,
    ADD CONSTRAINT psp_imports_error_code_check CHECK (error_code IS NULL OR status = 'failed'
        AND error_code IN ('psp_credentials_rejected', 'psp_not_readable', 'pull_roster_implausible')),
    ADD COLUMN apply_requested_by text CONSTRAINT psp_imports_apply_requested_by_check CHECK (apply_requested_by IS NULL OR apply_requested_by <> '');
CREATE UNIQUE INDEX psp_imports_in_flight_key ON billing.psp_imports (merchant_id, psp_id) WHERE status IN ('pulling', 'applying');
COMMENT ON COLUMN billing.psp_imports.error_code IS 'Why a failed import stopped when pulling it again would stop the same way: psp_credentials_rejected (the provider refused the credentials), psp_not_readable (the PSP, its rail''s import or a credential is not declared) or pull_roster_implausible (the NMI roster listed under a tenth of the PSP''s live subscriptions).';
COMMENT ON COLUMN billing.psp_imports.apply_requested_by IS 'The person who last asked to apply the import; its corrections are approved in their name.';

CREATE TABLE billing.psp_import_basis (
    merchant_id uuid NOT NULL,
    import_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    row_version bigint NOT NULL,
    CONSTRAINT psp_import_basis_pkey PRIMARY KEY (merchant_id, import_id, subscription_id),
    CONSTRAINT psp_import_basis_import_id_fkey FOREIGN KEY (merchant_id, import_id) REFERENCES billing.psp_imports(merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT psp_import_basis_subscription_id_fkey FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions(merchant_id, id) ON DELETE CASCADE
);
CREATE INDEX psp_import_basis_subscription_id_idx ON billing.psp_import_basis (merchant_id, subscription_id);
COMMENT ON TABLE billing.psp_import_basis IS 'Each subscription of the PSP an import found before it read the roster, at its row version: the import proposes a state change only for a row still at that version. Deleted with its import.';

-- openrails prune is the one command that soft-deletes these rows.
COMMENT ON COLUMN billing.subscriptions.deleted_at IS 'Soft delete: set, the row is invisible to every live read. Only `openrails prune` sets it, and `openrails undo-run` clears it.';
COMMENT ON COLUMN billing.payments.deleted_at IS 'Soft delete: set, the row is invisible to every live read. Only `openrails prune` sets it, and `openrails undo-run` clears it.';
COMMENT ON COLUMN billing.order_attempts.deleted_at IS 'Soft delete: set, the row is invisible to every live read. Only `openrails prune` sets it, and `openrails undo-run` clears it.';
