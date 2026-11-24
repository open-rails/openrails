-- name: InsertOperationLogEntry :exec
-- Stamped by the database clock when the writer names no time, so entries
-- order with the holds below.
INSERT INTO billing.operation_log (merchant_id, occurred_at, actor, source, object_type, object_id, action, changes, reason, request_id, operation_id,
    rail, psp_id, custodian_id, subscription_id)
VALUES (sqlc.arg(merchant_id)::uuid, COALESCE(sqlc.narg(occurred_at)::timestamptz, clock_timestamp()), sqlc.arg(actor)::text, sqlc.arg(source)::text,
    sqlc.arg(object_type)::text, sqlc.arg(object_id)::text, sqlc.arg(action)::text, sqlc.arg(changes)::jsonb,
    sqlc.narg(reason)::text, sqlc.narg(request_id)::text, sqlc.narg(operation_id)::uuid,
    sqlc.arg(rail)::text, sqlc.narg(psp_id)::uuid, sqlc.narg(custodian_id)::uuid, sqlc.narg(subscription_id)::uuid);

-- name: RecordSubscriptionOperationHold :exec
-- Timestamp after acquiring the row lock: transaction start time could order
-- a waiting transaction before the hold it followed.
-- The caller already updated (and therefore locked) the intent in this transaction.
-- Unknown readbacks do not end a hold. Only attempted execution or a terminal
-- result separates two observations of the same hold.
INSERT INTO billing.operation_log
 (merchant_id,occurred_at,actor,source,object_type,object_id,action,changes,operation_id,rail,psp_id,custodian_id,subscription_id)
SELECT i.merchant_id,clock_timestamp(),
 CASE
  WHEN i.origin='admin' AND i.actor<>'' THEN 'staff:'||i.actor
  WHEN i.origin='user' AND i.actor<>'' THEN 'customer:'||i.actor
  WHEN i.origin='provider' THEN 'provider:'||i.rail
  ELSE 'system'
 END,
 'intent','provider_intent',i.id::text,'parked',
 jsonb_strip_nulls(jsonb_build_object('version',1,'kind',i.intent_type,'attempt',i.attempts,'origin',i.origin,
  'executor',NULLIF(sqlc.arg(executor)::text,''),'reason_code',sqlc.arg(reason_code)::text)),
 i.id,i.rail,i.psp_id,i.custodian_id,i.subscription_id
FROM billing.provider_intents i
WHERE i.merchant_id=sqlc.arg(merchant_id)::uuid AND i.id=sqlc.arg(id)::uuid AND i.subscription_id IS NOT NULL
 AND NOT EXISTS (
  SELECT 1 FROM (
   SELECT l.action,l.changes->>'reason_code' AS reason_code FROM billing.operation_log l
   WHERE l.merchant_id=i.merchant_id AND l.operation_id=i.id
     AND l.action IN ('parked','attempting','succeeded','failed')
   ORDER BY l.occurred_at DESC,l.id DESC LIMIT 1
  ) last WHERE last.action='parked' AND last.reason_code=sqlc.arg(reason_code)::text
 );

-- name: ListOperationLog :many
-- Newest first, by rail, operation, PSP and phase.
SELECT * FROM billing.operation_log
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(operation_id)::uuid IS NULL OR operation_id = sqlc.narg(operation_id)::uuid)
  AND (sqlc.narg(psp_id)::uuid IS NULL OR psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(action)::text IS NULL OR action = sqlc.narg(action)::text)
ORDER BY occurred_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: CountOperationLog :one
SELECT count(*) FROM billing.operation_log
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(operation_id)::uuid IS NULL OR operation_id = sqlc.narg(operation_id)::uuid)
  AND (sqlc.narg(psp_id)::uuid IS NULL OR psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(action)::text IS NULL OR action = sqlc.narg(action)::text);
