-- Repair: none-needed A new index.
-- The enrichment pass reads unenriched NMI attempts across merchants by time.
-- Every attempt leaves the index: read, given up, or marked read once past the
-- pass's window.
CREATE INDEX payment_attempts_attempted_at_merchant_id_unenriched_idx ON billing.payment_attempts USING btree (attempted_at, merchant_id)
    WHERE enriched_at IS NULL AND rail = 'nmi' AND transaction_id IS NOT NULL;
