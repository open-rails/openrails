-- name: RecordCardAttemptFailure :exec
INSERT INTO openrails.card_attempt_failures (merchant_id, subject, bucket_at, failures)
SELECT sqlc.arg(merchant_id)::uuid, s.subject, sqlc.arg(bucket_at)::timestamptz, 1
FROM unnest(sqlc.arg(subjects)::text[]) AS s(subject)
ON CONFLICT (merchant_id, subject, bucket_at)
DO UPDATE SET failures = openrails.card_attempt_failures.failures + 1;

-- name: CardAttemptFailureCounts :many
SELECT subject,
       COALESCE(sum(failures) FILTER (WHERE bucket_at >= sqlc.arg(burst_since)::timestamptz), 0)::bigint AS burst,
       COALESCE(sum(failures), 0)::bigint AS daily
FROM openrails.card_attempt_failures
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND subject = ANY(sqlc.arg(subjects)::text[])
  AND bucket_at >= sqlc.arg(daily_since)::timestamptz
GROUP BY subject;

-- name: PruneCardAttemptFailures :execrows
DELETE FROM openrails.card_attempt_failures
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND bucket_at < sqlc.arg(before)::timestamptz;

-- name: CardAttackBreadth :one
-- A merchant's card failures since a time, and how many customers and client
-- addresses they came from.
SELECT COALESCE(sum(failures) FILTER (WHERE subject = sqlc.arg(merchant_subject)::text), 0)::bigint AS failures,
       count(DISTINCT subject) FILTER (WHERE starts_with(subject, sqlc.arg(customer_prefix)::text))::bigint AS customers,
       count(DISTINCT subject) FILTER (WHERE starts_with(subject, sqlc.arg(address_prefix)::text))::bigint AS addresses
FROM openrails.card_attempt_failures
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND bucket_at >= sqlc.arg(since)::timestamptz;
