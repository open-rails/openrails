-- #843: org <-> merchant is deliberately 1:1 (#527). permission_group_id is
-- "used to resolve a merchant from its authenticated group id" but carried a
-- NON-unique index, so two merchants could claim one AuthKit group and
-- resolution became ambiguous — the resolved merchant decides whose books a
-- request writes to, so this is authorization-adjacent, not untidy data.
-- Partial on live merchants, mirroring uq_merchants_api_host.
--
-- Audit before applying:
--   SELECT permission_group_id, count(*) FROM openrails.merchants
--    WHERE permission_group_id IS NOT NULL AND deleted_at IS NULL
--    GROUP BY 1 HAVING count(*) > 1;

DROP INDEX openrails.idx_merchants_permission_group_id;

CREATE UNIQUE INDEX uq_merchants_permission_group_id
    ON openrails.merchants USING btree (permission_group_id)
    WHERE ((permission_group_id IS NOT NULL) AND (deleted_at IS NULL));
