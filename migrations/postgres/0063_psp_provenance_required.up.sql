-- or#893 phase 1, task 1 — the ATOMIC half PR #235 deferred: PSP provenance
-- becomes TOTAL on every provider-bound table, and the nil-UUID lane that
-- nullability forced into the uniques is deleted with it.
--
-- 0060 did this for rail_refresh_watermarks. The rest stayed nullable because
-- the import wire contract was undecided; it is decided now (billingimport
-- requires a per-row PSP or a whole-import default), so the columns can close.
--
-- THE DEFECT THIS CLOSES (live, and not hypothetical — 0013's own header
-- records the class): while `psp_id` is nullable, `uq_payments_merchant_psp_transaction`
-- keys on `COALESCE(psp_id, '00000000-…'::uuid)`. An unattributed row and a
-- PSP-attributed row for the SAME provider transaction hash to two DIFFERENT
-- keys, so both are insertable. That is one provider charge, twice: double-counted
-- revenue and double fulfilment. 0013 closed it by dropping the PSP dimension;
-- 0017 correctly restored the dimension (a provider id is only unique WITHIN a
-- gateway account) but restored the hole along with it, because COALESCE-to-nil
-- is only "one total index" if nil is unreachable. Making the column NOT NULL is
-- what actually makes it unreachable, and then the COALESCE has nothing left to
-- do. Same shape on subscriptions, checkout_sessions (0020) and payment_methods
-- (whose two `WHERE psp_id IS NULL` legacy uniques are disjoint from
-- `uq_payment_methods_psp_instrument` — the identical hole, one instrument twice).
--
-- GENUINELY PROVIDER-NEUTRAL ROWS, classified explicitly rather than defaulted
-- to NULL (#893 task 1, bullet 1). Two of the nine columns have a real
-- non-provider lane: `payments` and `invoice_payments` also record OFF-RAIL
-- money — the admin manual-payment endpoint and manual invoice settlement write
-- rail = 'manual'/'admin' (models.Channel: "a channel is NOT a rail: it has no
-- adapter, no credentials, and no provider account"). Those rows have no PSP
-- because no PSP was involved, so they get a CHECK that says exactly that
-- instead of a NOT NULL that would be a lie. Every other provider-bound table
-- closes to NOT NULL: an off-rail subscription, vault entry, checkout session,
-- intent or mutation log does not exist.
--
-- The CHECK is what closes the double-count hole on `payments`, not the index
-- shape: a row on a REAL rail can no longer be unattributed at all, so the
-- attributed and unattributed lanes can never hold the same provider
-- transaction. The two partial uniques below are therefore provably disjoint by
-- constraint, which is what 0013's pair was missing.
--
-- BACKFILL DOCTRINE: prelaunch, disposable dev databases (#893's own doctrine).
-- A row is attributed when its context RESOLVES — the merchant has exactly one
-- non-archived PSP on the row's rail — and deleted when it does not. Nothing is
-- invented: two PSPs on a rail means the row's origin is genuinely unknown, and
-- a guessed provenance is worse than no row. Every count is RAISEd as a NOTICE.

SET LOCAL statement_timeout = '120s';
SET LOCAL lock_timeout = '10s';

-- The one resolvable-context rule, repeated verbatim below rather than
-- materialised: a merchant+rail with exactly one live PSP means that PSP is the
-- only thing the row could have come from. (A temp table would show up in
-- sqlc's schema read of migrations/ as a generated model.)

-- --------------------------------------------- the double-count, resolved ---
-- The defect itself, in whatever rows a dev database already holds: an
-- UNATTRIBUTED row for a provider key that an ATTRIBUTED row already covers is
-- the same provider object counted twice. Attributing it would just move the
-- collision to the unique, so the duplicate goes — the attributed row is the
-- one with provenance, and it is the one that survives.

DO $$
DECLARE n bigint;
BEGIN
    DELETE FROM openrails.payments p
     WHERE p.psp_id IS NULL
       AND EXISTS (SELECT 1 FROM openrails.payments q
                    WHERE q.psp_id IS NOT NULL AND q.merchant_id = p.merchant_id
                      AND q.rail = p.rail AND q.transaction_id = p.transaction_id);
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE 'or#893 double-counted duplicates removed: payments=%', n;

    DELETE FROM openrails.subscriptions s
     WHERE s.psp_id IS NULL AND s.rail_subscription_id <> ''
       AND EXISTS (SELECT 1 FROM openrails.subscriptions q
                    WHERE q.psp_id IS NOT NULL AND q.merchant_id = s.merchant_id
                      AND q.rail = s.rail AND q.rail_subscription_id = s.rail_subscription_id);
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE 'or#893 double-counted duplicates removed: subscriptions=%', n;

    DELETE FROM openrails.payment_methods m
     WHERE m.psp_id IS NULL
       AND EXISTS (SELECT 1 FROM openrails.payment_methods q
                    WHERE q.psp_id IS NOT NULL AND q.merchant_id = m.merchant_id
                      AND q.rail = m.rail AND q.rail_customer_ref = m.rail_customer_ref
                      AND q.rail_method_ref = m.rail_method_ref);
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE 'or#893 double-counted duplicates removed: payment_methods=%', n;

    DELETE FROM openrails.checkout_sessions c
     WHERE c.psp_id IS NULL
       AND EXISTS (SELECT 1 FROM openrails.checkout_sessions q
                    WHERE q.psp_id IS NOT NULL AND q.merchant_id = c.merchant_id
                      AND q.rail = c.rail
                      AND (q.transaction_id IS NOT DISTINCT FROM c.transaction_id
                           OR q.reference IS NOT DISTINCT FROM c.reference));
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE 'or#893 double-counted duplicates removed: checkout_sessions=%', n;
END $$;

-- ------------------------------------------------------------- attribution ---

-- Off-rail rows (rail = manual/admin) match no PSP by construction and stay NULL.
UPDATE openrails.payments t SET psp_id = s.psp_id
  FROM (SELECT merchant_id, rail, (array_agg(id))[1] AS psp_id
         FROM openrails.psps WHERE archived = false
        GROUP BY merchant_id, rail HAVING count(*) = 1) s
 WHERE t.psp_id IS NULL AND t.merchant_id = s.merchant_id AND t.rail = s.rail;

UPDATE openrails.subscriptions t SET psp_id = s.psp_id
  FROM (SELECT merchant_id, rail, (array_agg(id))[1] AS psp_id
         FROM openrails.psps WHERE archived = false
        GROUP BY merchant_id, rail HAVING count(*) = 1) s
 WHERE t.psp_id IS NULL AND t.merchant_id = s.merchant_id AND t.rail = s.rail;

UPDATE openrails.payment_methods t SET psp_id = s.psp_id
  FROM (SELECT merchant_id, rail, (array_agg(id))[1] AS psp_id
         FROM openrails.psps WHERE archived = false
        GROUP BY merchant_id, rail HAVING count(*) = 1) s
 WHERE t.psp_id IS NULL AND t.merchant_id = s.merchant_id AND t.rail::text = s.rail;

UPDATE openrails.checkout_sessions t SET psp_id = s.psp_id
  FROM (SELECT merchant_id, rail, (array_agg(id))[1] AS psp_id
         FROM openrails.psps WHERE archived = false
        GROUP BY merchant_id, rail HAVING count(*) = 1) s
 WHERE t.psp_id IS NULL AND t.merchant_id = s.merchant_id AND t.rail = s.rail;

UPDATE openrails.rail_intents t SET psp_id = s.psp_id
  FROM (SELECT merchant_id, rail, (array_agg(id))[1] AS psp_id
         FROM openrails.psps WHERE archived = false
        GROUP BY merchant_id, rail HAVING count(*) = 1) s
 WHERE t.psp_id IS NULL AND t.merchant_id = s.merchant_id AND t.rail = s.rail;

UPDATE openrails.rail_mutation_logs t SET psp_id = s.psp_id
  FROM (SELECT merchant_id, rail, (array_agg(id))[1] AS psp_id
         FROM openrails.psps WHERE archived = false
        GROUP BY merchant_id, rail HAVING count(*) = 1) s
 WHERE t.psp_id IS NULL AND t.merchant_id = s.merchant_id AND t.rail = s.rail;

-- invoice_payments has a nearer source of truth than the rail: the instrument
-- it charged already carries the PSP that vaulted it (or#823/#240 recorded this
-- column as "to be stamped"; its writer starts stamping in this PR).
UPDATE openrails.invoice_payments t SET psp_id = pm.psp_id
  FROM openrails.payment_methods pm
 WHERE t.psp_id IS NULL AND t.payment_method_id = pm.id AND pm.psp_id IS NOT NULL;

UPDATE openrails.invoice_payments t SET psp_id = s.psp_id
  FROM (SELECT merchant_id, rail, (array_agg(id))[1] AS psp_id
         FROM openrails.psps WHERE archived = false
        GROUP BY merchant_id, rail HAVING count(*) = 1) s
 WHERE t.psp_id IS NULL AND t.merchant_id = s.merchant_id AND t.rail = s.rail;

-- -------------------------------------------------------- orphan deletion ---
-- Order follows the FK graph. Nullable references to a doomed row are cleared
-- first so the delete cannot fail on a NO ACTION edge; RESTRICT dependents are
-- deleted with their parent.

DO $$
DECLARE
    n bigint;
    doomed_payments uuid[];
    doomed_subs uuid[];
BEGIN
    -- checkout_sessions: nothing references it.
    DELETE FROM openrails.checkout_sessions WHERE psp_id IS NULL;
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE 'or#893 orphans deleted: checkout_sessions=%', n;

    -- rail_intents: rail_mutation_logs.rail_intent_id is ON DELETE SET NULL.
    DELETE FROM openrails.rail_intents WHERE psp_id IS NULL;
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE 'or#893 orphans deleted: rail_intents=%', n;

    DELETE FROM openrails.rail_mutation_logs WHERE psp_id IS NULL;
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE 'or#893 orphans deleted: rail_mutation_logs=%', n;

    DELETE FROM openrails.invoice_payments
     WHERE psp_id IS NULL AND coalesce(rail, '') NOT IN ('manual', 'admin');
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE 'or#893 orphans deleted: invoice_payments=%', n;

    SELECT coalesce(array_agg(id), '{}') INTO doomed_payments
      FROM openrails.payments WHERE psp_id IS NULL AND rail NOT IN ('manual', 'admin');
    UPDATE openrails.grants SET payment_id = NULL WHERE payment_id = ANY (doomed_payments);
    UPDATE openrails.checkout_sessions SET payment_id = NULL WHERE payment_id = ANY (doomed_payments);
    UPDATE openrails.payments SET refunded_payment_id = NULL WHERE refunded_payment_id = ANY (doomed_payments);
    DELETE FROM openrails.payments WHERE id = ANY (doomed_payments);
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE 'or#893 orphans deleted: payments=%', n;

    SELECT coalesce(array_agg(id), '{}') INTO doomed_subs
      FROM openrails.subscriptions WHERE psp_id IS NULL;
    UPDATE openrails.checkout_sessions SET subscription_id = NULL WHERE subscription_id = ANY (doomed_subs);
    DELETE FROM openrails.subscription_reprices WHERE subscription_id = ANY (doomed_subs);
    DELETE FROM openrails.subscriptions WHERE psp_id IS NULL;
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE 'or#893 orphans deleted: subscriptions=%', n;

    -- every remaining reference to a payment method is ON DELETE SET NULL.
    DELETE FROM openrails.payment_methods WHERE psp_id IS NULL;
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE 'or#893 orphans deleted: payment_methods=%', n;
END $$;

-- ------------------------------------------------ the columns close for good ---
-- squawk `adding-not-nullable-field`: every row the scan validates was either
-- attributed or deleted by the statements above, in this same transaction —
-- the rule's two-step (nullable + CHECK) cannot express a column attribute, and
-- CUR-1's invariant reads information_schema.is_nullable. Same argument 0044
-- and 0060 recorded.
-- squawk-ignore adding-not-nullable-field
ALTER TABLE openrails.subscriptions ALTER COLUMN psp_id SET NOT NULL;
-- squawk-ignore adding-not-nullable-field
ALTER TABLE openrails.payment_methods ALTER COLUMN psp_id SET NOT NULL;
-- squawk-ignore adding-not-nullable-field
ALTER TABLE openrails.checkout_sessions ALTER COLUMN psp_id SET NOT NULL;
-- squawk-ignore adding-not-nullable-field
ALTER TABLE openrails.rail_intents ALTER COLUMN psp_id SET NOT NULL;

-- The two tables that also carry OFF-RAIL money. NOT NULL would be a lie there,
-- so the invariant is stated as what it actually is: on a real rail the row is
-- attributed; the only unattributed rows are the ones no provider touched.
-- The statements above attributed or deleted every row this validates, in this
-- same transaction, and NOT VALID buys nothing inside a single-transaction
-- migrator (see 0014).
ALTER TABLE openrails.payments
    -- squawk-ignore constraint-missing-not-valid
    ADD CONSTRAINT payments_psp_required_on_rail
    CHECK (psp_id IS NOT NULL OR rail IN ('manual', 'admin'));
ALTER TABLE openrails.invoice_payments
    -- squawk-ignore constraint-missing-not-valid
    ADD CONSTRAINT invoice_payments_psp_required_on_rail
    CHECK (psp_id IS NOT NULL OR rail IN ('manual', 'admin'));
-- squawk-ignore adding-not-nullable-field
ALTER TABLE openrails.rail_mutation_logs ALTER COLUMN psp_id SET NOT NULL;

-- A mutation log whose PSP is gone was already unreadable as provenance; ON
-- DELETE SET NULL is now unrepresentable, so it follows the PSP out (the same
-- choice 0060 made for the watermark cursor).
ALTER TABLE openrails.rail_mutation_logs DROP CONSTRAINT rail_mutation_logs_psp_fk;
ALTER TABLE openrails.rail_mutation_logs
    -- squawk-ignore adding-foreign-key-constraint, constraint-missing-not-valid
    ADD CONSTRAINT rail_mutation_logs_psp_fk FOREIGN KEY (psp_id)
    REFERENCES openrails.psps(id) ON DELETE CASCADE;

COMMENT ON COLUMN openrails.payments.psp_id IS 'PSP that took this charge. Required on every real rail (payments_psp_required_on_rail); NULL only for off-rail channels (manual/admin), which have no provider — or#893.';
COMMENT ON COLUMN openrails.subscriptions.psp_id IS 'PSP that produced this remote subscription mirror row. Required (or#893).';
COMMENT ON COLUMN openrails.payment_methods.psp_id IS 'PSP that vaulted this payment method. Required (or#893).';
COMMENT ON COLUMN openrails.checkout_sessions.psp_id IS 'PSP selected for this provider checkout/session. Required (or#893).';
COMMENT ON COLUMN openrails.invoice_payments.psp_id IS 'PSP that took this invoice payment attempt. Required on every real rail (invoice_payments_psp_required_on_rail); NULL only for off-rail manual settlement — or#893.';
COMMENT ON COLUMN openrails.rail_intents.psp_id IS 'PSP the outbound intent was enqueued against. Required (or#893).';
COMMENT ON COLUMN openrails.rail_mutation_logs.psp_id IS 'PSP the logged mutation was addressed to. Required (or#893).';

-- ------------------------------------------------- uniques lose the nil lane ---

-- The COALESCE-to-nil lane goes. It made an unattributed row hash to a DIFFERENT
-- key than its attributed twin, which is the double-count. The replacement pair
-- is partial on the same column 0013 warned about — but it is safe here for a
-- reason 0013's pair did not have: payments_psp_required_on_rail makes
-- `psp_id IS NULL` imply an off-rail channel, and a channel shares no `rail`
-- value with any PSP. A provider transaction can only ever land in the first
-- index, and it is always attributed there.
-- squawk-ignore disallowed-unique-constraint
DROP INDEX openrails.uq_payments_merchant_psp_transaction;
CREATE UNIQUE INDEX uq_payments_merchant_psp_transaction
    ON openrails.payments USING btree (merchant_id, rail, psp_id, transaction_id)
    WHERE (psp_id IS NOT NULL AND deleted_at IS NULL);
CREATE UNIQUE INDEX uq_payments_merchant_offrail_transaction
    ON openrails.payments USING btree (merchant_id, rail, transaction_id)
    WHERE (psp_id IS NULL AND deleted_at IS NULL);

-- squawk-ignore disallowed-unique-constraint
DROP INDEX openrails.uq_subscriptions_merchant_psp_subscription_id;
CREATE UNIQUE INDEX uq_subscriptions_merchant_psp_subscription_id
    ON openrails.subscriptions USING btree (merchant_id, rail, psp_id, rail_subscription_id)
    WHERE (rail_subscription_id <> ''::text AND deleted_at IS NULL);

DROP INDEX openrails.uq_checkout_sessions_merchant_psp_reference;
CREATE UNIQUE INDEX uq_checkout_sessions_merchant_psp_reference
    ON openrails.checkout_sessions USING btree (merchant_id, rail, psp_id, reference)
    WHERE (reference IS NOT NULL AND deleted_at IS NULL);

DROP INDEX openrails.uq_checkout_sessions_merchant_psp_transaction;
CREATE UNIQUE INDEX uq_checkout_sessions_merchant_psp_transaction
    ON openrails.checkout_sessions USING btree (merchant_id, rail, psp_id, transaction_id)
    WHERE (transaction_id IS NOT NULL AND deleted_at IS NULL);

-- The two `WHERE psp_id IS NULL` uniques were the unattributed half of the same
-- disjoint pair; with no NULL lane left they index nothing. The PSP-scoped one
-- becomes total.
DROP INDEX openrails.uq_payment_methods_customer_instrument_legacy;
DROP INDEX openrails.uq_payment_methods_merchant_rail_instrument_legacy;
DROP INDEX openrails.uq_payment_methods_psp_instrument;
CREATE UNIQUE INDEX uq_payment_methods_psp_instrument
    ON openrails.payment_methods USING btree (merchant_id, psp_id, rail_customer_ref, rail_method_ref);

-- `WHERE psp_id IS NOT NULL` on a NOT NULL column is a partial index over the
-- whole table that the planner must still prove matches. Rebuild those total.
-- payments and invoice_payments keep theirs: the column is genuinely nullable
-- there, and the partial correctly skips the off-rail rows.
DROP INDEX openrails.idx_subscriptions_psp;
CREATE INDEX idx_subscriptions_psp ON openrails.subscriptions USING btree (psp_id);
DROP INDEX openrails.idx_payment_methods_psp;
CREATE INDEX idx_payment_methods_psp ON openrails.payment_methods USING btree (psp_id);
DROP INDEX openrails.idx_checkout_sessions_psp;
CREATE INDEX idx_checkout_sessions_psp ON openrails.checkout_sessions USING btree (psp_id);
DROP INDEX openrails.idx_rail_intents_psp;
CREATE INDEX idx_rail_intents_psp ON openrails.rail_intents USING btree (psp_id);
DROP INDEX openrails.idx_rail_mutation_logs_psp;
CREATE INDEX idx_rail_mutation_logs_psp ON openrails.rail_mutation_logs USING btree (psp_id);

-- --------------------------------------------- rail_customer_accounts (#704) ---
-- #704 dropped this table's PSP column because its only writer never set it.
-- Both writers resolve one now (the Stripe webhook plane from the routed PSP,
-- converge from the subscription's own psp_id), and the mapping being unique
-- only by (merchant, customer, rail) means a merchant running two Stripe
-- accounts has ONE mapping: whichever account's webhook landed last overwrites
-- the other, and the reverse lookup then resolves the wrong person's cus_.

ALTER TABLE openrails.rail_customer_accounts ADD COLUMN psp_id uuid;

UPDATE openrails.rail_customer_accounts t SET psp_id = s.psp_id
  FROM (SELECT merchant_id, rail, (array_agg(id))[1] AS psp_id
         FROM openrails.psps WHERE archived = false
        GROUP BY merchant_id, rail HAVING count(*) = 1) s
 WHERE t.psp_id IS NULL AND t.merchant_id = s.merchant_id AND t.rail = s.rail;

DO $$
DECLARE n bigint;
BEGIN
    DELETE FROM openrails.rail_customer_accounts WHERE psp_id IS NULL;
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE 'or#893 orphans deleted: rail_customer_accounts=%', n;
END $$;

-- squawk-ignore adding-not-nullable-field
ALTER TABLE openrails.rail_customer_accounts ALTER COLUMN psp_id SET NOT NULL;
ALTER TABLE openrails.rail_customer_accounts
    -- squawk-ignore adding-foreign-key-constraint, constraint-missing-not-valid
    ADD CONSTRAINT rail_customer_accounts_psp_fk FOREIGN KEY (psp_id)
    REFERENCES openrails.psps(id) ON DELETE CASCADE;

COMMENT ON TABLE openrails.rail_customer_accounts IS 'customer <-> rail customer-id mapping, per PSP. Two accounts on one rail hold independent mappings (or#893 supersedes #704, which dropped psp_id when no writer set it).';
COMMENT ON COLUMN openrails.rail_customer_accounts.psp_id IS 'PSP whose remote customer object this row maps. Required (or#893).';

DROP INDEX openrails.uq_rail_customer_accounts_customer_rail;
DROP INDEX openrails.uq_rail_customer_accounts_merchant_rail_customer;
-- rail is redundant beside psp_id (a PSP has exactly one rail) and kept because
-- every lookup is (customer, rail) or (rail, account_id) shaped.
CREATE UNIQUE INDEX uq_rail_customer_accounts_customer_psp
    ON openrails.rail_customer_accounts USING btree (merchant_id, customer_id, rail, psp_id);
CREATE UNIQUE INDEX uq_rail_customer_accounts_psp_account
    ON openrails.rail_customer_accounts USING btree (merchant_id, rail, psp_id, account_id);
CREATE INDEX idx_rail_customer_accounts_psp ON openrails.rail_customer_accounts USING btree (psp_id);
