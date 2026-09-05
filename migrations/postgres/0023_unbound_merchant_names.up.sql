SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '60s';

-- AuthKit owns bound names. A cached projection must not reserve a name after
-- AuthKit releases it, including while the former merchant remains active.
DROP INDEX openrails.uq_merchants_slug;
CREATE UNIQUE INDEX uq_merchants_unbound_slug ON openrails.merchants(slug)
 WHERE deleted_at IS NULL AND permission_group_id IS NULL;
