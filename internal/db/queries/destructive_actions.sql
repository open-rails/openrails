-- Command admission serializes only rare destructive commands, never billing.
-- name: LockDestructiveAdmissions :exec
SELECT pg_advisory_xact_lock(hashtextextended('openrails:destructive-admissions:' || 'billing.destructive_actions'::regclass::oid::text,0));

-- name: GetDestructiveActionByKey :one
SELECT * FROM billing.destructive_actions
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND action_key=sqlc.arg(action_key)::text
ORDER BY (state<>'no_effect') DESC,admission_sequence DESC LIMIT 1;

-- name: GetDestructiveAction :one
SELECT * FROM billing.destructive_actions
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid;

-- name: LockDestructiveAction :one
SELECT * FROM billing.destructive_actions
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid FOR UPDATE;

-- name: CreateDestructiveAction :one
INSERT INTO billing.destructive_actions(merchant_id,id,kind,target,action_key,request_digest,origin,actor_issuer,actor_id,actor_key,credential,units,admitted_at,state)
VALUES(sqlc.arg(merchant_id),sqlc.arg(id),sqlc.arg(kind),sqlc.arg(target),sqlc.arg(action_key),sqlc.arg(request_digest),sqlc.arg(origin),sqlc.arg(actor_issuer),sqlc.arg(actor_id),sqlc.arg(actor_key),sqlc.narg(credential),sqlc.arg(units),sqlc.arg(admitted_at),'reserved') RETURNING *;

-- name: StartDestructiveAction :one
UPDATE billing.destructive_actions SET state='started',started_at=sqlc.arg(now)::timestamptz
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid AND state='reserved' RETURNING *;

-- name: FinishDestructiveAction :execrows
UPDATE billing.destructive_actions
SET state=sqlc.arg(state)::text,
 started_at=coalesce(started_at,sqlc.narg(started_at)::timestamptz),
 finished_at=sqlc.narg(finished_at)::timestamptz,result=sqlc.narg(result)::text
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid AND state IN ('reserved','started','uncertain');

-- name: DestructiveActionProviderState :one
SELECT EXISTS(SELECT 1 FROM billing.provider_intents WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND destructive_action_id=sqlc.arg(id)::uuid AND status NOT IN ('succeeded','failed_terminal','superseded','expired'))::boolean AS pending,
 EXISTS(SELECT 1 FROM billing.provider_intents WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND destructive_action_id=sqlc.arg(id)::uuid AND (attempts>0 OR status='succeeded'))::boolean AS attempted;

-- Directory scope is deliberate: an actor and a deployment cross merchants.
-- name: FindOpenDestructiveHold :one
SELECT * FROM billing.destructive_holds WHERE scope_key=ANY(sqlc.arg(scope_keys)::text[]) AND cleared_at IS NULL
ORDER BY opened_at,id LIMIT 1;

-- name: DestructiveScopeClearEpoch :one
SELECT coalesce(max(clear_through_sequence),0)::bigint AS clear_through_sequence FROM billing.destructive_holds WHERE scope_key=sqlc.arg(scope_key)::text AND cleared_at IS NOT NULL;

-- name: CreateDestructiveHold :one
INSERT INTO billing.destructive_holds(merchant_id,id,scope,scope_key,actor_issuer,actor_id,credential,action_kind,target,window_name,observed,requested,ceiling,opened_at)
VALUES(sqlc.arg(merchant_id),sqlc.arg(id),sqlc.arg(scope),sqlc.arg(scope_key),sqlc.arg(actor_issuer),sqlc.arg(actor_id),sqlc.narg(credential),sqlc.arg(action_kind),sqlc.arg(target),sqlc.arg(window_name),sqlc.arg(observed),sqlc.arg(requested),sqlc.arg(ceiling),sqlc.arg(opened_at)) RETURNING *;

-- name: GetDestructiveHold :one
SELECT * FROM billing.destructive_holds WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid;

-- name: ListDestructiveHolds :many
SELECT * FROM billing.destructive_holds WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND (sqlc.narg(after_id)::uuid IS NULL OR id>sqlc.narg(after_id)::uuid)
ORDER BY id LIMIT sqlc.arg(row_limit)::int;

-- Operator discovery returns only incident addresses; tenant details use a scoped read.
-- name: ListOpenDestructiveHoldAddresses :many
SELECT merchant_id,id FROM billing.destructive_holds WHERE cleared_at IS NULL
AND (sqlc.narg(after_id)::uuid IS NULL OR id>sqlc.narg(after_id)::uuid) ORDER BY id LIMIT sqlc.arg(row_limit)::int;

-- name: ClearDestructiveHold :one
UPDATE billing.destructive_holds SET cleared_at=sqlc.arg(now)::timestamptz,clear_through_sequence=(SELECT coalesce(max(admission_sequence),0) FROM billing.destructive_actions),cleared_by_issuer=sqlc.arg(issuer)::text,cleared_by_id=sqlc.arg(actor_id)::text,clear_reason=sqlc.arg(reason)::text
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid AND cleared_at IS NULL RETURNING *;

-- name: EnqueueDestructiveHoldEvent :exec
INSERT INTO billing.host_outbox(merchant_id,event_type,subject_type,subject_id,occurred_at,data,dedupe_key)
VALUES(sqlc.arg(merchant_id)::uuid,sqlc.arg(event_type)::text,'destructive_hold',sqlc.arg(hold_id)::uuid,sqlc.arg(occurred_at)::timestamptz,sqlc.arg(data)::jsonb,sqlc.arg(dedupe_key)::text)
ON CONFLICT(merchant_id,dedupe_key) DO NOTHING;

-- name: ResolveDestructiveHoldFinding :exec
UPDATE billing.reconciliation_findings SET status='auto_fixed',resolution='admin_fixed',resolved_at=sqlc.arg(now)::timestamptz,resolved_by=sqlc.arg(actor)::text,operator_notes=sqlc.arg(reason)::text,updated_at=sqlc.arg(now)::timestamptz
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND finding_type='life.destructive.hold' AND subject_key=sqlc.arg(hold_id)::text AND status IN ('requires_review','reconcile_required','ignored');

-- name: HasDestructiveArchiveBarrier :one
SELECT EXISTS(SELECT 1 FROM billing.destructive_actions WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND (state IN ('reserved','started','uncertain') OR (state='succeeded' AND admitted_at>sqlc.arg(since)::timestamptz)))
 OR EXISTS(SELECT 1 FROM billing.destructive_holds WHERE cleared_at IS NULL AND (scope='deployment' OR merchant_id=sqlc.arg(merchant_id)::uuid OR scope_key IN (SELECT 'actor:'||actor_key FROM billing.destructive_actions WHERE merchant_id=sqlc.arg(merchant_id)::uuid))) AS held;

-- name: CountDestructiveUnitsForActor :one
WITH counted AS (
 SELECT units,admitted_at,true AS pending FROM billing.destructive_actions WHERE actor_key=sqlc.arg(actor_key)::text AND state IN ('reserved','started','uncertain')
 UNION ALL
 SELECT units,admitted_at,false AS pending FROM billing.destructive_actions WHERE actor_key=sqlc.arg(actor_key)::text AND state='succeeded' AND admission_sequence>sqlc.arg(clear_after)::bigint AND admitted_at>=sqlc.arg(day_since)::timestamptz
)
SELECT coalesce(sum(units) FILTER(WHERE pending OR admitted_at>=sqlc.arg(hour_since)::timestamptz),0)::bigint AS hourly,
 coalesce(sum(units),0)::bigint AS daily FROM counted;

-- name: CountDestructiveUnitsForMerchant :one
WITH counted AS (
 SELECT units,admitted_at,true AS pending FROM billing.destructive_actions WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND state IN ('reserved','started','uncertain')
 UNION ALL
 SELECT units,admitted_at,false AS pending FROM billing.destructive_actions WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND state='succeeded' AND admission_sequence>sqlc.arg(clear_after)::bigint AND admitted_at>=sqlc.arg(day_since)::timestamptz
)
SELECT coalesce(sum(units) FILTER(WHERE pending OR admitted_at>=sqlc.arg(hour_since)::timestamptz),0)::bigint AS hourly,
 coalesce(sum(units),0)::bigint AS daily FROM counted;

-- name: CountDestructiveUnitsForDeployment :one
WITH counted AS (
 SELECT units,admitted_at,true AS pending FROM billing.destructive_actions WHERE true AND state IN ('reserved','started','uncertain')
 UNION ALL
 SELECT units,admitted_at,false AS pending FROM billing.destructive_actions WHERE true AND state='succeeded' AND admission_sequence>sqlc.arg(clear_after)::bigint AND admitted_at>=sqlc.arg(day_since)::timestamptz
)
SELECT coalesce(sum(units) FILTER(WHERE pending OR admitted_at>=sqlc.arg(hour_since)::timestamptz),0)::bigint AS hourly,
 coalesce(sum(units),0)::bigint AS daily FROM counted;

-- Customer membership scale is billing activity, not a cache or routine sweep.
-- name: DestructiveActiveCustomerScale :one
WITH active AS (
 SELECT s.merchant_id,s.customer_id FROM billing.subscriptions s
 JOIN billing.prices p ON p.merchant_id=s.merchant_id AND p.id=s.price_id
 WHERE s.status IN ('active','past_due') AND s.deleted_at IS NULL AND p.amount>0
 UNION
 SELECT p.merchant_id,p.customer_id FROM billing.payments p
 WHERE p.purchased_at>=sqlc.arg(since)::timestamptz AND p.status='succeeded' AND p.amount>0 AND p.refunded_payment_id IS NULL AND p.deleted_at IS NULL
), live AS (
 SELECT a.merchant_id FROM active a JOIN billing.customers c ON c.merchant_id=a.merchant_id AND c.id=a.customer_id
 JOIN billing.merchants m ON m.id=a.merchant_id
 WHERE c.deleted_at IS NULL AND m.deleted_at IS NULL AND m.status='active'
)
SELECT count(*) FILTER(WHERE merchant_id=sqlc.arg(merchant_id)::uuid)::bigint AS merchant_active,count(*)::bigint AS deployment_active FROM live;
