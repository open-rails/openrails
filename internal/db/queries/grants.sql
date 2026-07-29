-- #514 append-only grant ledger (openrails.grants). derive-1 appends events here;
-- derive-2 folds them into projections (entitlement windows, #512 credit deposits;
-- ownership is read directly off this table).

-- name: InsertGrant :one
INSERT INTO openrails.grants (
    merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id,
    event, supersedes_id, spec_snapshot, starts_at, ends_at, amount, currency, reason
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.narg(product_id)::uuid,
    sqlc.arg(kind)::text, sqlc.arg(source_type)::text, sqlc.arg(source_id)::text, sqlc.narg(payment_id)::uuid,
    sqlc.arg(event)::text, sqlc.narg(supersedes_id)::uuid, sqlc.narg(spec_snapshot)::jsonb,
    sqlc.arg(starts_at)::timestamptz, sqlc.narg(ends_at)::timestamptz,
    sqlc.narg(amount)::bigint, sqlc.narg(currency)::text, sqlc.narg(reason)::text
)
RETURNING *;

-- name: GetGrant :one
SELECT * FROM openrails.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- GetCreditGrantBySourceID: idempotency lookup for a deposit-as-credit-grant by
-- its natural source_id key (the deposit's SourceID).
-- name: GetCreditGrantBySourceID :one
SELECT * FROM openrails.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND kind = 'credit' AND event = 'grant'
  AND source_id = sqlc.arg(source_id)::text
ORDER BY created_at ASC
LIMIT 1;

-- name: GetOwnershipGrantBySourceID :one
SELECT * FROM openrails.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND product_id = sqlc.arg(product_id)::uuid
  AND kind = 'ownership' AND event = 'grant'
  AND source_id = sqlc.arg(source_id)::text
ORDER BY created_at ASC
LIMIT 1;

-- name: ListIncludedProductIDs :many
SELECT included_product_id FROM openrails.product_includes
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND product_id = sqlc.arg(product_id)::uuid
ORDER BY included_product_id;

-- name: ListProductUsageLimitSpecs :many
SELECT pul.usage_limit_key, cul.measure, cul.windows, p.key AS product_key
FROM openrails.product_usage_limits pul
JOIN openrails.catalog_usage_limits cul
  ON cul.merchant_id = pul.merchant_id AND cul.key = pul.usage_limit_key
JOIN openrails.products p
  ON p.merchant_id = pul.merchant_id AND p.id = pul.product_id
WHERE pul.merchant_id = sqlc.arg(merchant_id)::uuid
  AND pul.product_id = sqlc.arg(product_id)::uuid
ORDER BY pul.usage_limit_key;

-- name: ProductUsageLimitBindingExistsForGrant :one
SELECT EXISTS (
    SELECT 1 FROM openrails.product_usage_limit_bindings
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND grant_id = sqlc.arg(grant_id)::uuid
      AND usage_limit_key = sqlc.arg(usage_limit_key)::text
) AS exists;

-- name: CreateProductUsageLimitBinding :exec
INSERT INTO openrails.product_usage_limit_bindings (
    id, merchant_id, customer_id, usage_limit_key, measure, windows,
    source_type, source_id, product_key, grant_id, starts_at, ends_at, policy_version
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid,
    sqlc.arg(usage_limit_key)::text, sqlc.arg(measure)::text, sqlc.arg(windows)::jsonb,
    sqlc.arg(source_type)::text, sqlc.narg(source_id)::uuid, sqlc.narg(product_key)::text,
    sqlc.narg(grant_id)::uuid, sqlc.arg(starts_at)::timestamptz,
    sqlc.narg(ends_at)::timestamptz, sqlc.arg(policy_version)::bigint
);

-- name: RevokeProductUsageLimitBindingsByGrant :exec
UPDATE openrails.product_usage_limit_bindings
SET revoked_at = sqlc.arg(revoked_at)::timestamptz,
    updated_at = now(),
    policy_version = policy_version + 1
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND grant_id = sqlc.arg(grant_id)::uuid
  AND revoked_at IS NULL;

-- ListLiveGrantsByCustomer: grant-events not terminated by a later revoke/expire/
-- supersede event. The fold's input.
-- name: ListLiveGrantsByCustomer :many
SELECT g.* FROM openrails.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND g.customer_id = sqlc.arg(customer_id)::uuid
  AND g.event = 'grant'
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants t
      WHERE t.supersedes_id = g.id AND t.event IN ('revoke', 'expire', 'supersede')
  )
ORDER BY g.created_at;

-- ListGrantsByCustomer: every grant-event for the customer (live or terminated),
-- the full input to a customer-scoped re-derive.
-- name: ListGrantsByCustomer :many
SELECT * FROM openrails.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND event = 'grant'
ORDER BY created_at;

-- name: IsGrantTerminated :one
SELECT EXISTS (
    SELECT 1 FROM openrails.grants t
    WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid
      AND t.supersedes_id = sqlc.arg(grant_id)::uuid
      AND t.event IN ('revoke', 'expire', 'supersede')
) AS terminated;

-- GrantCreditDeposited: has derive-2 already emitted this credit grant's #512
-- deposit transfer? (idempotency for the credit projection)
-- name: GrantCreditDeposited :one
SELECT EXISTS (
    SELECT 1 FROM openrails.ledger_transfers
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND transfer_type = 'deposit' AND grant_id = sqlc.arg(grant_id)::uuid
) AS deposited;

-- GetCreditLotRemaining: a single credit lot's derived unspent remainder
-- (amount − spent − expired − already-revoked). Deducting `credit_revoke` makes
-- the revoke clawback idempotent: once clawed, remaining is 0 and re-running the
-- projection is a no-op.
-- name: GetCreditLotRemaining :one
SELECT (g.amount - COALESCE((
    SELECT SUM(t.amount) FROM openrails.ledger_transfers t
    WHERE t.merchant_id = g.merchant_id AND t.grant_id = g.id
      AND t.transfer_type IN ('credit_spend', 'credit_expire', 'credit_revoke')
), 0))::bigint AS remaining
FROM openrails.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid AND g.id = sqlc.arg(grant_id)::uuid
  AND g.kind = 'credit' AND g.event = 'grant';

-- SumCreditGrants: cumulative credits granted to a customer in one currency (the
-- "amount paid / trust signal" that graduates the tier). Replaces SumMoneyDeposits.
-- name: SumCreditGrants :one
SELECT COALESCE(SUM(amount), 0)::bigint
FROM openrails.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND kind = 'credit' AND event = 'grant';

-- name: EntitlementExistsForGrant :one
SELECT EXISTS (
    SELECT 1 FROM openrails.entitlements
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND grant_id = sqlc.arg(grant_id)::uuid
      AND entitlement = sqlc.arg(entitlement)::text
      AND deleted_at IS NULL
) AS exists;

-- ListSpendableCreditLots: live (started, unexpired, non-terminated) credit-lot
-- grants with derived remaining = lot amount − Σ(credit_spend + credit_expire
-- transfers tagged to the lot). FIFO order: soonest expiry first.
-- name: ListSpendableCreditLots :many
SELECT g.id, g.amount, g.ends_at,
    (g.amount - COALESCE((
        SELECT SUM(t.amount) FROM openrails.ledger_transfers t
        WHERE t.merchant_id = g.merchant_id AND t.grant_id = g.id
          AND t.transfer_type IN ('credit_spend', 'credit_expire')
    ), 0))::bigint AS remaining
FROM openrails.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND g.customer_id = sqlc.arg(customer_id)::uuid
  AND g.kind = 'credit' AND g.event = 'grant' AND g.currency = sqlc.arg(currency)::text
  AND g.starts_at <= sqlc.arg(as_of)::timestamptz
  AND (g.ends_at IS NULL OR g.ends_at > sqlc.arg(as_of)::timestamptz)
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants tt WHERE tt.supersedes_id = g.id AND tt.event IN ('revoke', 'expire', 'supersede')
  )
ORDER BY g.ends_at ASC NULLS LAST, g.created_at ASC;

-- ListLapsedCreditLots: credit lots past their expiry with an unspent remainder
-- to claw to expired_credits.
-- name: ListLapsedCreditLots :many
SELECT g.id,
    (g.amount - COALESCE((
        SELECT SUM(t.amount) FROM openrails.ledger_transfers t
        WHERE t.merchant_id = g.merchant_id AND t.grant_id = g.id
          AND t.transfer_type IN ('credit_spend', 'credit_expire')
    ), 0))::bigint AS remaining
FROM openrails.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND g.customer_id = sqlc.arg(customer_id)::uuid
  AND g.kind = 'credit' AND g.event = 'grant' AND g.currency = sqlc.arg(currency)::text
  AND g.ends_at IS NOT NULL AND g.ends_at <= sqlc.arg(as_of)::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants tt WHERE tt.supersedes_id = g.id AND tt.event IN ('revoke', 'supersede')
  )
ORDER BY g.ends_at ASC;

-- ListCustomersWithLapsedCreditLots: distinct (merchant, customer, currency) that
-- have at least one past-expiry credit lot with an unspent remainder — the work
-- list for the credit-expiry job's per-customer ExpireLapsed sweep. Bounded batch.
-- name: ListCustomersWithLapsedCreditLots :many
SELECT DISTINCT g.merchant_id, g.customer_id, g.currency
FROM openrails.grants g
WHERE g.kind = 'credit' AND g.event = 'grant'
  AND g.ends_at IS NOT NULL AND g.ends_at <= sqlc.arg(as_of)::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants tt WHERE tt.supersedes_id = g.id AND tt.event IN ('revoke', 'supersede')
  )
  AND (g.amount - COALESCE((
        SELECT SUM(t.amount) FROM openrails.ledger_transfers t
        WHERE t.merchant_id = g.merchant_id AND t.grant_id = g.id
          AND t.transfer_type IN ('credit_spend', 'credit_expire')
    ), 0)) > 0
LIMIT sqlc.arg(batch_size)::int;

-- name: RevokeEntitlementsByGrant :execrows
UPDATE openrails.entitlements
SET revoked_at = sqlc.arg(revoked_at)::timestamptz,
    revoke_reason = sqlc.arg(revoke_reason)::text,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND grant_id = sqlc.arg(grant_id)::uuid
  AND revoked_at IS NULL AND deleted_at IS NULL;

-- #511 DERIVE `derive.grant.missing` (grant tier): a customer's completed,
-- positive, one-off (non-subscription) payments for a product that PROMISES
-- grants — a non-empty `entitlements_spec` or `credits_spec` — yet produced NO
-- grant at all. "Paid for a grantable product, got nothing." Spec-aware (the
-- positive signal is the product's own grant spec via payment→price→product), so
-- empty-spec products / pure fees are never flagged; subscription payments are
-- excluded (their entitlement grant is subscription-sourced, not payment-linked);
-- refund rows are negative-amount (amount > 0 excludes them); a refunded purchase
-- still has its `grant` event (existence, not liveness), so it is not flagged.
-- Surface-only ADMIN: auto-granting re-runs derive-1 (owned by the purchase path).
-- customer_id is nullable (#575): the inline customer-scoped path passes it (fast
-- indexed seek); the merchant-wide convergence sweep passes NULL (one anti-join
-- over the whole merchant instead of one query per grant-holder).
-- name: ListUngrantedGrantablePayments :many
SELECT p.id, p.amount, p.currency
FROM openrails.payments p
JOIN openrails.prices pr ON pr.id = p.price_id AND pr.merchant_id = p.merchant_id
JOIN openrails.products pd ON pd.id = pr.product_id AND pd.merchant_id = p.merchant_id
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR p.customer_id = sqlc.narg(customer_id)::uuid)
  AND p.deleted_at IS NULL
  AND p.status = 'completed'
  AND p.amount > 0
  AND p.subscription_id IS NULL
  AND (
        (pd.entitlements_spec IS NOT NULL AND pd.entitlements_spec <> '{}'::jsonb)
     OR (pd.credits_spec IS NOT NULL AND pd.credits_spec <> '{}'::jsonb)
      )
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants g
      WHERE g.merchant_id = p.merchant_id AND g.event = 'grant'
        AND (g.payment_id = p.id OR g.source_id = p.id::text)
  )
ORDER BY p.id;

-- #511 DERIVE `derive.grant.excess`: a customer's LIVE grants whose backing
-- payment was refunded — the source no longer justifies the grant (money came
-- back, access is still live). Surface-only ADMIN: a refund that intentionally
-- keeps access (goodwill) is legitimate, so an operator decides; the source
-- (refund) is a PRESENT recorded fact, so this is NOT confirmed-absence-gated.
-- customer_id is nullable (#575): NULL = merchant-wide sweep.
-- name: ListLiveGrantsWithRefundedPayment :many
SELECT g.id, g.kind, g.payment_id
FROM openrails.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR g.customer_id = sqlc.narg(customer_id)::uuid)
  AND g.event = 'grant'
  AND g.payment_id IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants tt
      WHERE tt.supersedes_id = g.id AND tt.event IN ('revoke', 'expire', 'supersede')
  )
  AND EXISTS (
      SELECT 1 FROM openrails.payments p
      WHERE p.id = g.payment_id AND p.merchant_id = g.merchant_id AND p.deleted_at IS NULL AND p.status = 'refunded'
  )
ORDER BY g.id;

-- #511/#575 DERIVE `derive.grant_effect.missing` as a single set query: live
-- (un-terminated) entitlement/credit grants whose derived effect is NOT fully
-- materialized. Mirrors the Go isMaterialized exactly — entitlement: some spec
-- feature has no entitlement row (deleted_at IS NULL, revoked rows still count as
-- materialized); credit: no #512 deposit transfer. ownership has no effect (never
-- flagged). customer_id nullable: NULL = merchant-wide sweep. Repair =
-- MaterializeGrant (idempotent), so re-running converges to empty.
-- name: ListLiveGrantsMissingEffects :many
SELECT g.* FROM openrails.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR g.customer_id = sqlc.narg(customer_id)::uuid)
  AND g.event = 'grant'
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants t
      WHERE t.supersedes_id = g.id AND t.event IN ('revoke', 'expire', 'supersede')
  )
  AND (
    (g.kind = 'entitlement' AND EXISTS (
        SELECT 1 FROM jsonb_array_elements_text(
                 COALESCE(g.spec_snapshot->'entitlements', '[]'::jsonb)) AS feat
        WHERE NOT EXISTS (
            SELECT 1 FROM openrails.entitlements e
            WHERE e.merchant_id = g.merchant_id AND e.grant_id = g.id
              AND e.entitlement = feat AND e.deleted_at IS NULL)
          -- #691: one live STANDING subscription window satisfies every
          -- per-period grant of that subscription (mirrors MaterializeGrant's
          -- ensure-standing skip — detection and repair must agree or the
          -- sweep never converges).
          AND NOT (g.source_type = 'subscription' AND EXISTS (
            SELECT 1 FROM openrails.entitlements e2
            WHERE e2.merchant_id = g.merchant_id
              AND e2.customer_id = g.customer_id
              AND e2.entitlement = feat
              AND e2.source_type = 'subscription'
              AND e2.source_id::text = g.source_id
              AND e2.end_at IS NULL
              AND e2.revoked_at IS NULL AND e2.deleted_at IS NULL))
          -- #695 absent-by-overlap: a LIVE window (any source) overlapping the
          -- grant's own [starts_at, ends_at) means MaterializeGrant deliberately
          -- projected NO window for it (the grant is provenance-only; the window
          -- is overlap-constrained). Mirrors derive-2's overlap no-op — detection
          -- and repair must agree or the sweep never converges.
          AND NOT EXISTS (
            SELECT 1 FROM openrails.entitlements e3
            WHERE e3.merchant_id = g.merchant_id
              AND e3.customer_id = g.customer_id
              AND e3.entitlement = feat
              AND e3.revoked_at IS NULL AND e3.deleted_at IS NULL
              AND e3.period && tstzrange(g.starts_at, COALESCE(g.ends_at, 'infinity'::timestamptz), '[)'))))
    OR
    (g.kind = 'credit' AND NOT EXISTS (
        SELECT 1 FROM openrails.ledger_transfers lt
        WHERE lt.merchant_id = g.merchant_id AND lt.transfer_type = 'deposit' AND lt.grant_id = g.id))
  )
ORDER BY g.created_at;

-- #511/#575 DERIVE `derive.grant_effect.excess` as a single set query: TERMINATED
-- grants whose derived effect is still live (a revoke/expire recorded but its
-- retraction never propagated). Mirrors Go IsGrantTerminated + effectStillLive —
-- entitlement: a non-revoked, non-deleted entitlement row; credit: lot remainder
-- (amount − spend/expire/revoke transfers) > 0. customer_id nullable: NULL =
-- merchant-wide sweep. Repair = MaterializeGrant (retracts) — idempotent.
-- name: ListUnretractedTerminations :many
SELECT g.* FROM openrails.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR g.customer_id = sqlc.narg(customer_id)::uuid)
  AND g.event = 'grant'
  AND EXISTS (
      SELECT 1 FROM openrails.grants t
      WHERE t.merchant_id = g.merchant_id AND t.supersedes_id = g.id
        AND t.event IN ('revoke', 'expire', 'supersede')
  )
  AND (
    (g.kind = 'entitlement' AND EXISTS (
        SELECT 1 FROM openrails.entitlements e
        WHERE e.merchant_id = g.merchant_id AND e.grant_id = g.id
          AND e.revoked_at IS NULL AND e.deleted_at IS NULL))
    OR
    (g.kind = 'credit' AND (
        g.amount - COALESCE((
            SELECT SUM(t.amount) FROM openrails.ledger_transfers t
            WHERE t.merchant_id = g.merchant_id AND t.grant_id = g.id
              AND t.transfer_type IN ('credit_spend', 'credit_expire', 'credit_revoke')
        ), 0)) > 0)
  )
ORDER BY g.created_at;

-- #511 ownership-on-grants: live (un-terminated) ownership grant ids backing a
-- payment, so a refund/chargeback can revoke product access for that payment.
-- name: ListLiveOwnershipGrantIDsByPayment :many
SELECT g.id FROM openrails.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND g.payment_id = sqlc.arg(payment_id)::uuid
  AND g.kind = 'ownership'
  AND g.event = 'grant'
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants t
      WHERE t.supersedes_id = g.id AND t.event IN ('revoke', 'expire', 'supersede')
  );

-- #511 ownership-on-grants: every ownership grant-event for a customer with its
-- derived status (the termination event, if any) — so the legacy
-- ProductAccessGrant.{status,revoked_at,revoke_reason} shape can be reconstructed
-- from the append-only ledger in one query.
-- #658: revoked_at is the termination's EFFECTIVE instant (valid time on
-- term.starts_at), not term.created_at (transaction time), so a backdated/grace
-- revocation reports when access actually ended. Historically the two coincided.
-- name: ListOwnershipGrantsWithStatus :many
SELECT g.id, g.merchant_id, g.customer_id, g.product_id, g.source_type, g.source_id,
       g.payment_id, g.starts_at, g.ends_at, g.created_at,
       term.starts_at AS revoked_at, term.reason AS revoke_reason
FROM openrails.grants g
LEFT JOIN openrails.grants term
  ON term.supersedes_id = g.id AND term.event IN ('revoke', 'expire', 'supersede')
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND g.customer_id = sqlc.arg(customer_id)::uuid
  AND g.kind = 'ownership'
  AND g.event = 'grant'
ORDER BY g.created_at;

-- #631 DERIVE `derive.subscription.missing`: subscriptions in an access-
-- granting state (active/cancelled/unknown) for a product that PROMISES entitlements,
-- with NO subscription-sourced grant yet. After the migrate/convergence split the
-- doujins migrate moves subscriptions as source-of-truth (#724) but no longer
-- writes their entitlements — derive-1 materializes the grant + entitlement window
-- from the stored subscription. Window is computed Go-side (mirrors the retired
-- migrate logic): [COALESCE(current_period_starts_at,started_at),
-- COALESCE(current_period_ends_at,ended_at)). active+cancelled+unknown grant
-- access (pending/expired/failed/past_due do not). #716 fail-open: `unknown`
-- matches SubscriptionProjectsStandingAccess — an imported-as-unknown sub gets
-- its entitlement while the resolution machinery finds the truth. #717:
-- cancel_type='chargeback' grants NO runway — money reversed = access reversed.
-- Bounded to windows ending within
-- scan_since (3y) — a past-ended window is harmless but skipping ancient ones
-- keeps the sweep cheap. customer_id nullable (#575): NULL = merchant-wide sweep.
-- name: ListUngrantedSubscriptions :many
SELECT s.id, s.customer_id, s.product_id, s.status,
       s.current_period_starts_at, s.current_period_ends_at, s.started_at, s.ended_at,
       pd.entitlements_spec
FROM openrails.subscriptions s
JOIN openrails.products pd ON pd.id = s.product_id AND pd.merchant_id = s.merchant_id
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR s.customer_id = sqlc.narg(customer_id)::uuid)
  AND s.deleted_at IS NULL
  AND s.status IN ('active', 'cancelled', 'unknown')
  AND NOT (s.status = 'cancelled' AND s.cancel_type = 'chargeback')
  AND pd.entitlements_spec IS NOT NULL AND pd.entitlements_spec <> '{}'::jsonb
  AND COALESCE(s.current_period_starts_at, s.started_at) < COALESCE(s.current_period_ends_at, s.ended_at)
  AND COALESCE(s.current_period_ends_at, s.ended_at) >= sqlc.arg(scan_since)::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants g
      WHERE g.merchant_id = s.merchant_id AND g.event = 'grant'
        AND g.source_type = 'subscription' AND g.source_id = s.id::text
  )
ORDER BY COALESCE(s.current_period_starts_at, s.started_at);

-- #631 DERIVE `derive.wallet.missing`: completed solana wallet payments
-- carrying a stored access window (metadata.expiration_rfc3339) for a grantable
-- product, with NO grant yet. The doujins migrate moved these payments as
-- source-of-truth (rail=solana, amount>0) but no longer derives their membership
-- entitlement — derive-1 materializes grant + window [purchased_at,
-- expiration_rfc3339). Distinct from the general `derive.grant.missing` (payments)
-- ADMIN finding: the explicit stored expiration makes the window unambiguous, so
-- this migrated cohort auto-repairs instead of waiting for an operator.
-- name: ListUngrantedWalletPayments :many
SELECT p.id, p.customer_id, p.purchased_at,
       (p.metadata->>'expiration_rfc3339')::timestamptz AS expires_at,
       pd.entitlements_spec
FROM openrails.payments p
JOIN openrails.prices pr ON pr.id = p.price_id AND pr.merchant_id = p.merchant_id
JOIN openrails.products pd ON pd.id = pr.product_id AND pd.merchant_id = p.merchant_id
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR p.customer_id = sqlc.narg(customer_id)::uuid)
  AND p.deleted_at IS NULL
  AND p.rail = 'solana'
  AND p.status = 'completed'
  AND p.amount > 0
  AND p.subscription_id IS NULL
  AND p.metadata->>'expiration_rfc3339' IS NOT NULL
  AND p.metadata->>'expiration_rfc3339' ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}'
  AND (p.metadata->>'expiration_rfc3339')::timestamptz > p.purchased_at
  AND (p.metadata->>'expiration_rfc3339')::timestamptz >= sqlc.arg(scan_since)::timestamptz
  AND pd.entitlements_spec IS NOT NULL AND pd.entitlements_spec <> '{}'::jsonb
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants g
      WHERE g.merchant_id = p.merchant_id AND g.event = 'grant'
        AND ((g.source_type = 'purchase' AND g.source_id = p.id::text) OR g.payment_id = p.id)
  )
ORDER BY p.purchased_at;

-- #631 overlap precheck for derive-1: does the customer already hold a LIVE
-- (non-revoked, non-deleted) entitlement for this feature whose window overlaps
-- [lower, upper)? derive-1 SKIPs the grant when true, so it never trips the
-- entitlements_customer_no_overlap exclusion constraint. Half-open [) semantics
-- match the constraint (abutting monthly windows do NOT overlap).
-- name: EntitlementWindowOverlaps :one
SELECT EXISTS (
    SELECT 1 FROM openrails.entitlements e
    WHERE e.merchant_id = sqlc.arg(merchant_id)::uuid
      AND e.customer_id = sqlc.arg(customer_id)::uuid
      AND e.entitlement = sqlc.arg(entitlement)::text
      AND e.revoked_at IS NULL
      AND e.deleted_at IS NULL
      AND e.period && tstzrange(sqlc.arg(lower_bound)::timestamptz, sqlc.arg(upper_bound)::timestamptz, '[)')
) AS overlaps;

-- #691 per-period grant idempotency: the latest bounded end recorded for one
-- (customer, source, feature) — a renewal replay whose period end is not past
-- this mark appends nothing.
-- name: LatestEntitlementGrantEndForSource :one
-- Zero time = no bounded grant recorded yet.
SELECT COALESCE(max(g.ends_at), '0001-01-01 00:00:00+00'::timestamptz)::timestamptz AS latest_end
FROM openrails.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND g.customer_id = sqlc.arg(customer_id)::uuid
  AND g.kind = 'entitlement' AND g.event = 'grant'
  AND g.source_type = sqlc.arg(source_type)::text
  AND g.source_id = sqlc.arg(source_id)::text
  AND g.ends_at IS NOT NULL
  AND jsonb_exists(COALESCE(g.spec_snapshot->'entitlements', '[]'::jsonb), sqlc.arg(entitlement)::text);

-- #636 idempotency for admin-grant import: is there already an entitlement grant
-- from this admin source? Lets the doujins migrate hand admin comps over as grants
-- (source_type=admin) and re-run safely — convergence derive-2 projects the
-- entitlement, so doujins never writes entitlements directly.
-- name: AdminGrantExistsForSource :one
SELECT EXISTS (
    SELECT 1 FROM openrails.grants g
    WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
      AND g.event = 'grant' AND g.kind = 'entitlement'
      AND g.source_type = 'admin' AND g.source_id = sqlc.arg(source_id)::text
) AS exists;

-- CROSS-MERCHANT: merchants holding a lapsed credit lot, through migration
-- 0022's SECURITY DEFINER reader (or#868 B1). The credit-expiry worker used to
-- run ListCustomersWithLapsedCreditLots inside a bare RunInTx on the base pool;
-- grants FORCEs RLS, so it enumerated nothing and NO credit lot has ever been
-- clawed back. Ids only — the per-customer work list and the ledger transfers
-- run per-merchant under RunInMerchantConn.
-- name: ListLapsedCreditLotMerchants :many
SELECT merchant_id FROM openrails.lapsed_credit_lot_merchant_ids(
    sqlc.arg(as_of)::timestamptz,
    sqlc.arg(merchant_limit)::int);
