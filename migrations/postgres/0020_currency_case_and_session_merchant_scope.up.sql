-- GAP-10 (SEC-24 item 4) + GAP-6 / or#832. Two schema-level invariants that
-- were previously convention.
--
-- 1. checkout_sessions held the only CROSS-MERCHANT unique indexes in the
--    schema: (rail, reference) and (rail, transaction_id) with no merchant_id.
--    Every equivalent (uq_payments_*, uq_subscriptions_*) leads with
--    merchant_id. Under RLS the conflicting row is invisible, so one merchant's
--    session surfaces in another merchant's tenant as an opaque insert failure
--    — a cross-tenant existence oracle and a claim-squat.
--
--    Shape follows 0017: a provider reference/transaction id is only unique
--    WITHIN a gateway account, so the index carries the PSP dimension too, with
--    the nullable rail_merchant_account_id COALESCE'd to the nil uuid so ONE
--    TOTAL index both keeps per-PSP scope and leaves no disjoint-partial hole
--    for an unstamped legacy row to slip through.
--
-- 2. Currency codes were app-validated only. CUR-5 recorded "there is
--    deliberately no DB CHECK" and the drift it permitted was measured: a dev
--    DB's payments.currency was 100% lowercase, because the catalog manifest
--    loader lower-cased on the way in and nothing normalised on the way out.
--    A CHECK is chosen over boundary normalisation because the write boundary
--    is not one place — 17 currency columns are written from dozens of INSERTs
--    across modules, workers and importers, and any normaliser would be one
--    more thing to remember. The CHECK is the only point every write passes.
--
--    It constrains CASE and rough SHAPE, not membership: an UPPER-case code, or
--    a qualified custom-credit unit `slug/name` (#475). Membership stays in the
--    Go registry (money.ValidateCurrency), which is where per-currency scale
--    lives and where it can change without a migration. The length is 3..12
--    rather than exactly 3 because a price may be denominated in a stablecoin
--    (USDC, USDG — pkg/catalog.stablecoinCurrencies), so alpha-3 is not the
--    whole vocabulary. Case is the part that was actually drifting, and case is
--    what this pins absolutely.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------- part 1 ---

DROP INDEX IF EXISTS openrails.checkout_sessions_rail_reference_idx;
DROP INDEX IF EXISTS openrails.checkout_sessions_rail_transaction_id_idx;

CREATE UNIQUE INDEX uq_checkout_sessions_merchant_psp_reference
    ON openrails.checkout_sessions USING btree (
        merchant_id,
        rail,
        COALESCE(psp_id, '00000000-0000-0000-0000-000000000000'::uuid),
        reference
    )
    WHERE ((reference IS NOT NULL) AND (deleted_at IS NULL));

CREATE UNIQUE INDEX uq_checkout_sessions_merchant_psp_transaction
    ON openrails.checkout_sessions USING btree (
        merchant_id,
        rail,
        COALESCE(psp_id, '00000000-0000-0000-0000-000000000000'::uuid),
        transaction_id
    )
    WHERE ((transaction_id IS NOT NULL) AND (deleted_at IS NULL));

-- Second instance of the same class, found by the guard rather than by the
-- audit: catalog_drift_events is merchant_id NOT NULL and RLS-protected, yet
-- its open-drift unique spans merchants. An NMI plan id is only unique WITHIN a
-- gateway account (the fact 0017 exists to record), so two merchants can hold
-- the same external_resource_id — and the second one's drift event silently
-- fails to insert against a row it cannot see.
DROP INDEX IF EXISTS openrails.uq_catalog_drift_open;

CREATE UNIQUE INDEX uq_catalog_drift_open
    ON openrails.catalog_drift_events USING btree (
        merchant_id,
        rail,
        kind,
        openrails_resource_type,
        COALESCE(openrails_resource_id, ''::text),
        COALESCE(external_resource_id, ''::text),
        COALESCE(field, ''::text)
    )
    WHERE (resolved_at IS NULL);

-- ---------------------------------------------------------------- part 2 ---

-- Canonicalise what is already stored. Case-only, so no amount, identity or
-- ledger relationship changes; a qualified custom-credit unit is left verbatim
-- (its slug half is a lower-case merchant slug).
DO $$
DECLARE
    col record;
BEGIN
    FOR col IN
        SELECT c.table_name
          FROM information_schema.columns c
          JOIN pg_class pc ON pc.relname = c.table_name
          JOIN pg_namespace pn ON pn.oid = pc.relnamespace AND pn.nspname = 'openrails'
         WHERE c.table_schema = 'openrails'
           AND c.column_name = 'currency'
           AND pc.relkind = 'r'
    LOOP
        EXECUTE format(
            'UPDATE openrails.%I SET currency = upper(currency) '
            'WHERE currency IS NOT NULL AND position(''/'' in currency) = 0 '
            'AND currency <> upper(currency)', col.table_name);

        EXECUTE format(
            'ALTER TABLE openrails.%I ADD CONSTRAINT %I CHECK ('
            '  currency IS NULL'
            '  OR currency ~ ''^[A-Z0-9]{3,12}$'''
            '  OR currency ~ ''^[a-z0-9][a-z0-9_-]*/[^/[:space:]]+$'''
            ')', col.table_name, col.table_name || '_currency_shape');
    END LOOP;
END
$$;

