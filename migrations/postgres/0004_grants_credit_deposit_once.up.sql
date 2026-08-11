-- or#906: deposit once-only becomes a DATABASE fact.
--
-- The deposit's structural key is the credit grant's (merchant, customer,
-- source_id) — the exact coordinate depositTx has always deduped on, but only
-- by check-then-insert under lockBalance (the shape or#892 condemned), backed
-- by the NON-unique idx_grants_source. The or#892 ledger unique cannot help
-- here: the deposit's ledger leg is keyed on the GRANT id, not the caller's
-- coordinate, so a double-inserted grant produces two "distinct" ledger
-- deposits. This index closes that: a new deposit path that forgets the lock
-- still cannot double-credit (the LED-14 argument, applied to the lot ledger).
--
-- source_type is deliberately NOT in the key (doctrine, restated by or#906
-- rather than "fixed": the same SourceID under a different Source label is
-- still the same deposit — a retry that relabels its source must not become a
-- second credit). event='grant' only: revoke/expire/supersede events reuse the
-- grant's source_id by construction and are terminations, not deposits.
-- source_id <> '' matches the write path: a blank source_id never enters the
-- dedupe lookup (and pkg/service refuses a keyless deposit outright, or#891).
--
-- Pre-existing duplicates: CHECK AND FAIL, never dedupe. grants is an
-- append-only money ledger; a duplicate at this key is a double-credit that
-- ALREADY happened, and a migration silently deleting or merging lots would
-- both rewrite ledger history and decide on its own which credit "counts".
-- That is an operator reconciliation (revoke the surplus lot through the grant
-- ledger), so the migration names the offenders and refuses instead of leaving
-- CREATE UNIQUE INDEX's opaque one-row error.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DO $$
DECLARE
    dup record;
    detail text := '';
    n integer := 0;
BEGIN
    FOR dup IN
        SELECT merchant_id, customer_id, source_id, count(*) AS copies, sum(amount) AS total_amount
        FROM openrails.grants
        WHERE kind = 'credit' AND event = 'grant' AND source_id <> ''
        GROUP BY merchant_id, customer_id, source_id
        HAVING count(*) > 1
        ORDER BY count(*) DESC, merchant_id, customer_id, source_id
        LIMIT 20
    LOOP
        n := n + 1;
        detail := detail || format(E'\n  merchant=%s customer=%s source_id=%s copies=%s total_amount=%s',
            dup.merchant_id, dup.customer_id, dup.source_id, dup.copies, dup.total_amount);
    END LOOP;
    IF n > 0 THEN
        RAISE EXCEPTION USING
            MESSAGE = 'or#906: refusing to install uq_grants_credit_deposit_once — duplicate credit grants exist at the deposit key (merchant, customer, source_id); first 20:' || detail,
            HINT = 'Each duplicate is a double-credit that already happened. Reconcile by hand (revoke the surplus lot through the grant ledger), then re-apply. A migration must not decide which credit counts.';
    END IF;
END $$;

CREATE UNIQUE INDEX uq_grants_credit_deposit_once
    ON openrails.grants USING btree (merchant_id, customer_id, source_id)
    WHERE ((kind = 'credit'::text) AND (event = 'grant'::text) AND (source_id <> ''::text));

COMMENT ON INDEX openrails.uq_grants_credit_deposit_once IS 'LED-17 (or#906): a deposit happens at most once at the caller''s key (merchant, customer, source_id). Merchant-led (ID-11). source_type is NOT part of the key — the same source_id under a different source label is the same deposit. Once-only is a database fact, not a consequence of depositTx''s lockBalance serialization.';
