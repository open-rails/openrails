-- Repair: none-needed A migration's provider push is a price_migration_push
-- operation; a move that no longer applies is skipped, and blocked and skipped
-- are terminal.
ALTER TABLE billing.scheduled_changes DROP CONSTRAINT scheduled_changes_status_check;
ALTER TABLE billing.scheduled_changes ADD CONSTRAINT scheduled_changes_status_check
    CHECK (status = ANY (ARRAY['scheduled'::text, 'applied'::text, 'canceled'::text, 'blocked'::text, 'skipped'::text]));
ALTER TABLE billing.scheduled_changes DROP CONSTRAINT scheduled_changes_blocked_check;
ALTER TABLE billing.scheduled_changes ADD CONSTRAINT scheduled_changes_blocked_check
    CHECK (((status = ANY (ARRAY['blocked'::text, 'skipped'::text])) = (blocked_reason IS NOT NULL))
           AND ((status <> ALL (ARRAY['blocked'::text, 'skipped'::text])) OR (source = 'migration'::text))
           AND (blocked_reason <> ''::text));
DROP INDEX billing.scheduled_changes_redrive_idx;
COMMENT ON TABLE billing.scheduled_changes IS 'A subscription''s change waiting for its renewal: the price (price_id) and, for a per-seat price, the seats (quantity; NULL keeps the subscription''s) it bills from its first renewal on or after effective_at. At most one is scheduled per subscription. source is change (a tier or seat change) or migration (price_migration_id). A migration''s move its provider cannot take is blocked, one that no longer applies is skipped; both are terminal and blocked_reason says why. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.scheduled_changes.blocked_reason IS 'Why a blocked or skipped move did not happen; NULL otherwise.';
COMMENT ON TABLE billing.price_migrations IS 'One move of subscribers from a price (from_price_id), or from every version of a price key but the target (from_product_id, from_price_key), to to_price_id at each subscription''s first renewal on or after effective_at. Matched and skipped are facts of creation (a skipped subscription gets no scheduled change); progress is counted from the scheduled_changes that carry price_migration_id. Its command settles when its last move resolves. Retention: permanent, never pruned.';
