-- Repair: none-needed. Existing immutable facts and destination inboxes remain
-- unchanged. Only new explicit account-inbox publications allocate revisions.
CREATE TABLE billing.notification_issue_revisions (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    issue_key text NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    withdrawn boolean NOT NULL,
    source_event uuid,
    PRIMARY KEY (merchant_id, customer_id, issue_key),
    FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE
);
COMMENT ON TABLE billing.notification_issue_revisions IS 'Producer ordering for exact account-notification identities. No inbox payload, read state, event cursor, or delivery queue. Retained until customer deletion so delayed publication cannot revive resolved issues.';

CREATE TABLE billing.notification_channel_state (
    merchant_id uuid PRIMARY KEY REFERENCES billing.merchants(id) ON DELETE CASCADE,
    restore_required boolean NOT NULL DEFAULT false
);
COMMENT ON TABLE billing.notification_channel_state IS 'Financial archive restore blocks inbox publication until explicit notification ordering-state adoption. No implicit revision reset.';
