ALTER TABLE openrails.reprice_batches
    DROP CONSTRAINT reprice_batches_source_price_fk,
    DROP CONSTRAINT reprice_batches_fallback_chk,
    DROP CONSTRAINT reprice_batches_kind_chk,
    DROP COLUMN subscriptions_blocked,
    DROP COLUMN fallback_policy,
    DROP COLUMN source_price_id,
    DROP COLUMN kind;

ALTER TABLE openrails.subscription_reprices
    DROP CONSTRAINT subscription_reprices_blocked_has_reason;

DELETE FROM openrails.subscription_reprices WHERE status = 'blocked';

ALTER TABLE openrails.subscription_reprices
    DROP CONSTRAINT subscription_reprices_status_chk;
ALTER TABLE openrails.subscription_reprices
    ADD CONSTRAINT subscription_reprices_status_chk
        CHECK (status = ANY (ARRAY['scheduled'::text, 'applied'::text, 'canceled'::text]));

ALTER TABLE openrails.subscription_reprices
    DROP CONSTRAINT subscription_reprices_kind_chk,
    DROP COLUMN blocked_reason,
    DROP COLUMN kind;
