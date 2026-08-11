-- or#911: a spend-delegation grant carries an opaque caller-supplied
-- provenance reference (e.g. the digest of the signed document that authorized
-- it), returned on reads. Hosts kept a mirror table solely to serve "which
-- document authorized this grant" — the grant itself is the right home.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE openrails.invoker_spend_limits
    ADD COLUMN provenance text DEFAULT ''::text NOT NULL;

COMMENT ON COLUMN openrails.invoker_spend_limits.provenance IS 'Opaque caller-supplied provenance reference (or#911), e.g. a signed-document digest. Stored verbatim, returned on reads; never interpreted.';
