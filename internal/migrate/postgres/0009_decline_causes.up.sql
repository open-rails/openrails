-- Decline causes: outcome and decline_cause replace category on payment
-- attempts and NMI history months, and attempts keep the PSP's and the
-- network's advice. Existing rows are classified again from their stored
-- codes; an attempt's action is what dunning decided then and stays.

-- Each reason's cause.
CREATE TEMPORARY TABLE decline_causes (reason text PRIMARY KEY, cause text NOT NULL);
INSERT INTO decline_causes VALUES
    ('insufficient_funds', 'customer'), ('over_limit', 'customer'), ('incorrect_number', 'customer'),
    ('no_such_issuer', 'customer'), ('invalid_account', 'customer'), ('expired_card', 'customer'),
    ('invalid_expiry', 'customer'), ('incorrect_cvc', 'customer'), ('incorrect_zip', 'customer'),
    ('incorrect_address', 'customer'), ('invalid_pin', 'customer'), ('update_cardholder_data', 'customer'),
    ('authentication_required', 'customer'), ('lost_card', 'customer'), ('stop_recurring', 'customer'),
    ('generic_decline', 'issuer'), ('do_not_honor', 'issuer'), ('call_issuer', 'issuer'),
    ('retry_later', 'issuer'), ('try_again_later', 'issuer'), ('restricted_card', 'issuer'),
    ('transaction_not_allowed', 'issuer'), ('card_not_supported', 'issuer'), ('currency_not_supported', 'issuer'),
    ('suspected_fraud', 'fraud'), ('security_violation', 'fraud'), ('pickup_card', 'fraud'),
    ('stolen_card', 'fraud'), ('fraudulent', 'fraud'),
    ('gateway_rejected', 'merchant'), ('blocked_by_psp', 'merchant'), ('brand_not_accepted', 'merchant'),
    ('merchant_config', 'merchant'), ('invalid_request', 'merchant'), ('duplicate_transaction', 'merchant'),
    ('processing_error', 'processing'), ('communication_error', 'processing'), ('issuer_unavailable', 'processing'),
    ('unknown', 'unknown');

ALTER TABLE billing.payment_attempts
    ADD COLUMN outcome text,
    ADD COLUMN decline_cause text,
    ADD COLUMN advice_code text,
    ADD COLUMN network_advice_code text;

-- Codes mapped since: NMI 461 is the merchant account's brand, and Stripe's
-- newer codes were unknown.
UPDATE billing.payment_attempts SET reason = 'brand_not_accepted'
WHERE rail = 'nmi' AND reason = 'card_not_supported';
UPDATE billing.payment_attempts a SET reason = m.reason
FROM (VALUES
    ('do_not_try_again', 'generic_decline'), ('new_account_information_available', 'update_cardholder_data'),
    ('service_not_allowed', 'transaction_not_allowed'), ('mobile_device_authentication_required', 'authentication_required'),
    ('invalid_amount', 'invalid_request'), ('testmode_decline', 'invalid_request'),
    ('highest_risk_level', 'blocked_by_psp'), ('elevated_risk_level', 'blocked_by_psp'), ('rule', 'blocked_by_psp'),
    ('low_probability_of_authorization', 'blocked_by_psp')
) AS m (code, reason)
WHERE a.rail = 'stripe' AND a.reason = 'unknown' AND a.response_code = m.code;

-- The issuer's code names the cause of a generic decline, as the classifier
-- reads it now: before a postal-code mismatch is taken for the cause.
CREATE TEMPORARY TABLE issuer_reasons (code text PRIMARY KEY, reason text NOT NULL);
INSERT INTO issuer_reasons VALUES
    ('01', 'call_issuer'), ('02', 'call_issuer'), ('03', 'merchant_config'), ('58', 'merchant_config'),
    ('04', 'pickup_card'), ('07', 'pickup_card'), ('05', 'do_not_honor'),
    ('12', 'transaction_not_allowed'), ('57', 'transaction_not_allowed'), ('93', 'transaction_not_allowed'),
    ('13', 'invalid_request'), ('14', 'incorrect_number'), ('15', 'no_such_issuer'), ('19', 'try_again_later'),
    ('41', 'lost_card'), ('43', 'stolen_card'),
    ('46', 'invalid_account'), ('78', 'invalid_account'), ('39', 'invalid_account'), ('52', 'invalid_account'), ('53', 'invalid_account'),
    ('51', 'insufficient_funds'), ('54', 'expired_card'), ('55', 'invalid_pin'), ('75', 'invalid_pin'),
    ('59', 'suspected_fraud'), ('61', 'over_limit'), ('65', 'over_limit'), ('62', 'restricted_card'),
    ('63', 'security_violation'), ('82', 'incorrect_cvc'), ('N7', 'incorrect_cvc'), ('1A', 'authentication_required'),
    ('91', 'issuer_unavailable'), ('94', 'duplicate_transaction'), ('96', 'processing_error'),
    ('R0', 'stop_recurring'), ('R1', 'stop_recurring'), ('R3', 'stop_recurring');
UPDATE billing.payment_attempts a SET reason = m.reason
FROM issuer_reasons m
WHERE a.reason IN ('generic_decline', 'do_not_honor', 'unknown') AND upper(a.issuer_code) = m.code AND a.reason <> m.reason;
UPDATE billing.payment_attempts a SET reason = m.reason
FROM issuer_reasons m
WHERE a.reason = 'incorrect_zip' AND upper(a.issuer_code) = m.code
  AND m.reason NOT IN ('generic_decline', 'do_not_honor', 'incorrect_number')
  AND ((a.rail = 'nmi' AND a.response_code IN ('200', '201', '260'))
    OR (a.rail = 'stripe' AND a.response_code IN ('generic_decline', 'card_declined', 'do_not_honor', 'no_action_taken', 'do_not_try_again')));

UPDATE billing.payment_attempts SET outcome = 'approved' WHERE category = 'approved';
UPDATE billing.payment_attempts a SET decline_cause = COALESCE((SELECT c.cause FROM decline_causes c WHERE c.reason = a.reason), 'unknown')
WHERE a.category <> 'approved';
UPDATE billing.payment_attempts SET outcome = CASE decline_cause WHEN 'merchant' THEN 'refused' WHEN 'processing' THEN 'error' ELSE 'declined' END
WHERE outcome IS NULL;

ALTER TABLE billing.payment_attempts DROP CONSTRAINT payment_attempts_outcome_check;
ALTER TABLE billing.payment_attempts DROP COLUMN category;
ALTER TABLE billing.payment_attempts
    ALTER COLUMN outcome SET NOT NULL,
    ADD CONSTRAINT payment_attempts_outcome_check CHECK (outcome IN ('approved', 'declined', 'refused', 'error')),
    ADD CONSTRAINT payment_attempts_decline_cause_check CHECK (decline_cause IN ('customer', 'issuer', 'fraud', 'merchant', 'processing', 'unknown')),
    ADD CONSTRAINT payment_attempts_refusal_check CHECK (
        (outcome = 'approved') = (decline_cause IS NULL) AND (decline_cause IS NULL) = (reason IS NULL) AND (reason IS NULL) = (action IS NULL)
        AND (decline_cause = 'merchant') = (outcome = 'refused') AND (decline_cause = 'processing') = (outcome = 'error')
    ),
    ADD CONSTRAINT payment_attempts_advice_check CHECK (
        advice_code <> '' AND length(advice_code) <= 32 AND network_advice_code <> '' AND length(network_advice_code) <= 8
    );
COMMENT ON COLUMN billing.payment_attempts.outcome IS 'approved; declined (the issuer said no); refused (the merchant or its gateway refused it before the issuer); error (it did not complete).';
COMMENT ON COLUMN billing.payment_attempts.decline_cause IS 'Who or what refused it: customer, issuer (discretion), fraud, merchant (or its gateway), processing or unknown. Follows the reason; null on approvals.';
COMMENT ON COLUMN billing.payment_attempts.advice_code IS 'The PSP''s advice on the refusal, verbatim (Stripe advice_code).';
COMMENT ON COLUMN billing.payment_attempts.network_advice_code IS 'The card network''s advice, verbatim (Mastercard''s Merchant Advice Code).';

ALTER TABLE billing.nmi_history_months
    ADD COLUMN outcome text,
    ADD COLUMN decline_cause text;
UPDATE billing.nmi_history_months SET reason = 'brand_not_accepted' WHERE reason = 'card_not_supported';
UPDATE billing.nmi_history_months SET outcome = 'approved' WHERE category = 'approved';
UPDATE billing.nmi_history_months h SET decline_cause = COALESCE((SELECT c.cause FROM decline_causes c WHERE c.reason = h.reason), 'unknown')
WHERE h.category <> 'approved';
UPDATE billing.nmi_history_months SET outcome = CASE decline_cause WHEN 'merchant' THEN 'refused' WHEN 'processing' THEN 'error' ELSE 'declined' END
WHERE outcome IS NULL;
ALTER TABLE billing.nmi_history_months DROP CONSTRAINT nmi_history_months_pkey;
ALTER TABLE billing.nmi_history_months DROP CONSTRAINT nmi_history_months_reason_check;
ALTER TABLE billing.nmi_history_months DROP COLUMN category;
-- Months that told one reason apart by category are one count now.
WITH merged AS (
    DELETE FROM billing.nmi_history_months RETURNING merchant_id, psp_id, month_at, kind, reason, authorizations, outcome, decline_cause
)
INSERT INTO billing.nmi_history_months (merchant_id, psp_id, month_at, kind, reason, authorizations, outcome, decline_cause)
SELECT merchant_id, psp_id, month_at, kind, reason, sum(authorizations)::bigint, outcome, decline_cause
FROM merged GROUP BY merchant_id, psp_id, month_at, kind, reason, outcome, decline_cause;
ALTER TABLE billing.nmi_history_months
    ALTER COLUMN outcome SET NOT NULL,
    ADD PRIMARY KEY (merchant_id, psp_id, month_at, kind, reason),
    ADD CONSTRAINT nmi_history_months_outcome_check CHECK (outcome IN ('approved', 'declined', 'refused', 'error')),
    ADD CONSTRAINT nmi_history_months_decline_cause_check CHECK (decline_cause IN ('customer', 'issuer', 'fraud', 'merchant', 'processing', 'unknown')),
    ADD CONSTRAINT nmi_history_months_reason_check CHECK (
        (outcome = 'approved') = (reason = '') AND (outcome = 'approved') = (decline_cause IS NULL)
        AND (decline_cause = 'merchant') = (outcome = 'refused') AND (decline_cause = 'processing') = (outcome = 'error')
    );
COMMENT ON TABLE billing.nmi_history_months IS 'Authorizations NMI answered per PSP, month_at (the month''s first instant, UTC), kind (verification, one_off_sale, scheduled_rebill) and answer: a refusal''s reason from the one classifier, with its outcome and decline_cause, or an approval (reason empty). A read replaces every month it covers. Retention: rows are deleted 25 months (761 days) after their month.';

DROP TABLE decline_causes;
DROP TABLE issuer_reasons;

-- A case's retry the network's advice set, or deferred past the next renewal.
ALTER TABLE billing.subscriptions ADD COLUMN retry_advised_at timestamptz;
COMMENT ON COLUMN billing.subscriptions.retry_advised_at IS 'The earliest retry the network''s advice allowed, when that advice set the scheduled retry: at or past the next renewal it deferred every retry, and the case ends at grace_ends_at. NULL when the dunning policy set it.';

-- A saved filter on what used to be declined keeps its meaning: an
-- authorization's outcome declined then covered every refusal and error, a
-- rebill's first_outcome declined every refusal but a processing error.
WITH widened AS (
    SELECT c.merchant_id,
        jsonb_agg(CASE
            WHEN w.widget #> '{query,filters,outcome}' ? 'declined'
                THEN jsonb_set(w.widget, '{query,filters,outcome}',
                    (SELECT jsonb_agg(DISTINCT v ORDER BY v) FROM jsonb_array_elements_text((w.widget #> '{query,filters,outcome}') || '["refused", "error"]'::jsonb) AS v))
            WHEN w.widget #> '{query,filters,first_outcome}' ? 'declined'
                THEN jsonb_set(w.widget, '{query,filters,first_outcome}',
                    (SELECT jsonb_agg(DISTINCT v ORDER BY v) FROM jsonb_array_elements_text((w.widget #> '{query,filters,first_outcome}') || '["refused"]'::jsonb) AS v))
            ELSE w.widget END ORDER BY w.position) AS layout,
        bool_or(COALESCE(w.widget #> '{query,filters,outcome}' ? 'declined', false)
            OR COALESCE(w.widget #> '{query,filters,first_outcome}' ? 'declined', false)) AS changed
    FROM billing.dashboard_configs c
    CROSS JOIN LATERAL jsonb_array_elements(c.layout) WITH ORDINALITY AS w(widget, position)
    WHERE jsonb_typeof(c.layout) = 'array'
    GROUP BY c.merchant_id
)
UPDATE billing.dashboard_configs c
SET layout = k.layout, updated_at = now()
FROM widened k
WHERE c.merchant_id = k.merchant_id AND k.changed;

-- Saved dashboards drop every widget naming a dimension decline_cause
-- renamed; a layout left without widgets is deleted, so its merchant gets the
-- health overview.
WITH widgets AS (
    SELECT c.merchant_id, w.widget, w.position,
        COALESCE(w.widget #> '{query,by}' ?| ARRAY['category', 'first_failure_category'], false)
            OR COALESCE(w.widget #> '{query,filters}' ?| ARRAY['category', 'first_failure_category'], false) AS stale
    FROM billing.dashboard_configs c
    CROSS JOIN LATERAL jsonb_array_elements(c.layout) WITH ORDINALITY AS w(widget, position)
    WHERE jsonb_typeof(c.layout) = 'array'
), cleaned AS (
    SELECT merchant_id, COALESCE(jsonb_agg(widget ORDER BY position) FILTER (WHERE NOT stale), '[]'::jsonb) AS layout
    FROM widgets
    GROUP BY merchant_id
    HAVING bool_or(stale)
), emptied AS (
    DELETE FROM billing.dashboard_configs c
    USING cleaned k
    WHERE c.merchant_id = k.merchant_id AND k.layout = '[]'::jsonb
)
UPDATE billing.dashboard_configs c
SET layout = k.layout, updated_at = now()
FROM cleaned k
WHERE c.merchant_id = k.merchant_id AND k.layout <> '[]'::jsonb;
