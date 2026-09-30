-- parent: 30 sha256:0d76ba03ab697471706c58ae3b9206244a9d5385b1ee52e4bfb350b9c9e6fec4
-- #1114: what NMI's Query API reports about an attempt beyond its reply: the
-- card's BIN and the issuer's raw answer. enriched_at marks a row filled from
-- that read, so the enrichment pass reads each transaction once.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

ALTER TABLE openrails.payment_attempts
    ADD COLUMN card_bin text,
    ADD COLUMN issuer_code text,
    ADD COLUMN issuer_text text,
    ADD COLUMN enriched_at timestamp with time zone;
ALTER TABLE openrails.payment_attempts
    ADD CONSTRAINT chk_payment_attempts_card_bin CHECK (card_bin ~ '^[0-9]{6,8}$') NOT VALID;
ALTER TABLE openrails.payment_attempts
    ADD CONSTRAINT chk_payment_attempts_issuer CHECK (length(issuer_code) <= 32 AND length(issuer_text) <= 128) NOT VALID;

COMMENT ON COLUMN openrails.payment_attempts.issuer_code IS '#1114 the issuer''s raw answer (NMI processor_response_code); response_code is the gateway''s.';
COMMENT ON COLUMN openrails.payment_attempts.enriched_at IS '#1114 when the row was filled from the PSP''s transaction read; NULL rows are read by the enrichment pass.';

CREATE INDEX idx_payment_attempts_unenriched ON openrails.payment_attempts USING btree (merchant_id, attempted_at)
    WHERE enriched_at IS NULL AND rail = 'nmi' AND transaction_id IS NOT NULL;

-- The merchants holding NMI attempts in [p_since, p_before) not yet enriched.
CREATE FUNCTION openrails.unenriched_attempt_merchant_ids(p_since timestamp with time zone, p_before timestamp with time zone, p_limit integer) RETURNS TABLE(merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT a.merchant_id
      FROM openrails.payment_attempts a
     WHERE a.enriched_at IS NULL AND a.rail = 'nmi' AND a.transaction_id IS NOT NULL
       AND a.attempted_at >= p_since AND a.attempted_at < p_before
     GROUP BY a.merchant_id
     ORDER BY MIN(a.attempted_at), a.merchant_id
     LIMIT p_limit;
END;
$$;

REVOKE ALL ON FUNCTION openrails.unenriched_attempt_merchant_ids(p_since timestamp with time zone, p_before timestamp with time zone, p_limit integer) FROM PUBLIC;
