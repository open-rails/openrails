-- name: CreateCapturedPaymentMethod :execrows
INSERT INTO billing.payment_methods
(id,merchant_id,customer_id,rail,custodian,custodian_id,charge_via,status,setup_ref,capture,metadata,created_at,updated_at)
VALUES(sqlc.arg(id)::uuid,sqlc.arg(merchant_id)::uuid,sqlc.arg(customer_id)::uuid,'nmi','hyperswitch',sqlc.arg(custodian_id)::uuid,'pan_proxy','requires_action',sqlc.arg(setup_ref)::text,sqlc.arg(capture)::jsonb,sqlc.arg(metadata)::jsonb,sqlc.arg(now)::timestamptz,sqlc.arg(now)::timestamptz)
ON CONFLICT (merchant_id,id) DO NOTHING;

-- name: GetCapturedPaymentMethodForUpdate :one
SELECT * FROM billing.payment_methods
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid AND capture IS NOT NULL
FOR UPDATE;

-- name: GetCapturedInstrument :one
SELECT * FROM billing.payment_methods
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND custodian_id=sqlc.arg(custodian_id)::uuid
AND rail_customer_ref=sqlc.arg(vendor_customer_id)::text AND rail_method_ref=sqlc.arg(vendor_method_id)::text
AND custodian='hyperswitch' AND psp_id IS NULL;

-- name: CompleteCapturedPaymentMethod :one
-- The pending resource becomes the instrument, or points at the same customer's
-- already stored instrument. Replaying never reactivates a removed card.
WITH saved AS (
UPDATE billing.payment_methods SET
    status=CASE WHEN sqlc.narg(replacement)::uuid IS NULL THEN 'active' ELSE 'replaced' END,
    replaced_by_id=sqlc.narg(replacement)::uuid, setup_ref=NULL, capture=sqlc.arg(capture)::jsonb,
    rail_customer_ref=CASE WHEN sqlc.narg(replacement)::uuid IS NULL THEN sqlc.arg(vendor_customer_id)::text ELSE NULL END,
    rail_method_ref=CASE WHEN sqlc.narg(replacement)::uuid IS NULL THEN sqlc.arg(vendor_method_id)::text ELSE NULL END,
    card_brand=sqlc.narg(card_brand)::text,card_last4=sqlc.narg(card_last4)::text,
    card_exp_month=sqlc.narg(card_exp_month)::smallint,card_exp_year=sqlc.narg(card_exp_year)::smallint,
    updated_at=sqlc.arg(now)::timestamptz
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid AND status='requires_action'
RETURNING *
), history AS (
INSERT INTO billing.payment_method_versions (merchant_id,customer_id,payment_method_id,source,kind,event_ref,custodian_id,
    rail_customer_ref,rail_method_ref,card_brand,card_last4,card_exp_month,card_exp_year,effective_at)
SELECT merchant_id,customer_id,id,'customer_save','saved','created',custodian_id,
    rail_customer_ref,rail_method_ref,NULLIF(card_brand,''),card_last4,card_exp_month,card_exp_year,updated_at
FROM saved WHERE status='active'
ON CONFLICT (merchant_id,payment_method_id,source,event_ref) DO NOTHING
)
SELECT count(*) FROM saved;

-- name: ExpireCapturedPaymentMethod :execrows
UPDATE billing.payment_methods SET status='removed',setup_ref=NULL,
    capture=capture-'secret_ciphertext',updated_at=sqlc.arg(now)::timestamptz
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid
AND status='requires_action' AND capture IS NOT NULL
AND (capture->>'expires_at')::timestamptz<=sqlc.arg(now)::timestamptz;

-- name: CountInvalidPaymentMethodCaptureReferences :one
SELECT count(*) FROM billing.payment_methods pm
WHERE pm.merchant_id=sqlc.arg(merchant_id)::uuid AND pm.capture IS NOT NULL
AND (
 NOT EXISTS(SELECT 1 FROM billing.custodians c WHERE c.merchant_id=pm.merchant_id AND c.id::text=pm.capture->>'custodian_id' AND c.kind='hyperswitch' AND c.account_id=pm.capture->>'account_id')
 OR EXISTS(SELECT 1 FROM billing.payment_methods target WHERE target.merchant_id=pm.merchant_id AND target.customer_id<>pm.customer_id AND target.id::text=pm.capture->>'payment_method_id')
);
