-- #781: server-side notice-window enforcement for scheduled price INCREASES.
-- The merchant-configurable window itself (default 30 days) lives in the
-- existing general merchant-config JSONB row (#499,
-- openrails.merchant_configurations.config.reprice_notice_window_days) — no
-- new column needed there.
--
-- acknowledged_short_notice is the audit trail for the one escape hatch
-- (#781's acknowledge_short_notice request flag): true when a reprice whose
-- effective_at fell inside the merchant's configured notice window (an
-- INCREASE only — decreases are exempt) was scheduled anyway via an explicit
-- override. Never set silently — the service also emits a warn-level log line
-- whenever it is used.
ALTER TABLE openrails.subscription_reprices
    ADD COLUMN acknowledged_short_notice boolean DEFAULT false NOT NULL;

COMMENT ON COLUMN openrails.subscription_reprices.acknowledged_short_notice IS '#781: true when this INCREASE reprice''s effective_at was inside the merchant''s configured notice window and was scheduled anyway via the explicit acknowledge_short_notice override on the request — the audit record for the support/emergency bypass path.';
