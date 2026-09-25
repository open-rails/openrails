-- parent: 12 sha256:57dc21d70c831a972f15f679ec9ab32315454c15e31b7b75d7aaf77883fe6ad3
-- #1093: explicit processor try-again answers get a short bounded ladder that
-- is not counted as a dunning failure. The count resets with the dunning case.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

ALTER TABLE openrails.subscriptions ADD COLUMN transient_retries integer DEFAULT 0 NOT NULL,
    ADD CONSTRAINT chk_subscriptions_transient_retries CHECK (transient_retries >= 0);
