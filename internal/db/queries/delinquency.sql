-- or#878 arrears delinquency: the state projection, its due-work scans, and the
-- durable host-lifecycle signal feed.

-- CROSS-MERCHANT: merchants with delinquency work, through migration 0037's
-- SECURITY DEFINER work queue. Ids only — states and signals are computed
-- per-merchant under RunInMerchantScope. Both legs of the union are indexed on
-- the work itself (overdue receivables / already-non-current payers), so a pass
-- costs activity, never the size of the customer table.
-- name: ListDelinquencyWorkMerchants :many
SELECT merchant_id FROM openrails.delinquency_work_merchant_ids(
    sqlc.arg(now)::timestamptz,
    sqlc.arg(merchant_limit)::int);

-- name: ListOverdueInvoiceAggregates :many
-- The ENTER leg of the evaluation: per (payer, currency), how much is overdue
-- and since when. This is the whole derivation input — delinquency is a reading
-- of invoice truth against the merchant's policy, not a separately-maintained
-- fact. Rides ix_invoices_open_due (partial on exactly the open, still-owed set).
--
-- OLDEST DEBT FIRST, and capped: one pass is bounded work. If a merchant ever
-- has more overdue payers than the cap, the ones who have owed longest are the
-- ones evaluated — truncation with a defensible order beats an unbounded pass.
SELECT customer_id,
       currency,
       MIN(due_at)::timestamptz AS overdue_since,
       COALESCE(SUM(amount_due), 0)::bigint AS overdue_amount,
       COUNT(*)::bigint AS overdue_invoices
FROM openrails.invoices
WHERE merchant_id = sqlc.arg(merchant_id)
  AND status IN ('open', 'past_due')
  AND amount_due > 0
  AND due_at IS NOT NULL
  AND due_at < sqlc.arg(now)::timestamptz
GROUP BY customer_id, currency
ORDER BY MIN(due_at), customer_id, currency
LIMIT sqlc.arg(row_limit);

-- name: GetOverdueInvoiceAggregate :one
-- One payer's overdue aggregate — the live recompute the admission gate runs
-- before it agrees with a stored `delinquent`, so a payer who has since settled
-- is never refused on a stale projection.
--
-- overdue_since is COALESCEd to `now` so the row is total: it means nothing when
-- overdue_invoices = 0, and every caller reads the count first.
SELECT COALESCE(MIN(due_at), sqlc.arg(now)::timestamptz) AS overdue_since,
       COALESCE(SUM(amount_due), 0)::bigint AS overdue_amount,
       COUNT(*)::bigint AS overdue_invoices
FROM openrails.invoices
WHERE merchant_id = sqlc.arg(merchant_id)
  AND customer_id = sqlc.arg(customer_id)
  AND currency = sqlc.arg(currency)
  AND status IN ('open', 'past_due')
  AND amount_due > 0
  AND due_at IS NOT NULL
  AND due_at < sqlc.arg(now)::timestamptz;

-- name: ListNonCurrentDelinquency :many
-- The EXIT leg: payers already parked in grace/delinquent. Small by
-- construction, and the only way a cleared debt gets noticed — an invoice that
-- was paid no longer appears in the enter scan at all.
SELECT * FROM openrails.customer_delinquency
WHERE merchant_id = sqlc.arg(merchant_id)
  AND state <> 'current'
ORDER BY overdue_since, customer_id, currency
LIMIT sqlc.arg(row_limit);

-- name: GetCustomerDelinquency :one
SELECT * FROM openrails.customer_delinquency
WHERE merchant_id = sqlc.arg(merchant_id)
  AND customer_id = sqlc.arg(customer_id)
  AND currency = sqlc.arg(currency);

-- name: ListCustomerDelinquency :many
-- Every currency for one payer (the per-payer API read).
SELECT * FROM openrails.customer_delinquency
WHERE merchant_id = sqlc.arg(merchant_id)
  AND customer_id = sqlc.arg(customer_id)
ORDER BY currency;

-- name: ListDelinquentCustomers :many
-- The operator's roster: who is overdue, worst first. `current` rows are never
-- returned — a settled payer is not a row anyone needs to look at.
SELECT * FROM openrails.customer_delinquency
WHERE merchant_id = sqlc.arg(merchant_id)
  AND state <> 'current'
  AND (sqlc.narg(state)::text IS NULL OR state = sqlc.narg(state)::text)
ORDER BY overdue_since, customer_id, currency
LIMIT sqlc.arg(row_limit);

-- name: UpsertCustomerDelinquency :one
-- One evaluation, atomic, returning BOTH the state that was there and the state
-- that is there now — the edge, which is what the signal is cut from.
--
-- transition_seq advances only on a real change, so it is a deterministic
-- coordinate for the emitted event: two evaluators racing the same transition
-- compute the same sequence, hence the same dedupe key, hence one event.
WITH previous AS (
    SELECT state, entered_at, transition_seq
    FROM openrails.customer_delinquency
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND customer_id = sqlc.arg(customer_id)::uuid
      AND currency = sqlc.arg(currency)::text
), upserted AS (
    INSERT INTO openrails.customer_delinquency AS d (
        merchant_id, customer_id, currency, state, overdue_since, overdue_amount,
        overdue_invoices, entered_at, transition_seq, evaluated_at, created_at, updated_at)
    VALUES (
        sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text, sqlc.arg(state)::text,
        sqlc.narg(overdue_since)::timestamptz, sqlc.arg(overdue_amount)::bigint,
        sqlc.arg(overdue_invoices)::bigint, sqlc.arg(now)::timestamptz, 1,
        sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
    ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE
    SET state = EXCLUDED.state,
        overdue_since = EXCLUDED.overdue_since,
        overdue_amount = EXCLUDED.overdue_amount,
        overdue_invoices = EXCLUDED.overdue_invoices,
        entered_at = CASE WHEN d.state IS DISTINCT FROM EXCLUDED.state THEN EXCLUDED.entered_at ELSE d.entered_at END,
        transition_seq = d.transition_seq + (CASE WHEN d.state IS DISTINCT FROM EXCLUDED.state THEN 1 ELSE 0 END),
        evaluated_at = EXCLUDED.evaluated_at,
        updated_at = EXCLUDED.updated_at
    RETURNING d.state, d.overdue_since, d.overdue_amount, d.overdue_invoices, d.entered_at, d.transition_seq
)
SELECT upserted.state,
       upserted.overdue_since,
       upserted.overdue_amount,
       upserted.overdue_invoices,
       upserted.entered_at,
       upserted.transition_seq,
       COALESCE((SELECT previous.state FROM previous), '')::text AS previous_state
FROM upserted;

-- A row is NEVER deleted once it exists, and one is never created for a payer
-- whose first evaluation is already `current`. Deleting a cleared row would
-- restart transition_seq, and a restarted sequence collides with the dedupe key
-- of the payer's earlier transition — which would silently swallow the second
-- shutoff signal for anyone who has been delinquent before.
