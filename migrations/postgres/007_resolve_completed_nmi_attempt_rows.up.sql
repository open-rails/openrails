-- NMI subscription-checkout attempt rows (synthetic transaction_id
-- 'nmi_sub_attempt:…') were completed only in metadata
-- (nmi_attempt_status = 'completed'); the status column stayed 'pending'
-- forever, reading as a stuck payment to anything inspecting payment status.
-- CompleteProviderAttemptInPlace now also sets status = 'completed'; this
-- backfills the rows resolved before that change.
UPDATE openrails.payments
SET status = 'completed'
WHERE status = 'pending'
  AND transaction_id LIKE 'nmi\_sub\_attempt:%'
  AND metadata ->> 'nmi_attempt_status' = 'completed';
