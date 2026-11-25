-- One PSP's live linked book: statuses that still bill or grant access, with a
-- rail handle. The cancellation cap and roster ratio breaker are measured
-- against it.
-- name: CountLiveLinkedSubscriptionsForPSP :one
SELECT count(*) FROM billing.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND status IN ('active', 'past_due', 'awaiting_method', 'unverified')
  AND rail_subscription_id IS NOT NULL
  AND deleted_at IS NULL;
