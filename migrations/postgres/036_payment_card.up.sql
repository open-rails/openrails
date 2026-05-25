-- Per-payment card snapshot. Stripe's invoice.paid payload carries only the
-- charge/payment_intent id, so the card brand/last4 are captured from the
-- charge.succeeded / payment_method.attached webhooks (which embed
-- payment_method_details.card) and snapshotted here, keyed by charge id.
-- This keeps payment history self-contained (the card used per charge) without
-- ever fetching from Stripe at query time.
ALTER TABLE billing.payments
    ADD COLUMN IF NOT EXISTS card_brand TEXT,
    ADD COLUMN IF NOT EXISTS card_last4 TEXT;
