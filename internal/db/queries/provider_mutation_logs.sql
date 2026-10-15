-- name: InsertProviderMutationLog :exec
INSERT INTO billing.provider_mutation_logs (
 merchant_id,rail,psp_id,custodian_id,provider_intent_id,intent_type,idempotency_key,attempt,phase,reason,evidence,
 subscription_id,operation_id,origin,actor,executor,created_at
)
SELECT sqlc.arg(merchant_id)::uuid,sqlc.arg(rail)::text,sqlc.narg(psp_id)::uuid,sqlc.narg(custodian_id)::uuid,
 sqlc.narg(provider_intent_id)::uuid,sqlc.narg(intent_type)::text,sqlc.narg(idempotency_key)::text,
 sqlc.arg(attempt)::int,sqlc.arg(phase)::text,sqlc.narg(reason)::text,sqlc.narg(evidence)::jsonb,
 i.subscription_id,sqlc.narg(provider_intent_id)::uuid,
 COALESCE(i.origin,NULLIF(sqlc.arg(origin)::text,'')),
 CASE WHEN i.id IS NOT NULL THEN i.actor ELSE NULLIF(sqlc.arg(actor)::text,'') END,
 NULLIF(sqlc.arg(executor)::text,''),clock_timestamp()
FROM (SELECT 1) seed LEFT JOIN billing.provider_intents i
 ON i.merchant_id=sqlc.arg(merchant_id)::uuid AND i.id=sqlc.narg(provider_intent_id)::uuid;

-- name: RecordSubscriptionOperationHold :exec
-- Timestamp after acquiring the row lock: transaction start time could order
-- a waiting transaction before the hold it followed.
-- The caller already updated (and therefore locked) the intent in this transaction.
-- Unknown readbacks do not end a hold. Only attempted execution or a terminal
-- result separates two observations of the same hold.
INSERT INTO billing.provider_mutation_logs
 (merchant_id,rail,psp_id,custodian_id,provider_intent_id,operation_id,subscription_id,
 intent_type,attempt,phase,reason_code,origin,actor,executor,created_at)
SELECT i.merchant_id,i.rail,i.psp_id,i.custodian_id,i.id,i.id,i.subscription_id,
 i.intent_type,i.attempts,'parked',sqlc.arg(reason_code)::text,i.origin,i.actor,NULLIF(sqlc.arg(executor)::text,''),clock_timestamp()
FROM billing.provider_intents i
WHERE i.merchant_id=sqlc.arg(merchant_id)::uuid AND i.id=sqlc.arg(id)::uuid AND i.subscription_id IS NOT NULL
 AND NOT EXISTS (
  SELECT 1 FROM (
   SELECT l.phase,l.reason_code FROM billing.provider_mutation_logs l
   WHERE l.merchant_id=i.merchant_id AND l.operation_id=i.id
     AND l.phase IN ('parked','attempting','succeeded','failed')
   ORDER BY l.created_at DESC,l.id DESC LIMIT 1
  ) last WHERE last.phase='parked' AND last.reason_code=sqlc.arg(reason_code)::text
 );

-- Operator read surface: this table is the durable mutation log.

-- name: ListProviderMutationLogs :many
SELECT * FROM billing.provider_mutation_logs
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(provider_intent_id)::uuid IS NULL OR provider_intent_id = sqlc.narg(provider_intent_id)::uuid)
  AND (sqlc.narg(psp_id)::uuid IS NULL OR psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(phase)::text IS NULL OR phase = sqlc.narg(phase)::text)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(limit_rows);

-- name: CountProviderMutationLogs :one
SELECT count(*) FROM billing.provider_mutation_logs
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(provider_intent_id)::uuid IS NULL OR provider_intent_id = sqlc.narg(provider_intent_id)::uuid)
  AND (sqlc.narg(psp_id)::uuid IS NULL OR psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(phase)::text IS NULL OR phase = sqlc.narg(phase)::text);
