-- parent: 19 sha256:ccbd9723de6431d71dba2d6937cb8e6ac87141ded6a5ac6d62deacbb6622333d
-- #1102: every subscription update advances row_version, so a full-row write
-- from a stale image fails instead of reverting any other writer's change
-- (retry schedule, payment method, price), raw SQL included.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

ALTER TABLE openrails.subscriptions ADD COLUMN row_version bigint DEFAULT 0 NOT NULL;

CREATE FUNCTION openrails.subscriptions_row_version() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    NEW.row_version := OLD.row_version + 1;
    RETURN NEW;
END;
$$;

CREATE TRIGGER subscriptions_row_version BEFORE UPDATE ON openrails.subscriptions
    FOR EACH ROW EXECUTE FUNCTION openrails.subscriptions_row_version();
