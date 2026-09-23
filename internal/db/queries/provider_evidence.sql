-- Provider-neutral evidence captured by read-only pull fetchers. These facts
-- are deliberately separate from billing mirror tables: reports can inspect
-- provider truth without pretending OpenRails initiated or settled a charge.

-- name: CreateProviderEvidenceSnapshot :one
INSERT INTO openrails.provider_evidence_snapshots (
    merchant_id, reconciliation_run_id, provider, psp_id, fetched_at,
    window_since, window_until, capabilities, coverage
) VALUES (
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(reconciliation_run_id)::uuid,
    sqlc.arg(provider)::text,
    sqlc.arg(psp_id)::uuid,
    sqlc.arg(fetched_at)::timestamptz,
    sqlc.narg(window_since)::timestamptz,
    sqlc.narg(window_until)::timestamptz,
    sqlc.arg(capabilities)::jsonb,
    sqlc.arg(coverage)::jsonb
)
ON CONFLICT (merchant_id, reconciliation_run_id, provider, psp_id)
DO UPDATE SET
    fetched_at = EXCLUDED.fetched_at,
    window_since = EXCLUDED.window_since,
    window_until = EXCLUDED.window_until,
    capabilities = EXCLUDED.capabilities,
    coverage = EXCLUDED.coverage
RETURNING *;

-- name: UpsertProviderEvidenceTransaction :one
INSERT INTO openrails.provider_evidence_transactions (
    merchant_id, psp_id, provider, event_key, transaction_id,
    subscription_ref, type, success, amount_cents, currency, occurred_at,
    source, customer_ref, customer_email, order_ref, decline_code,
    decline_reason, raw, first_snapshot_id, last_snapshot_id,
    first_seen_at, last_seen_at
) VALUES (
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(psp_id)::uuid,
    sqlc.arg(provider)::text,
    sqlc.arg(event_key)::text,
    sqlc.arg(transaction_id)::text,
    sqlc.arg(subscription_ref)::text,
    sqlc.arg(type)::text,
    sqlc.arg(success)::boolean,
    sqlc.arg(amount_cents)::bigint,
    sqlc.arg(currency)::text,
    sqlc.arg(occurred_at)::timestamptz,
    sqlc.arg(source)::text,
    sqlc.arg(customer_ref)::text,
    sqlc.arg(customer_email)::text,
    sqlc.arg(order_ref)::text,
    sqlc.arg(decline_code)::text,
    sqlc.arg(decline_reason)::text,
    sqlc.arg(raw)::jsonb,
    sqlc.arg(snapshot_id)::uuid,
    sqlc.arg(snapshot_id)::uuid,
    sqlc.arg(observed_at)::timestamptz,
    sqlc.arg(observed_at)::timestamptz
)
ON CONFLICT (merchant_id, psp_id, provider, event_key)
DO UPDATE SET
    transaction_id = EXCLUDED.transaction_id,
    subscription_ref = EXCLUDED.subscription_ref,
    type = EXCLUDED.type,
    success = EXCLUDED.success,
    amount_cents = EXCLUDED.amount_cents,
    currency = EXCLUDED.currency,
    occurred_at = EXCLUDED.occurred_at,
    source = EXCLUDED.source,
    customer_ref = EXCLUDED.customer_ref,
    customer_email = EXCLUDED.customer_email,
    order_ref = EXCLUDED.order_ref,
    decline_code = EXCLUDED.decline_code,
    decline_reason = EXCLUDED.decline_reason,
    raw = EXCLUDED.raw,
    last_snapshot_id = EXCLUDED.last_snapshot_id,
    last_seen_at = EXCLUDED.last_seen_at
RETURNING *;

-- name: UpsertProviderEvidenceSubscription :one
INSERT INTO openrails.provider_evidence_subscriptions (
    snapshot_id, merchant_id, psp_id, provider, record_key,
    provider_subscription_ref, status, raw_status, customer_ref,
    customer_email, username, plan_ref, next_billing_at, last_billed_at,
    amount_cents, currency, raw
) VALUES (
    sqlc.arg(snapshot_id)::uuid,
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(psp_id)::uuid,
    sqlc.arg(provider)::text,
    sqlc.arg(record_key)::text,
    sqlc.arg(provider_subscription_ref)::text,
    sqlc.arg(status)::text,
    sqlc.arg(raw_status)::text,
    sqlc.arg(customer_ref)::text,
    sqlc.arg(customer_email)::text,
    sqlc.arg(username)::text,
    sqlc.arg(plan_ref)::text,
    sqlc.narg(next_billing_at)::timestamptz,
    sqlc.narg(last_billed_at)::timestamptz,
    sqlc.arg(amount_cents)::bigint,
    sqlc.arg(currency)::text,
    sqlc.arg(raw)::jsonb
)
ON CONFLICT (merchant_id, snapshot_id, record_key)
DO UPDATE SET
    provider_subscription_ref = EXCLUDED.provider_subscription_ref,
    status = EXCLUDED.status,
    raw_status = EXCLUDED.raw_status,
    customer_ref = EXCLUDED.customer_ref,
    customer_email = EXCLUDED.customer_email,
    username = EXCLUDED.username,
    plan_ref = EXCLUDED.plan_ref,
    next_billing_at = EXCLUDED.next_billing_at,
    last_billed_at = EXCLUDED.last_billed_at,
    amount_cents = EXCLUDED.amount_cents,
    currency = EXCLUDED.currency,
    raw = EXCLUDED.raw
RETURNING *;

-- name: UpsertProviderEvidencePaymentMethod :one
INSERT INTO openrails.provider_evidence_payment_methods (
    snapshot_id, merchant_id, psp_id, provider, record_key,
    customer_ref, card_last4, card_expiry, customer_email, raw
) VALUES (
    sqlc.arg(snapshot_id)::uuid,
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(psp_id)::uuid,
    sqlc.arg(provider)::text,
    sqlc.arg(record_key)::text,
    sqlc.arg(customer_ref)::text,
    sqlc.arg(card_last4)::text,
    sqlc.arg(card_expiry)::text,
    sqlc.arg(customer_email)::text,
    sqlc.arg(raw)::jsonb
)
ON CONFLICT (merchant_id, snapshot_id, record_key)
DO UPDATE SET
    customer_ref = EXCLUDED.customer_ref,
    card_last4 = EXCLUDED.card_last4,
    card_expiry = EXCLUDED.card_expiry,
    customer_email = EXCLUDED.customer_email,
    raw = EXCLUDED.raw
RETURNING *;

-- name: ListProviderEvidenceSnapshots :many
SELECT * FROM openrails.provider_evidence_snapshots
WHERE merchant_id = openrails.current_merchant_id()
  AND (sqlc.narg(psp_id)::uuid IS NULL OR psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(provider)::text IS NULL OR provider = sqlc.narg(provider)::text)
  AND (sqlc.narg(from_at)::timestamptz IS NULL OR fetched_at >= sqlc.narg(from_at)::timestamptz)
  AND (sqlc.narg(to_at)::timestamptz IS NULL OR fetched_at < sqlc.narg(to_at)::timestamptz)
ORDER BY fetched_at, id;

-- name: ListProviderEvidenceTransactions :many
SELECT * FROM openrails.provider_evidence_transactions
WHERE merchant_id = openrails.current_merchant_id()
  AND (sqlc.narg(psp_id)::uuid IS NULL OR psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(provider)::text IS NULL OR provider = sqlc.narg(provider)::text)
  AND (sqlc.narg(from_at)::timestamptz IS NULL OR occurred_at >= sqlc.narg(from_at)::timestamptz)
  AND (sqlc.narg(to_at)::timestamptz IS NULL OR occurred_at < sqlc.narg(to_at)::timestamptz)
ORDER BY occurred_at, id;

-- name: ListProviderEvidenceSubscriptions :many
SELECT s.* FROM openrails.provider_evidence_subscriptions s
JOIN openrails.provider_evidence_snapshots p ON p.id = s.snapshot_id
WHERE s.merchant_id = openrails.current_merchant_id()
  AND (sqlc.narg(psp_id)::uuid IS NULL OR s.psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(provider)::text IS NULL OR s.provider = sqlc.narg(provider)::text)
  AND (sqlc.narg(from_at)::timestamptz IS NULL OR p.fetched_at >= sqlc.narg(from_at)::timestamptz)
  AND (sqlc.narg(to_at)::timestamptz IS NULL OR p.fetched_at < sqlc.narg(to_at)::timestamptz)
ORDER BY p.fetched_at, s.id;

-- name: ListCurrentProviderEvidenceSubscriptions :many
SELECT DISTINCT ON (s.provider, s.psp_id, s.provider_subscription_ref)
       s.id, s.snapshot_id, s.merchant_id, s.psp_id, s.provider,
       s.record_key, s.provider_subscription_ref, s.status, s.raw_status,
       s.customer_ref, s.customer_email, s.username, s.plan_ref,
       s.next_billing_at, s.last_billed_at, s.amount_cents, s.currency, s.raw
FROM openrails.provider_evidence_subscriptions s
JOIN openrails.provider_evidence_snapshots p ON p.id = s.snapshot_id
WHERE s.merchant_id = openrails.current_merchant_id()
  AND (sqlc.narg(psp_id)::uuid IS NULL OR s.psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(provider)::text IS NULL OR s.provider = sqlc.narg(provider)::text)
  AND p.fetched_at <= sqlc.arg(to_at)::timestamptz
  AND s.provider_subscription_ref <> ''
ORDER BY s.provider, s.psp_id, s.provider_subscription_ref, p.fetched_at DESC, s.id DESC;
