-- parent: 13 sha256:18afceee7ee689bbc33dfdf23b9072976b40b57c54fabc1bb4cd5617cb6e060c
-- #1094: unverified rows are read from the provider at once (#1089 §12).
-- subscription_verifications tracks every unverified row (since when, how
-- often read), maintained at commit by a deferred trigger that also wakes the
-- in-process resolver with a per-schema NOTIFY. Reads are non-destructive, so
-- this is no queue: a missed notification is re-detected by the next pass.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

CREATE TABLE openrails.subscription_verifications (
    merchant_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    since timestamp with time zone NOT NULL,
    reads integer DEFAULT 0 NOT NULL,
    last_read_at timestamp with time zone,
    last_error text,
    CONSTRAINT subscription_verifications_pkey PRIMARY KEY (merchant_id, subscription_id),
    CONSTRAINT subscription_verifications_subscription_fk FOREIGN KEY (merchant_id, subscription_id)
        REFERENCES openrails.subscriptions(merchant_id, id) ON DELETE CASCADE
);

COMMENT ON TABLE openrails.subscription_verifications IS '#1094: one row per unverified subscription, kept by trg_subscriptions_track_unverified at commit. since dates entry (the row''s updated_at); reads/last_read_at record provider reads. Feeds life.unverified.backlog and the unresolved escalation.';

CREATE INDEX idx_subscription_verifications_since ON openrails.subscription_verifications USING btree (merchant_id, since);

-- Deferred to commit so a row that enters and leaves unverified in one
-- transaction (an import resolving what it seeded) is neither tracked nor read.
CREATE FUNCTION openrails.subscriptions_track_unverified() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    current_status openrails.subscription_status;
    entered_at timestamp with time zone;
BEGIN
    IF openrails.billing_restore_active(NEW.merchant_id) THEN RETURN NULL; END IF;
    SELECT s.status, s.updated_at INTO current_status, entered_at
      FROM openrails.subscriptions s
     WHERE s.merchant_id = NEW.merchant_id AND s.id = NEW.id AND s.deleted_at IS NULL;
    IF current_status = 'unverified' THEN
        INSERT INTO openrails.subscription_verifications (merchant_id, subscription_id, since)
        VALUES (NEW.merchant_id, NEW.id, entered_at)
        ON CONFLICT (merchant_id, subscription_id) DO NOTHING;
        IF FOUND THEN
            PERFORM pg_notify('openrails_unverified:' || TG_TABLE_SCHEMA, NEW.merchant_id::text || ':' || NEW.id::text);
        END IF;
    ELSE
        DELETE FROM openrails.subscription_verifications WHERE merchant_id = NEW.merchant_id AND subscription_id = NEW.id;
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_subscriptions_track_unverified AFTER INSERT OR UPDATE OF status ON openrails.subscriptions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION openrails.subscriptions_track_unverified();

INSERT INTO openrails.subscription_verifications (merchant_id, subscription_id, since)
SELECT merchant_id, id, updated_at FROM openrails.subscriptions WHERE status = 'unverified' AND deleted_at IS NULL;

-- A bulk verification read (roster + transactions by date range) resumes from
-- its last completed transaction page after a crash.
CREATE TABLE openrails.nmi_bulk_checkpoints (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    since timestamp with time zone NOT NULL,
    until timestamp with time zone NOT NULL,
    next_page integer DEFAULT 1 NOT NULL,
    started_at timestamp with time zone NOT NULL,
    CONSTRAINT nmi_bulk_checkpoints_pkey PRIMARY KEY (merchant_id, psp_id),
    CONSTRAINT nmi_bulk_checkpoints_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES openrails.psps(merchant_id, id) ON DELETE CASCADE
);

COMMENT ON TABLE openrails.nmi_bulk_checkpoints IS '#1094: the in-progress bulk verification read per NMI account: its transaction window and the next page to read. Deleted when the pass completes.';
