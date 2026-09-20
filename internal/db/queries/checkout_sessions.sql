-- openrails.checkout_sessions.

-- name: CreateCheckoutSession :execrows
INSERT INTO openrails.checkout_sessions (
    id, merchant_id, customer_id, price_id, mode, rail, status, amount,
    currency, expires_at, reference, transaction_id, payment_id,
    subscription_id, metadata, rail_fields, rail_state, routing_reason,
    psp_id, created_at, updated_at
) VALUES (
    $1, sqlc.arg(merchant_id)::uuid, $2, $3, $4, $5, $6, $7,
    sqlc.arg(currency),
    sqlc.narg(expires_at), sqlc.narg(reference), sqlc.narg(transaction_id),
    sqlc.narg(payment_id), sqlc.narg(subscription_id), sqlc.narg(metadata),
    sqlc.narg(rail_fields), sqlc.narg(rail_state), sqlc.narg(routing_reason),
    sqlc.arg(psp_id)::uuid,
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: GetCheckoutSessionByID :one
SELECT * FROM openrails.checkout_sessions WHERE id = $1
  AND deleted_at IS NULL;

-- name: UpdateCheckoutSession :execrows
UPDATE openrails.checkout_sessions SET
    customer_id = $2,
    price_id = $3,
    mode = $4,
    rail = $5,
    status = $6,
    amount = $7,
    currency = $8,
    expires_at = sqlc.narg(expires_at),
    reference = sqlc.narg(reference),
    transaction_id = sqlc.narg(transaction_id),
    payment_id = sqlc.narg(payment_id),
    subscription_id = sqlc.narg(subscription_id),
    metadata = sqlc.narg(metadata),
    rail_fields = sqlc.narg(rail_fields),
    rail_state = sqlc.narg(rail_state),
    psp_id = sqlc.arg(psp_id)::uuid,
    updated_at = sqlc.arg(updated_at)
WHERE id = $1
  AND deleted_at IS NULL;

-- name: BindSolanaCheckoutSession :execrows
UPDATE openrails.checkout_sessions SET
    reference = sqlc.arg(reference),
    rail_state = sqlc.arg(rail_state),
    updated_at = sqlc.arg(updated_at)
WHERE id = $1
  AND rail = 'solana'
  AND status = 'requires_action'
  AND (reference IS NULL OR reference = sqlc.arg(reference))
  AND (COALESCE(rail_state ->> 'payer', '') = '' OR rail_state ->> 'payer' = sqlc.arg(payer)::text)
  AND deleted_at IS NULL;

-- name: GetCheckoutSessionByReference :one
SELECT * FROM openrails.checkout_sessions cs
WHERE cs.reference = $1
  AND cs.deleted_at IS NULL
LIMIT 1;

-- name: GetLatestOpenCheckoutSession :one
SELECT * FROM openrails.checkout_sessions cs
WHERE cs.customer_id = $1
  AND cs.price_id = $2
  AND cs.rail = $3
  AND cs.status IN ('created', 'requires_action')
  AND (cs.expires_at IS NULL OR cs.expires_at > sqlc.arg(now)::timestamptz)
  AND cs.deleted_at IS NULL
ORDER BY cs.created_at DESC
LIMIT 1;

-- Retention sweep (or#877 B4): one pass per merchant off the directory walk,
-- with the merchant predicate written out so it stays scoped on a BYPASSRLS
-- connection too.
-- or#837: batched — row_limit bounds one statement, the caller loops.
-- name: ExpireCheckoutSessions :execrows
UPDATE openrails.checkout_sessions
SET rail_state = CASE WHEN mode='payment_method' THEN rail_state #- '{capture,secret_ciphertext}' ELSE rail_state END,
    status = 'expired', updated_at = sqlc.arg(now)
WHERE deleted_at IS NULL
  AND ctid IN (
    SELECT cs.ctid FROM openrails.checkout_sessions cs
    WHERE cs.merchant_id = sqlc.arg(merchant_id)::uuid
      AND cs.expires_at IS NOT NULL AND cs.expires_at < sqlc.arg(now)::timestamptz
      AND cs.status IN ('created', 'requires_action')
      AND cs.deleted_at IS NULL
    LIMIT sqlc.arg(row_limit)::int
);

-- #511 LIFE plane (life.checkout_session.stale): expired-but-not-terminal
-- checkout sessions for a scope. Detection (read-only) for the Convergence Engine.
-- name: ListStaleCheckoutSessions :many
SELECT id FROM openrails.checkout_sessions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND expires_at IS NOT NULL AND expires_at < sqlc.arg(now)::timestamptz
  AND status IN ('created', 'requires_action')
  AND deleted_at IS NULL
ORDER BY expires_at;

-- name: ExpireCheckoutSessionByID :execrows
-- Repair for life.checkout_session.stale: mark one stale session expired.
UPDATE openrails.checkout_sessions
SET rail_state = CASE WHEN mode='payment_method' THEN rail_state #- '{capture,secret_ciphertext}' ELSE rail_state END,
    status = 'expired', updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND status IN ('created', 'requires_action')
  AND deleted_at IS NULL;

-- A setup row is addressed by the stable merchant/customer/idempotency-key
-- UUID. The first writer owns its immutable request fingerprint and binding.
-- name: CreatePaymentMethodSetupSession :execrows
INSERT INTO openrails.checkout_sessions
(id,merchant_id,customer_id,psp_id,mode,rail,status,expires_at,rail_state,metadata,created_at,updated_at)
VALUES(sqlc.arg(id),sqlc.arg(merchant_id),sqlc.arg(customer_id),sqlc.arg(psp_id),'payment_method','nmi','created',sqlc.arg(expires_at),sqlc.arg(rail_state),sqlc.arg(metadata),sqlc.arg(now),sqlc.arg(now))
ON CONFLICT (id) DO NOTHING;

-- Only one prepared vendor session is accepted and exposed to the browser.
-- Concurrent losers reload that same action; no accepted session is retargeted.
-- name: AcceptPaymentMethodSetupSession :execrows
UPDATE openrails.checkout_sessions
SET rail_state=jsonb_set(rail_state,'{capture}',sqlc.arg(capture)::jsonb),
    expires_at=sqlc.arg(expires_at),status='requires_action',updated_at=sqlc.arg(now)
WHERE id=sqlc.arg(id) AND merchant_id=sqlc.arg(merchant_id)
  AND mode='payment_method' AND status='created' AND deleted_at IS NULL
  AND expires_at>sqlc.arg(now) AND rail_state->'capture'=sqlc.arg(previous)::jsonb;

-- name: GetPaymentMethodSetupSessionForUpdate :one
SELECT * FROM openrails.checkout_sessions
WHERE id=sqlc.arg(id) AND merchant_id=sqlc.arg(merchant_id)
  AND mode='payment_method' AND deleted_at IS NULL
FOR UPDATE;

-- Completion and erasure of the short-lived secret commit with attachment.
-- name: CompletePaymentMethodSetupSession :execrows
UPDATE openrails.checkout_sessions
SET rail_state=jsonb_set(rail_state,'{capture}',sqlc.arg(capture)::jsonb),
    status='succeeded',updated_at=sqlc.arg(now)
WHERE id=sqlc.arg(id) AND merchant_id=sqlc.arg(merchant_id)
  AND mode='payment_method' AND status='requires_action' AND deleted_at IS NULL
  AND expires_at>sqlc.arg(now);

-- Capture attachment never reparents an existing instrument to another payer.
-- name: AttachCapturedPaymentMethod :one
INSERT INTO openrails.payment_methods
(id,merchant_id,customer_id,psp_id,rail,custodian,custodian_id,rail_customer_ref,rail_method_ref,last_four,card_type,expiry_date,charge_via,initial_transaction_id,created_at,updated_at)
VALUES(sqlc.arg(id),sqlc.arg(merchant_id),sqlc.arg(customer_id),sqlc.arg(psp_id),'nmi','hyperswitch',sqlc.arg(custodian_id),sqlc.arg(vendor_customer_id),sqlc.arg(vendor_method_id),sqlc.arg(last_four),sqlc.arg(card_type),sqlc.arg(expiry_date),'pan_proxy','',sqlc.arg(now),sqlc.arg(now))
ON CONFLICT (merchant_id,psp_id,custodian_id,rail_customer_ref,rail_method_ref)
DO UPDATE SET id=openrails.payment_methods.id
WHERE openrails.payment_methods.customer_id=EXCLUDED.customer_id
  AND openrails.payment_methods.custodian='hyperswitch'
RETURNING *;

-- name: CountInvalidCheckoutCaptureReferences :one
-- Terminal replay retains the original capture authority even after a later
-- legitimate instrument remap. The attached local method must still be owned
-- by this payer; it need not still use the historical custodian/PSP.
SELECT count(*) FROM openrails.checkout_sessions cs
WHERE cs.merchant_id=sqlc.arg(merchant_id)::uuid AND cs.mode='payment_method'
AND (
 NOT EXISTS(SELECT 1 FROM openrails.custodians c WHERE c.merchant_id=cs.merchant_id AND c.id::text=cs.rail_state#>>'{capture,custodian_id}' AND c.kind='hyperswitch' AND c.account_id=cs.rail_state#>>'{capture,account_id}')
 OR (cs.status='succeeded' AND NOT EXISTS(SELECT 1 FROM openrails.payment_methods pm WHERE pm.merchant_id=cs.merchant_id AND pm.customer_id=cs.customer_id AND pm.id::text=cs.rail_state#>>'{capture,payment_method_id}'))
);
