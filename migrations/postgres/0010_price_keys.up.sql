-- #774: price keys — a durable, merchant-unique handle naming a price's
-- substance-version chain. Row identity STAYS the #662 deterministic
-- substance UUID; `key` is a movable pointer to whichever row is CURRENT for
-- that chain. A version bump (same key, new substance) creates/reactivates a
-- row, re-points the key, and archives the displaced row — so at most one
-- NON-ARCHIVED row may hold a given (merchant, key) at a time. Archived
-- predecessor rows keep their key as a back-reference to the chain they
-- belonged to (never nulled out, never reused for something else).
ALTER TABLE openrails.prices
    ADD COLUMN key text;

COMMENT ON COLUMN openrails.prices.key IS '#774: durable per-merchant-unique handle for this price''s substance-version chain. Immutable identity-wise (the row''s id is still the #662 substance UUID) but the LABEL can be relabeled in place (a key rename). At most one non-archived row per (merchant_id, key) — see uq_prices_merchant_key_current. Archived rows keep their key as a back-reference to the chain.';

-- Backfill: derive key = "<product-key>-<interval>" for every existing row.
-- Grouping is (merchant, product, interval) so a pre-#774 version chain
-- (one active row + N archived predecessors from ordinary reprice-and-archive
-- edits) collapses onto ONE shared key, matching "archived predecessors keep a
-- back-reference to the chain". The only case that needs disambiguation is
-- TWO OR MORE rows that are simultaneously ACTIVE at the same interval (a real
-- promo/standard split) — those get "-alt2", "-alt3", ... suffixes so the
-- partial unique index below never rejects the backfill. This mirrors
-- pkg/service.PriceIntervalLabel exactly; keep the two in sync.
WITH labeled AS (
    SELECT
        p.id,
        p.merchant_id,
        p.product_id,
        p.archived,
        p.created_at,
        pr.key AS product_key,
        CASE
            WHEN NOT p.auto_renew OR p.access_duration_hours IS NULL THEN 'onetime'
            WHEN p.access_duration_hours = 168 THEN 'weekly'
            WHEN p.access_duration_hours IN (720, 744) THEN 'monthly'
            WHEN p.access_duration_hours IN (2160, 2184) THEN 'quarterly'
            WHEN p.access_duration_hours IN (8760, 8784) THEN 'yearly'
            ELSE (p.access_duration_hours / 24)::text || 'd'
        END AS interval_label
    FROM openrails.prices p
    JOIN openrails.products pr ON pr.id = p.product_id
),
active_rank AS (
    SELECT id,
           row_number() OVER (
               PARTITION BY merchant_id, product_id, interval_label
               ORDER BY created_at ASC, id ASC
           ) AS active_rn
    FROM labeled
    WHERE NOT archived
)
UPDATE openrails.prices p
SET key = l.product_key || '-' || l.interval_label ||
    (CASE WHEN ar.active_rn IS NOT NULL AND ar.active_rn > 1 THEN '-alt' || ar.active_rn::text ELSE '' END)
FROM labeled l
LEFT JOIN active_rank ar ON ar.id = l.id
WHERE p.id = l.id;

ALTER TABLE openrails.prices
    ALTER COLUMN key SET NOT NULL;

-- Safety net for direct-SQL inserts (test fixtures, ad-hoc scripts, anything
-- outside the Go CreatePrice path): auto-derive the SAME default the app
-- layer would compute when key is omitted, so the NOT NULL constraint above
-- can never surprise a caller that has no reason to know about #774 keys.
-- The Go layer (pkg/service.CreatePrice) ALWAYS resolves and supplies an
-- explicit key itself (auto-defaulted or caller-given) — this trigger only
-- ever fires for callers that bypass it entirely. It does NOT perform the
-- archive-displaced/repoint dance or write a pointer-movement log entry;
-- those are business logic that belongs in Go, not a trigger. Mirrors
-- pkg/service.PriceIntervalLabel and the backfill CASE above; keep all three
-- in sync.
CREATE FUNCTION openrails.prices_default_key() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    product_key text;
    interval_label text;
BEGIN
    IF NEW.key IS NOT NULL AND btrim(NEW.key) <> '' THEN
        RETURN NEW;
    END IF;
    SELECT key INTO product_key FROM openrails.products WHERE id = NEW.product_id;
    IF product_key IS NULL THEN
        RAISE EXCEPTION 'prices_default_key: product % not found for price %', NEW.product_id, NEW.id;
    END IF;
    IF NOT NEW.auto_renew OR NEW.access_duration_hours IS NULL THEN
        interval_label := 'onetime';
    ELSIF NEW.access_duration_hours = 168 THEN
        interval_label := 'weekly';
    ELSIF NEW.access_duration_hours IN (720, 744) THEN
        interval_label := 'monthly';
    ELSIF NEW.access_duration_hours IN (2160, 2184) THEN
        interval_label := 'quarterly';
    ELSIF NEW.access_duration_hours IN (8760, 8784) THEN
        interval_label := 'yearly';
    ELSE
        interval_label := (NEW.access_duration_hours / 24)::text || 'd';
    END IF;
    NEW.key := product_key || '-' || interval_label;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_prices_default_key
    BEFORE INSERT ON openrails.prices
    FOR EACH ROW EXECUTE FUNCTION openrails.prices_default_key();

-- At most one CURRENT (non-archived) row may hold a given key per merchant —
-- the DB-enforced half of "keys are unique among non-archived pointer
-- targets". Archived rows may freely share a key (that's the version chain).
CREATE UNIQUE INDEX uq_prices_merchant_key_current ON openrails.prices USING btree (merchant_id, key) WHERE (NOT archived);

-- Lookup by key across the whole chain (archived included), e.g. "all prior
-- versions of key K".
CREATE INDEX idx_prices_merchant_key ON openrails.prices USING btree (merchant_id, key);

-- #774: the pointer-movement history log — (key, price_row, effective_at).
-- Answers "what did key K sell on date D" and, together with prices.archived,
-- defines #773's "all prior versions of key K". NOT a linked list on rows: a
-- reactivated row (flip-flop) gets a SECOND movement entry rather than a
-- second row, so the log — not row back-references — is the source of
-- history. Backfilled below with one entry per existing row (effective_at =
-- the row's created_at) so history is complete immediately after migration.
CREATE TABLE openrails.price_key_movements (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    key text NOT NULL,
    price_id uuid NOT NULL,
    effective_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

ALTER TABLE ONLY openrails.price_key_movements FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.price_key_movements IS '#774: append-only log of when a price key''s current pointer moved to which price row. History, not row identity — a row can appear more than once (reactivation).';

ALTER TABLE ONLY openrails.price_key_movements
    ADD CONSTRAINT price_key_movements_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.price_key_movements
    ADD CONSTRAINT price_key_movements_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.price_key_movements
    ADD CONSTRAINT price_key_movements_price_fk FOREIGN KEY (price_id) REFERENCES openrails.prices(id) ON DELETE RESTRICT;

CREATE INDEX idx_price_key_movements_key ON openrails.price_key_movements USING btree (merchant_id, key, effective_at DESC);

CREATE INDEX idx_price_key_movements_price ON openrails.price_key_movements USING btree (price_id);

ALTER TABLE openrails.price_key_movements ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.price_key_movements USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.price_key_movements TO openrails_app;

INSERT INTO openrails.price_key_movements (merchant_id, key, price_id, effective_at, created_at)
SELECT merchant_id, key, id, created_at, created_at
FROM openrails.prices;
