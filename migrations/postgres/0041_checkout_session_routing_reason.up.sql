-- or#288: record WHY a checkout session landed on the PSP it landed on.
--
-- Until now the row stated the outcome (rail, psp_id) and nothing about the
-- decision, so "why did this customer get CCBill instead of Stripe" was only
-- answerable by re-deriving arming, credentials and price links as they were at
-- the time — which is to say, not answerable at all once anything moved.
--
-- One compact jsonb column holds the whole trace: which policy and rule
-- matched, the winner, the still-eligible fallbacks, and every candidate passed
-- over with its skip class. Written once at creation and never rewritten
-- (UpdateCheckoutSession does not name it), so the row keeps the decision as it
-- was made.
--
-- Nullable rather than defaulted: sessions created before this column existed
-- had no trace, and inventing an empty one would assert a decision nobody
-- recorded (#651).

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE openrails.checkout_sessions ADD COLUMN IF NOT EXISTS routing_reason jsonb;

COMMENT ON COLUMN openrails.checkout_sessions.routing_reason IS 'or#288 processor-routing decision trace, written once at creation: {policy: explicit|merchant|default, rule: matched merchant-rule index, selected: PSP key, rail, fallbacks: [remaining eligible PSP keys, ranked], skipped: [{selector, reason}]}. Skip reasons are PRE-CHARGE availability classes (not_armed, credentials_missing, link_missing, mode_unsupported, service_unavailable, ambiguous_selector, unknown_selector, resolve_failed); a decline is never one of them. NULL = created before the column existed.';
