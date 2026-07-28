DROP INDEX openrails.uq_merchants_permission_group_id;

CREATE INDEX idx_merchants_permission_group_id
    ON openrails.merchants USING btree (permission_group_id) WHERE (permission_group_id IS NOT NULL);
