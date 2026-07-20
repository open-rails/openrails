ALTER TABLE openrails.invoices
    ADD COLUMN collection_failure_count integer NOT NULL DEFAULT 0,
    ADD COLUMN collection_failed_at timestamp with time zone,
    ADD COLUMN next_collection_attempt_at timestamp with time zone,
    ADD COLUMN last_collection_failure_code text,
    ADD COLUMN last_collection_failure_message text,
    ADD CONSTRAINT invoices_collection_failure_count_nonneg CHECK (collection_failure_count >= 0);

ALTER TABLE openrails.invoice_payments
    ADD COLUMN failure_reason text;

UPDATE openrails.invoices invoice
SET collection_failure_count = failures.count,
    collection_failed_at = failures.first_failed_at
FROM (
    SELECT invoice_id, count(*)::integer AS count, min(attempted_at) AS first_failed_at
    FROM openrails.invoice_payments
    WHERE status = 'failed'
    GROUP BY invoice_id
) failures
WHERE invoice.id = failures.invoice_id;

UPDATE openrails.invoices
SET status = 'uncollectible',
    uncollectible_at = COALESCE(uncollectible_at, now()),
    updated_at = now()
WHERE status IN ('open', 'past_due')
  AND amount_due > 0
  AND collection_failure_count >= 5;

UPDATE openrails.invoices
SET next_collection_attempt_at = now()
WHERE status IN ('open', 'past_due')
  AND amount_due > 0
  AND collection_failure_count BETWEEN 1 AND 4;

CREATE INDEX ix_invoices_collection_due
    ON openrails.invoices (merchant_id, next_collection_attempt_at, due_at)
    WHERE status IN ('open', 'past_due')
      AND amount_due > 0
      AND collection_method = 'charge_automatically';
