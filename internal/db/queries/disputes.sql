-- billing.disputes, billing.fraud_reports and billing.network_activity_daily.

-- name: InsertDispute :one
-- A case the PSP reports for the first time; a concurrent first report of the
-- same case inserts nothing and reads it with GetDisputeForUpdate.
INSERT INTO billing.disputes (
    merchant_id, psp_id, rail, provider_ref, customer_id, payment_id, kind, network, reason, reason_code, provider_reason,
    amount, currency, status, disputed_at, chargeback_at, evidence_due_at, closed_at, observed_via
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(rail)::text, sqlc.arg(provider_ref)::text,
    sqlc.narg(customer_id)::uuid, sqlc.narg(payment_id)::uuid, sqlc.arg(kind)::text, sqlc.arg(network)::text, sqlc.arg(reason)::text,
    sqlc.narg(reason_code)::text, sqlc.narg(provider_reason)::text, sqlc.narg(amount)::bigint, sqlc.narg(currency)::text,
    sqlc.arg(status)::text, sqlc.arg(disputed_at)::timestamptz, sqlc.narg(chargeback_at)::timestamptz,
    sqlc.narg(evidence_due_at)::timestamptz, sqlc.narg(closed_at)::timestamptz, sqlc.arg(observed_via)::text
)
ON CONFLICT (merchant_id, psp_id, provider_ref) DO NOTHING
RETURNING *;

-- name: GetDisputeForUpdate :one
SELECT * FROM billing.disputes
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND provider_ref = sqlc.arg(provider_ref)::text
FOR UPDATE;

-- name: LinkDisputePayments :exec
-- Names the payments that took the case's money back and returned it,
-- leaving everything else as the latest report left it.
UPDATE billing.disputes SET
    reversal_payment_id = COALESCE(sqlc.narg(reversal_payment_id)::uuid, reversal_payment_id),
    recovery_payment_id = COALESCE(sqlc.narg(recovery_payment_id)::uuid, recovery_payment_id),
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: MarkDisputeResponded :exec
UPDATE billing.disputes SET responded_at = sqlc.arg(responded_at)::timestamptz, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND responded_at IS NULL;

-- name: MarkUnmatchedDisputeResponded :exec
UPDATE billing.disputes SET responded_at = sqlc.arg(responded_at)::timestamptz, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND responded_at IS NULL AND payment_id IS NULL;

-- name: PaymentDisputeDecides :one
-- Whether a dispute decides the charge: an open inquiry or chargeback, or a
-- chargeback whose reversal was not booked.
SELECT EXISTS (
    SELECT 1 FROM billing.disputes
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND payment_id = sqlc.arg(payment_id)::uuid
      AND (status = 'open' OR (kind = 'chargeback' AND reversal_payment_id IS NULL))
)::boolean;

-- name: PaymentPreventionRefunded :one
-- Whether a prevented dispute already returned the charge's money.
SELECT EXISTS (
    SELECT 1 FROM billing.disputes
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND payment_id = sqlc.arg(payment_id)::uuid
      AND kind = 'prevented' AND reversal_payment_id IS NOT NULL
)::boolean;

-- name: PSPHasOpenThresholdFinding :one
-- Whether a dispute threshold finding of the PSP is open for the month a sale
-- settled in: its sales re-evaluate it. An ended month's is settled already.
SELECT EXISTS (
    SELECT 1 FROM billing.reconciliation_findings
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND finding_type = 'life.disputes.threshold' AND psp_id = sqlc.arg(psp_id)::uuid
      AND status IN ('reconcile_required', 'requires_review') AND subject_key LIKE '%:' || sqlc.arg(month)::text
)::boolean;

-- name: GetAbsorbingPrevention :one
-- The prevented case whose RDR refund a Stripe refund of the charge for the
-- same amount, made no earlier than the case, is: one that absorbed none yet.
SELECT d.* FROM billing.disputes d
JOIN billing.payments r ON r.merchant_id = d.merchant_id AND r.id = d.reversal_payment_id
WHERE d.merchant_id = sqlc.arg(merchant_id)::uuid AND d.payment_id = sqlc.arg(payment_id)::uuid
  AND d.kind = 'prevented' AND d.absorbed_refund_ref IS NULL AND d.disputed_at <= sqlc.arg(refunded_at)::timestamptz
  AND r.refund_reason = 'dispute_prevention' AND r.amount = -sqlc.arg(amount)::bigint AND r.currency = sqlc.arg(currency)::text
ORDER BY d.disputed_at
LIMIT 1
FOR UPDATE OF d;

-- name: AbsorbDisputeRefund :execrows
-- The case takes a provider refund as its RDR refund, once.
UPDATE billing.disputes SET absorbed_refund_ref = sqlc.arg(refund_ref)::text,
    reversal_payment_id = COALESCE(reversal_payment_id, sqlc.narg(reversal_payment_id)::uuid), updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND absorbed_refund_ref IS NULL;

-- name: DisputeRefundAbsorbed :one
-- Whether a case already took this provider refund as its own.
SELECT EXISTS (
    SELECT 1 FROM billing.disputes
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND payment_id = sqlc.arg(payment_id)::uuid AND absorbed_refund_ref = sqlc.arg(refund_ref)::text
)::boolean;

-- name: ListWaitingDisputeResponses :many
-- Every response not yet finished, across merchants: a wake queues them.
SELECT merchant_id, 'dispute'::text AS subject, id FROM billing.disputes
WHERE kind = 'chargeback' AND responded_at IS NULL
UNION ALL
SELECT merchant_id, 'fraud_report'::text AS subject, id FROM billing.fraud_reports
WHERE responded_at IS NULL AND action IN ('pending', 'refunded', 'nothing_to_refund');

-- name: MerchantHasWaitingDisputeResponses :one
SELECT (EXISTS (
    SELECT 1 FROM billing.disputes WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND kind = 'chargeback' AND responded_at IS NULL
) OR EXISTS (
    SELECT 1 FROM billing.fraud_reports WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND responded_at IS NULL
      AND action IN ('pending', 'refunded', 'nothing_to_refund')
))::boolean;

-- name: ListOpenThresholdFindings :many
-- The PSP's open dispute threshold findings: a past month's is finalized.
SELECT subject_key FROM billing.reconciliation_findings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND finding_type = 'life.disputes.threshold' AND psp_id = sqlc.arg(psp_id)::uuid
  AND status IN ('reconcile_required', 'requires_review')
ORDER BY subject_key;

-- name: UpdateDispute :one
-- What a later report of a case says; the recorder decides each transition.
UPDATE billing.disputes SET
    kind = sqlc.arg(kind)::text,
    network = sqlc.arg(network)::text,
    reason = sqlc.arg(reason)::text,
    reason_code = sqlc.narg(reason_code)::text,
    provider_reason = sqlc.narg(provider_reason)::text,
    amount = sqlc.narg(amount)::bigint,
    currency = sqlc.narg(currency)::text,
    status = sqlc.arg(status)::text,
    chargeback_at = sqlc.narg(chargeback_at)::timestamptz,
    evidence_due_at = sqlc.narg(evidence_due_at)::timestamptz,
    closed_at = sqlc.narg(closed_at)::timestamptz,
    reversal_payment_id = sqlc.narg(reversal_payment_id)::uuid,
    recovery_payment_id = sqlc.narg(recovery_payment_id)::uuid,
    observed_via = sqlc.arg(observed_via)::text,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
RETURNING *;

-- name: GetDispute :one
SELECT * FROM billing.disputes
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: ListDisputes :many
-- One page of the merchant's disputes, newest first, after a (disputed_at, id)
-- cursor; every filter is optional and a text filter matches any of its values.
SELECT d.*
FROM billing.disputes d
WHERE d.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(kinds)::text[] IS NULL OR d.kind = ANY(sqlc.narg(kinds)::text[]))
  AND (sqlc.narg(statuses)::text[] IS NULL OR d.status = ANY(sqlc.narg(statuses)::text[]))
  AND (sqlc.narg(reasons)::text[] IS NULL OR d.reason = ANY(sqlc.narg(reasons)::text[]))
  AND (sqlc.narg(networks)::text[] IS NULL OR d.network = ANY(sqlc.narg(networks)::text[]))
  AND (sqlc.narg(psp_id)::uuid IS NULL OR d.psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(customer_id)::uuid IS NULL OR d.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(payment_id)::uuid IS NULL OR d.payment_id = sqlc.narg(payment_id)::uuid)
  AND (sqlc.narg(since)::timestamptz IS NULL OR d.disputed_at >= sqlc.narg(since)::timestamptz)
  AND (sqlc.narg(until)::timestamptz IS NULL OR d.disputed_at < sqlc.narg(until)::timestamptz)
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (d.disputed_at, d.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY d.disputed_at DESC, d.id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: InsertFraudReport :one
INSERT INTO billing.fraud_reports (
    merchant_id, psp_id, rail, provider_ref, customer_id, payment_id, network, fraud_type, actionable, amount, currency, reported_at, action
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(rail)::text, sqlc.arg(provider_ref)::text,
    sqlc.arg(customer_id)::uuid, sqlc.arg(payment_id)::uuid, sqlc.arg(network)::text, sqlc.arg(fraud_type)::text,
    sqlc.arg(actionable)::boolean, sqlc.arg(amount)::bigint, sqlc.arg(currency)::text, sqlc.arg(reported_at)::timestamptz, sqlc.arg(action)::text
)
ON CONFLICT (merchant_id, psp_id, provider_ref) DO NOTHING
RETURNING *;

-- name: GetFraudReportForUpdate :one
SELECT * FROM billing.fraud_reports
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND provider_ref = sqlc.arg(provider_ref)::text
FOR UPDATE;

-- name: NoteFraudReportActionable :one
-- A report stays actionable only while every report of it says so; one no
-- longer actionable before it was answered is recorded only.
UPDATE billing.fraud_reports SET actionable = actionable AND sqlc.arg(actionable)::boolean,
    action = CASE WHEN action = 'pending' AND NOT sqlc.arg(actionable)::boolean THEN 'recorded' ELSE action END,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
RETURNING *;

-- name: DecideFraudReport :execrows
-- What OpenRails does about a pending report, decided once.
UPDATE billing.fraud_reports SET action = sqlc.arg(action)::text, refund_payment_id = sqlc.narg(refund_payment_id)::uuid,
    responded_at = CASE WHEN sqlc.arg(action)::text = 'disputed' THEN sqlc.arg(now)::timestamptz END, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND action = 'pending';

-- name: MarkFraudReportResponded :exec
UPDATE billing.fraud_reports SET responded_at = sqlc.arg(responded_at)::timestamptz, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND responded_at IS NULL;

-- name: MarkFraudReportEnded :exec
UPDATE billing.fraud_reports SET ended_at = sqlc.arg(ended_at)::timestamptz, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND ended_at IS NULL;

-- name: GetFraudReport :one
SELECT * FROM billing.fraud_reports WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: ListNetworkActivity :many
-- The rollup's days in [from_at, to_at) for every card PSP, or one.
SELECT psp_id, rail, network, currency, day_at, sales, sales_amount, refunds, refund_amount, chargebacks, chargeback_amount,
       fraud_chargebacks, fraud_chargeback_amount, fraud_reports, fraud_report_amount
FROM billing.network_activity_daily
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(psp_id)::uuid IS NULL OR psp_id = sqlc.narg(psp_id)::uuid)
  AND day_at >= sqlc.arg(from_at)::timestamptz AND day_at < sqlc.arg(to_at)::timestamptz
ORDER BY psp_id, day_at, network, currency;

-- name: ListPSPsWithDisputes :many
-- The PSPs whose rail has reported a dispute: a PSP whose processor never
-- reports chargebacks has none.
SELECT p.id FROM billing.psps p
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid
  AND EXISTS (SELECT 1 FROM billing.disputes d WHERE d.merchant_id = p.merchant_id AND d.psp_id = p.id);

-- name: MarkPaymentMethodDisputed :execrows
-- A card charged back is never charged again.
UPDATE billing.payment_methods SET status = 'disputed', updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND status = 'active';

-- name: CountCustomerChargebacks :one
SELECT count(*)::bigint FROM billing.disputes
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND kind = 'chargeback';

-- name: ListUnnotifiedFindings :many
-- Open findings of one type no alert has claimed yet.
SELECT * FROM billing.reconciliation_findings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND finding_type = sqlc.arg(finding_type)::text
  AND status = 'requires_review' AND notified_at IS NULL
ORDER BY created_at
LIMIT 100;

-- name: ListLiveSubscriptionsOnCard :many
-- The customer's live subscriptions that charge one card.
SELECT s.id, s.collection_policy FROM billing.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.customer_id = sqlc.arg(customer_id)::uuid AND s.deleted_at IS NULL
  AND s.status IN ('active', 'past_due', 'awaiting_method', 'unverified')
  AND billing.subscription_payment_method_id(s.merchant_id, s.customer_id, s.payment_method_id, s.price_id, s.rail, s.collection_policy) = sqlc.arg(payment_method_id)::uuid
ORDER BY s.id;

-- name: ListCustomerCardsByRailCustomerRef :many
-- The customer's saved cards a provider vault holds: an NMI schedule's sale
-- names its vault, and one card in it is the card charged.
SELECT id FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND rail_customer_ref = sqlc.arg(rail_customer_ref)::text
LIMIT 2;

-- name: AttachDisputePayment :one
-- An operator identifies the charge an unmatched case disputes, once.
UPDATE billing.disputes SET
    payment_id = sqlc.arg(payment_id)::uuid,
    customer_id = sqlc.arg(customer_id)::uuid,
    amount = sqlc.arg(amount)::bigint,
    currency = sqlc.arg(currency)::text,
    network = sqlc.arg(network)::text,
    reason = sqlc.arg(reason)::text,
    responded_at = NULL,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND payment_id IS NULL
RETURNING *;
