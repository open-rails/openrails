-- Known-through for what imports brought before they recorded it: each PSP
-- holding imported data with no recorded read or import is dated from what
-- is stored, and imported_from says which rule dated it.

ALTER TABLE billing.psp_freshness ADD COLUMN imported_from text CHECK (imported_from IN ('as_of', 'latest_record'));
COMMENT ON COLUMN billing.psp_freshness.imported_from IS 'How imported_through was found: as_of, an import''s own evidence instant; latest_record, for an import that recorded none, the latest instant the provider''s own records it brought carry (subscription and period starts, payments): complete through at least it.';
UPDATE billing.psp_freshness SET imported_from = 'as_of' WHERE imported_through IS NOT NULL;
ALTER TABLE billing.psp_freshness ADD CONSTRAINT psp_freshness_imported_check CHECK ((imported_through IS NULL) = (imported_from IS NULL));
COMMENT ON COLUMN billing.psp_freshness.imported_through IS 'The evidence instant of the latest import naming the PSP; imported_from says how it was found.';

-- An import's as_of survives where it stamped it: on a host book's accepted
-- schedules and recurring mandates, and as an applied PSP import's pull. Else
-- the latest instant the provider's own records carry: its host-landed
-- subscriptions' starts and their payments. A cancel or an end the import may
-- have stamped at its own run time is not one. A PSP so dated is stale from
-- it until a catch-up reads the gap.
WITH host AS (
    SELECT s.merchant_id, s.psp_id, s.id, s.started_at, s.current_period_starts_at
      FROM billing.subscriptions s
     WHERE s.facts_authority = 'host' AND s.deleted_at IS NULL
), stated AS (
    SELECT h.merchant_id, h.psp_id, ps.created_at AS at
      FROM host h JOIN billing.provider_schedules ps ON ps.merchant_id = h.merchant_id AND ps.subscription_id = h.id AND ps.psp_id = h.psp_id
    UNION ALL
    SELECT h.merchant_id, h.psp_id, m.accepted_at
      FROM host h JOIN billing.mandates m ON m.merchant_id = h.merchant_id AND m.subscription_id = h.id AND m.psp_id = h.psp_id
    UNION ALL
    SELECT i.merchant_id, i.psp_id, i.pulled_at
      FROM billing.psp_imports i WHERE i.status = 'applied' AND i.pulled_at IS NOT NULL
), recorded AS (
    SELECT h.merchant_id, h.psp_id, greatest(h.started_at, h.current_period_starts_at) AS at FROM host h
    UNION ALL
    SELECT h.merchant_id, h.psp_id, y.purchased_at
      FROM host h JOIN billing.payments y ON y.merchant_id = h.merchant_id AND y.subscription_id = h.id AND y.psp_id = h.psp_id
     WHERE y.deleted_at IS NULL
), dated AS (
    SELECT p.merchant_id, p.id AS psp_id, st.at AS stated, r.at AS recorded
      FROM billing.psps p
      LEFT JOIN (SELECT merchant_id, psp_id, max(at) AS at FROM stated GROUP BY merchant_id, psp_id) st
        ON st.merchant_id = p.merchant_id AND st.psp_id = p.id
      LEFT JOIN (SELECT merchant_id, psp_id, max(at) AS at FROM recorded GROUP BY merchant_id, psp_id) r
        ON r.merchant_id = p.merchant_id AND r.psp_id = p.id
     WHERE NOT EXISTS (SELECT 1 FROM billing.psp_freshness f
                        WHERE f.merchant_id = p.merchant_id AND f.psp_id = p.id
                          AND (f.read_at IS NOT NULL OR f.imported_through IS NOT NULL))
)
INSERT INTO billing.psp_freshness (merchant_id, psp_id, imported_through, imported_from, stale_since)
SELECT d.merchant_id, d.psp_id, greatest(d.stated, d.recorded),
       CASE WHEN d.stated >= d.recorded OR d.recorded IS NULL THEN 'as_of' ELSE 'latest_record' END,
       greatest(d.stated, d.recorded)
  FROM dated d
 WHERE d.stated IS NOT NULL OR d.recorded IS NOT NULL
ON CONFLICT (merchant_id, psp_id) DO UPDATE SET
    imported_through = EXCLUDED.imported_through, imported_from = EXCLUDED.imported_from,
    stale_since = COALESCE(psp_freshness.stale_since, EXCLUDED.stale_since), updated_at = now();
