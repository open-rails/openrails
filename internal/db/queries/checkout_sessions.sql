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
SELECT * FROM openrails.checkout_sessions WHERE checkout_sessions.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1
  AND deleted_at IS NULL;

-- name: LockCheckoutSessionForShare :one
SELECT id FROM openrails.checkout_sessions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND deleted_at IS NULL
FOR SHARE;

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
    rail_state = COALESCE(sqlc.narg(rail_state)::jsonb, '{}'::jsonb)
      || CASE WHEN rail_state ? 'accepted_purchase' THEN jsonb_build_object('accepted_purchase', rail_state->'accepted_purchase') ELSE '{}'::jsonb END
      || CASE WHEN rail_state->>'purchase_submitted'='true' THEN '{"purchase_submitted":true}'::jsonb ELSE '{}'::jsonb END
      || CASE WHEN rail_state->>'provider_closed'='true' THEN '{"provider_closed":true}'::jsonb ELSE '{}'::jsonb END,
    psp_id = sqlc.arg(psp_id)::uuid,
    updated_at = sqlc.arg(updated_at)
WHERE checkout_sessions.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1
  AND deleted_at IS NULL
  AND (NOT COALESCE(rail_state ? 'accepted_purchase', false)
       OR status <> 'succeeded' OR $6 = 'succeeded')
  AND (NOT COALESCE(rail_state ? 'accepted_purchase', false)
       OR (customer_id=$2 AND price_id IS NOT DISTINCT FROM $3 AND mode=$4 AND rail=$5
           AND amount IS NOT DISTINCT FROM $7 AND currency IS NOT DISTINCT FROM $8
           AND psp_id=sqlc.arg(psp_id)::uuid))
  AND (NOT COALESCE((rail_state->>'provider_closed')::boolean, false)
       OR COALESCE((sqlc.narg(rail_state)::jsonb->>'provider_closed')::boolean, false));

-- name: BindSolanaCheckoutSession :execrows
UPDATE openrails.checkout_sessions SET
    reference = sqlc.arg(reference),
    rail_state = sqlc.arg(rail_state),
    updated_at = sqlc.arg(updated_at)
WHERE checkout_sessions.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1
  AND rail = 'solana'
  AND status = 'requires_action'
  AND (reference IS NULL OR reference = sqlc.arg(reference))
  AND (COALESCE(rail_state ->> 'payer', '') = '' OR rail_state ->> 'payer' = sqlc.arg(payer)::text)
  AND deleted_at IS NULL;

-- name: GetCheckoutSessionByReference :one
SELECT * FROM openrails.checkout_sessions cs
WHERE cs.merchant_id = sqlc.arg(merchant_id)::uuid AND cs.reference = $1
  AND cs.deleted_at IS NULL
LIMIT 1;

-- name: GetLatestOpenCheckoutSession :one
SELECT * FROM openrails.checkout_sessions cs
WHERE cs.merchant_id = sqlc.arg(merchant_id)::uuid AND cs.customer_id = $1
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
-- legitimate instrument remap. A still-present method must belong to this payer; its legitimate later
-- deletion leaves historical replay intact and does not recreate the method.
SELECT count(*) FROM openrails.checkout_sessions cs
WHERE cs.merchant_id=sqlc.arg(merchant_id)::uuid AND cs.mode='payment_method' AND cs.rail='nmi'
AND (
 NOT EXISTS(SELECT 1 FROM openrails.custodians c WHERE c.merchant_id=cs.merchant_id AND c.id::text=cs.rail_state#>>'{capture,custodian_id}' AND c.kind='hyperswitch' AND c.account_id=cs.rail_state#>>'{capture,account_id}')
 OR (cs.status='succeeded' AND EXISTS(SELECT 1 FROM openrails.payment_methods pm WHERE pm.merchant_id=cs.merchant_id AND pm.customer_id<>cs.customer_id AND pm.id::text=cs.rail_state#>>'{capture,payment_method_id}'))
);

-- name: GetCheckoutCaptureAccountsForShare :one
-- Recheck current authority after vendor metadata readback, inside only the
-- short local attachment transaction. Archive/reconfiguration serializes here.
SELECT sqlc.embed(p),sqlc.embed(c) FROM openrails.psps p
JOIN openrails.custodians c ON c.id=p.custodian_id AND c.merchant_id=p.merchant_id
WHERE p.merchant_id=sqlc.arg(merchant_id)::uuid AND p.id=sqlc.arg(psp_id)::uuid
FOR SHARE OF p,c;

-- name: CountInvalidStripeSetupReferences :one
SELECT count(*) FROM openrails.checkout_sessions cs
WHERE cs.merchant_id=sqlc.arg(merchant_id)::uuid AND cs.mode='payment_method' AND cs.rail='stripe' AND cs.status='succeeded'
AND EXISTS(SELECT 1 FROM openrails.payment_methods pm WHERE pm.merchant_id=cs.merchant_id AND pm.id::text=cs.rail_state->>'payment_method_id' AND pm.customer_id<>cs.customer_id);

-- name: CountInvalidEngineCheckoutReferences :one
SELECT count(*) FROM openrails.checkout_sessions cs
LEFT JOIN openrails.rail_intents i ON i.merchant_id=cs.merchant_id
 AND i.payload->>'checkout_session_id'=cs.id::text AND i.intent_type='initial_membership'
WHERE cs.merchant_id=sqlc.arg(merchant_id)::uuid AND cs.rail_state ? 'initial_membership_quote'
AND ((cs.status='succeeded' AND (i.id IS NULL OR i.status<>'succeeded'))
 OR (i.id IS NOT NULL AND (
   i.rail<>cs.rail OR i.psp_id<>cs.psp_id OR i.price_id<>cs.price_id OR i.payload->'terms'->>'customer_id'<>cs.customer_id::text
   OR (i.status='succeeded' AND (cs.status<>'succeeded' OR cs.subscription_id::text IS DISTINCT FROM i.payload->'terms'->>'subscription_id' OR cs.payment_id::text IS DISTINCT FROM i.payload->'terms'->>'payment_id'))
   OR (i.status='failed_terminal' AND cs.status<>'failed'))));
-- name: LockPurchasableCheckoutPrice :one
SELECT p.id FROM openrails.prices p
JOIN openrails.products product ON product.id=p.product_id AND product.merchant_id=p.merchant_id
WHERE p.id=sqlc.arg(price_id)::uuid AND p.merchant_id=sqlc.arg(merchant_id)::uuid
  AND NOT p.archived AND NOT product.archived
FOR SHARE OF p, product;

-- Claim once before sending a hosted purchase to Stripe. A crash or transport
-- failure after this point has an unknown provider outcome; it is not a license
-- to create another payable session after provider idempotency retention ends.
-- name: ClaimHostedPurchaseDispatch :execrows
UPDATE openrails.checkout_sessions
SET rail_state = rail_state || '{"purchase_submitted":true}'::jsonb
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid
  AND deleted_at IS NULL AND mode='one_off' AND rail='stripe'
  AND status IN ('created','failed') AND rail_state ? 'accepted_purchase'
  AND NOT COALESCE((rail_state->>'purchase_submitted')::boolean, false)
  AND NOT COALESCE((rail_state->>'provider_closed')::boolean, false);

-- Validation failed before dispatch, or the dispatched request had an unknown
-- outcome. Decide from the persisted claim atomically, never a stale Go copy.
-- name: FailHostedPurchaseInitialization :execrows
UPDATE openrails.checkout_sessions
SET status='failed', updated_at=sqlc.arg(now)::timestamptz,
    rail_state=rail_state || jsonb_build_object('failure_reason', sqlc.arg(reason)::text)
      || CASE WHEN NOT COALESCE((rail_state->>'purchase_submitted')::boolean, false)
              THEN '{"provider_closed":true}'::jsonb ELSE '{}'::jsonb END
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid
  AND deleted_at IS NULL AND mode='one_off' AND rail='stripe' AND rail_state ? 'accepted_purchase'
  AND status<>'succeeded';

-- name: CloseHostedCheckoutFromProvider :execrows
UPDATE openrails.checkout_sessions
SET status=CASE WHEN status='succeeded' THEN status ELSE sqlc.arg(status)::text END,
    rail_state=COALESCE(rail_state, '{}'::jsonb) || '{"provider_closed":true}'::jsonb,
    updated_at=sqlc.arg(now)::timestamptz
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid
  AND psp_id=sqlc.arg(psp_id)::uuid AND rail='stripe' AND deleted_at IS NULL;

-- A local expiry or failed HTTP request does not prove a provider cannot charge.
-- Only a completed purchase or authoritative provider cancellation releases a
-- hosted session. NMI's accepted operation owns uncertainty after submission.
-- name: HasUnresolvedProductCheckout :one
SELECT EXISTS (
 SELECT 1 FROM openrails.checkout_sessions s
 JOIN openrails.prices p ON p.id=s.price_id AND p.merchant_id=s.merchant_id
 WHERE s.merchant_id=sqlc.arg(merchant_id)::uuid
   AND s.customer_id=sqlc.arg(customer_id)::uuid
   AND p.product_id=sqlc.arg(product_id)::uuid
   AND s.id<>sqlc.arg(except_session_id)::uuid AND s.mode='one_off'
   AND s.status<>'succeeded'
   AND (s.status IN ('created','requires_action')
     OR (s.rail='stripe' AND NOT COALESCE((s.rail_state->>'provider_closed')::boolean, false)))
);
